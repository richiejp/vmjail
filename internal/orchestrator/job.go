package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const DefaultJobControlTimeout = 10 * time.Second

var ErrUnknownJobControlRequest = errors.New("orchestrator: unknown job control request")

type pendingJobControl struct {
	node    identity.Fingerprint
	arrived chan jobControlArrival
}

type jobControlArrival struct {
	response *nodectrlv1.JobControlResponse
	err      error
}

func (h *NodeControlHandler) StartNodeJob(ctx context.Context, fp identity.Fingerprint, req nodejob.StartRequest) (nodejob.Snapshot, error) {
	if req.IdempotencyKey == "" {
		return nodejob.Snapshot{}, errors.New("orchestrator: StartNodeJob: empty idempotency key")
	}
	resp, err := h.controlNodeJob(ctx, fp, &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Start{
		Start: &nodectrlv1.JobStartRequest{IdempotencyKey: req.IdempotencyKey, SandboxUid: req.SandboxUID, Command: req.Command},
	}})
	if err != nil {
		return nodejob.Snapshot{}, err
	}
	return jobResponseSnapshot(resp)
}

func (h *NodeControlHandler) GetNodeJob(ctx context.Context, fp identity.Fingerprint, id string, stdoutCursor, stderrCursor uint64, maxBytes int) (nodejob.Snapshot, error) {
	if id == "" {
		return nodejob.Snapshot{}, errors.New("orchestrator: GetNodeJob: empty job ID")
	}
	maxRead := uint32(nodejob.DefaultMaxReadBytes)
	if maxBytes > 0 && maxBytes < nodejob.DefaultMaxReadBytes {
		maxRead = uint32(maxBytes)
	}
	resp, err := h.controlNodeJob(ctx, fp, &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Get{
		Get: &nodectrlv1.JobGetRequest{JobId: id, StdoutCursor: stdoutCursor, StderrCursor: stderrCursor, MaxBytesPerStream: maxRead},
	}})
	if err != nil {
		return nodejob.Snapshot{}, err
	}
	return jobResponseSnapshot(resp)
}

func (h *NodeControlHandler) CancelNodeJob(ctx context.Context, fp identity.Fingerprint, id string) (nodejob.Snapshot, error) {
	if id == "" {
		return nodejob.Snapshot{}, errors.New("orchestrator: CancelNodeJob: empty job ID")
	}
	resp, err := h.controlNodeJob(ctx, fp, &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Cancel{
		Cancel: &nodectrlv1.JobCancelRequest{JobId: id},
	}})
	if err != nil {
		return nodejob.Snapshot{}, err
	}
	return jobResponseSnapshot(resp)
}

func (h *NodeControlHandler) DeleteNodeJob(ctx context.Context, fp identity.Fingerprint, id string) error {
	if id == "" {
		return errors.New("orchestrator: DeleteNodeJob: empty job ID")
	}
	resp, err := h.controlNodeJob(ctx, fp, &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Delete{
		Delete: &nodectrlv1.JobDeleteRequest{JobId: id},
	}})
	if err != nil {
		return err
	}
	return jobResponseError(resp)
}

func (h *NodeControlHandler) controlNodeJob(ctx context.Context, fp identity.Fingerprint, req *nodectrlv1.JobControlRequest) (*nodectrlv1.JobControlResponse, error) {
	requestID, err := mintSessionID()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: mint job control ID: %w", err)
	}
	req.RequestId = requestID
	pending := &pendingJobControl{node: fp, arrived: make(chan jobControlArrival, 1)}
	h.mu.Lock()
	h.pendingJobs[requestID] = pending
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.pendingJobs[requestID] == pending {
			delete(h.pendingJobs, requestID)
		}
		h.mu.Unlock()
	}()
	if err := h.pushJobControl(fp, req); err != nil {
		return nil, err
	}
	waitCtx, cancel := waitContextWithFallback(ctx, DefaultJobControlTimeout)
	defer cancel()
	select {
	case arrival := <-pending.arrived:
		return arrival.response, arrival.err
	case <-waitCtx.Done():
		return nil, fmt.Errorf("orchestrator: wait for job control response: %w", waitCtx.Err())
	}
}

func (h *NodeControlHandler) pushJobControl(fp identity.Fingerprint, req *nodectrlv1.JobControlRequest) error {
	h.mu.Lock()
	registration, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}
	event := &nodectrlv1.NodeEvent{Event: &nodectrlv1.NodeEvent_JobControl{JobControl: req}}
	select {
	case registration.events <- event:
		return nil
	default:
		return ErrSandboxEventBacklog
	}
}

func (h *NodeControlHandler) ReportJobControl(_ context.Context, req *connect.Request[nodectrlv1.JobControlResponse]) (*connect.Response[nodectrlv1.JobControlAck], error) {
	fp, err := h.requireNode(req.Header())
	if err != nil {
		return nil, err
	}
	response := req.Msg
	if response.GetRequestId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: empty job control request ID"))
	}
	if err := validateJobControlResponse(response); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	h.mu.Lock()
	pending := h.pendingJobs[response.GetRequestId()]
	if pending == nil {
		h.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, ErrUnknownJobControlRequest)
	}
	if pending.node != fp {
		h.mu.Unlock()
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("orchestrator: job response came from wrong Node"))
	}
	delete(h.pendingJobs, response.GetRequestId())
	h.mu.Unlock()
	pending.arrived <- jobControlArrival{response: response}
	return connect.NewResponse(&nodectrlv1.JobControlAck{}), nil
}

func validateJobControlResponse(resp *nodectrlv1.JobControlResponse) error {
	if len(resp.GetError()) > nodejob.MaxTerminalReasonLen {
		return errors.New("orchestrator: job control error exceeds limit")
	}
	snapshot := resp.GetSnapshot()
	if snapshot == nil {
		return nil
	}
	if len(snapshot.GetReason()) > nodejob.MaxTerminalReasonLen || len(snapshot.GetStdout().GetData()) > nodejob.DefaultMaxReadBytes || len(snapshot.GetStderr().GetData()) > nodejob.DefaultMaxReadBytes {
		return errors.New("orchestrator: job snapshot exceeds limits")
	}
	return nil
}

func (h *NodeControlHandler) failPendingJobsLocked(fp identity.Fingerprint, err error) {
	for id, pending := range h.pendingJobs {
		if pending.node == fp {
			delete(h.pendingJobs, id)
			pending.arrived <- jobControlArrival{err: err}
		}
	}
}

func jobResponseSnapshot(resp *nodectrlv1.JobControlResponse) (nodejob.Snapshot, error) {
	if err := jobResponseError(resp); err != nil {
		return nodejob.Snapshot{}, err
	}
	s := resp.GetSnapshot()
	if s == nil {
		return nodejob.Snapshot{}, errors.New("orchestrator: Node returned no job snapshot")
	}
	out := nodejob.Snapshot{
		ID: s.GetJobId(), SandboxUID: s.GetSandboxUid(), State: jobStateFromProto(s.GetState()),
		Stdout: nodejob.Chunk{Data: append([]byte(nil), s.GetStdout().GetData()...), NextCursor: s.GetStdout().GetNextCursor(), CursorExpired: s.GetStdout().GetCursorExpired()},
		Stderr: nodejob.Chunk{Data: append([]byte(nil), s.GetStderr().GetData()...), NextCursor: s.GetStderr().GetNextCursor(), CursorExpired: s.GetStderr().GetCursorExpired()},
		Reason: s.GetReason(), CreatedAt: timestampFromProto(s.GetCreatedAt()), StartedAt: timestampFromProto(s.GetStartedAt()),
		CompletedAt: timestampFromProto(s.GetCompletedAt()), CancelCalled: s.GetCancelCalled(),
	}
	if s.ExitCode != nil {
		out.ExitCode, out.ExitCodeSet = s.GetExitCode(), true
	}
	return out, nil
}

func jobResponseError(resp *nodectrlv1.JobControlResponse) error {
	if resp.GetErrorCode() == nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_UNSPECIFIED {
		return nil
	}
	base := errors.New("node job control failed")
	switch resp.GetErrorCode() {
	case nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_FOUND:
		base = nodejob.ErrNotFound
	case nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_CAPACITY:
		base = nodejob.ErrCapacity
	case nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_CURSOR:
		base = nodejob.ErrInvalidCursor
	case nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_TERMINAL:
		base = nodejob.ErrNotTerminal
	case nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_IDEMPOTENCY_CONFLICT:
		base = nodejob.ErrIdempotencyConflict
	}
	return fmt.Errorf("%w: %s", base, resp.GetError())
}

func jobStateFromProto(state nodectrlv1.JobState) nodejob.State {
	switch state {
	case nodectrlv1.JobState_JOB_STATE_STARTING:
		return nodejob.StateStarting
	case nodectrlv1.JobState_JOB_STATE_RUNNING:
		return nodejob.StateRunning
	case nodectrlv1.JobState_JOB_STATE_CANCELING:
		return nodejob.StateCanceling
	case nodectrlv1.JobState_JOB_STATE_SUCCEEDED:
		return nodejob.StateSucceeded
	case nodectrlv1.JobState_JOB_STATE_FAILED:
		return nodejob.StateFailed
	case nodectrlv1.JobState_JOB_STATE_CANCELED:
		return nodejob.StateCanceled
	default:
		return ""
	}
}

func timestampFromProto(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
