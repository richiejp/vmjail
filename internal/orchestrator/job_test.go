package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const orchestratorTestJobID = "job"

func TestNodeJobControlRoundTripAndNativeProjection(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{1}
	events := h.attachStream(node)
	wantTime := time.Now().UTC().Truncate(time.Second)

	result := make(chan nodejob.Snapshot, 1)
	errCh := make(chan error, 1)
	go func() {
		s, err := h.StartNodeJob(t.Context(), node, nodejob.StartRequest{
			IdempotencyKey: "run-1", SandboxUID: "sb", Command: []string{"agent", "run"},
		})
		result <- s
		errCh <- err
	}()
	event := <-events
	req := event.GetJobControl()
	if req.GetRequestId() == "" || req.GetStart().GetIdempotencyKey() != "run-1" || req.GetStart().GetSandboxUid() != "sb" {
		t.Fatalf("job event = %+v", req)
	}
	exit := int32(3)
	callback := connect.NewRequest(&nodectrlv1.JobControlResponse{
		RequestId: req.GetRequestId(),
		Snapshot: &nodectrlv1.JobSnapshot{
			JobId: "job-1", SandboxUid: "sb", State: nodectrlv1.JobState_JOB_STATE_FAILED,
			Stdout: &nodectrlv1.JobOutputChunk{Data: []byte("out"), NextCursor: 4, CursorExpired: true},
			Stderr: &nodectrlv1.JobOutputChunk{Data: []byte("err"), NextCursor: 5}, ExitCode: &exit,
			Reason: "exit", CreatedAt: timestamppb.New(wantTime), CancelCalled: true,
		},
	})
	callback.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportJobControl(t.Context(), callback); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.ID != "job-1" || got.State != nodejob.StateFailed || string(got.Stdout.Data) != "out" ||
		!got.Stdout.CursorExpired || got.ExitCode != 3 || !got.ExitCodeSet || got.CreatedAt != wantTime || !got.CancelCalled {
		t.Fatalf("snapshot = %+v", got)
	}
	if _, err := h.ReportJobControl(t.Context(), callback); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("duplicate callback code = %s, err=%v", connect.CodeOf(err), err)
	}
}

func TestNodeJobControlRejectsWrongNodeThenAcceptsTarget(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{2}
	events := h.attachStream(node)
	done := make(chan error, 1)
	go func() {
		_, err := h.GetNodeJob(t.Context(), node, orchestratorTestJobID, 1, 2, 3)
		done <- err
	}()
	req := (<-events).GetJobControl()
	if req.GetGet().GetStdoutCursor() != 1 || req.GetGet().GetStderrCursor() != 2 || req.GetGet().GetMaxBytesPerStream() != 3 {
		t.Fatalf("Get request = %+v", req.GetGet())
	}
	resp := connect.NewRequest(&nodectrlv1.JobControlResponse{RequestId: req.GetRequestId(), Snapshot: &nodectrlv1.JobSnapshot{JobId: orchestratorTestJobID}})
	resp.Header().Set(peerFingerprintHeader, (identity.Fingerprint{3}).String())
	if _, err := h.ReportJobControl(t.Context(), resp); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("wrong-node code = %s, err=%v", connect.CodeOf(err), err)
	}
	resp.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportJobControl(t.Context(), resp); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNodeJobControlCancelDeleteAndStructuredError(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{4}
	events := h.attachStream(node)

	cancelDone := make(chan error, 1)
	go func() {
		_, err := h.CancelNodeJob(t.Context(), node, orchestratorTestJobID)
		cancelDone <- err
	}()
	cancelReq := (<-events).GetJobControl()
	if cancelReq.GetCancel().GetJobId() != orchestratorTestJobID {
		t.Fatalf("Cancel request = %+v", cancelReq)
	}
	reportJobResponse(t, h, node, &nodectrlv1.JobControlResponse{
		RequestId: cancelReq.GetRequestId(), ErrorCode: nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_FOUND, Error: "gone",
	})
	if err := <-cancelDone; !errors.Is(err, nodejob.ErrNotFound) {
		t.Fatalf("Cancel error = %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- h.DeleteNodeJob(t.Context(), node, orchestratorTestJobID) }()
	deleteReq := (<-events).GetJobControl()
	if deleteReq.GetDelete().GetJobId() != orchestratorTestJobID {
		t.Fatalf("Delete request = %+v", deleteReq)
	}
	reportJobResponse(t, h, node, &nodectrlv1.JobControlResponse{RequestId: deleteReq.GetRequestId()})
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
}

func TestNodeJobControlDisconnectAndTimeoutCleanup(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{5}
	registration := h.attachRegistration(node)
	done := make(chan error, 1)
	go func() {
		_, err := h.GetNodeJob(context.Background(), node, orchestratorTestJobID, 0, 0, 1)
		done <- err
	}()
	<-registration.events
	h.detachRegistration(node, registration)
	if err := <-done; !errors.Is(err, ErrNodeNotConnected) {
		t.Fatalf("disconnect error = %v", err)
	}

	h.attachStream(node)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := h.GetNodeJob(ctx, node, orchestratorTestJobID, 0, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	h.mu.Lock()
	pending := len(h.pendingJobs)
	h.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending jobs after timeout = %d", pending)
	}
}

func TestReportJobControlValidationAndPublicInputGuards(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{6}
	if _, err := h.ReportJobControl(t.Context(), connect.NewRequest(&nodectrlv1.JobControlResponse{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing auth code = %s", connect.CodeOf(err))
	}
	req := connect.NewRequest(&nodectrlv1.JobControlResponse{})
	req.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportJobControl(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty ID code = %s", connect.CodeOf(err))
	}
	if err := validateJobControlResponse(&nodectrlv1.JobControlResponse{Error: strings.Repeat("x", nodejob.MaxTerminalReasonLen+1)}); err == nil {
		t.Fatal("oversized error accepted")
	}
	if err := validateJobControlResponse(&nodectrlv1.JobControlResponse{Snapshot: &nodectrlv1.JobSnapshot{
		Stdout: &nodectrlv1.JobOutputChunk{Data: make([]byte, nodejob.DefaultMaxReadBytes+1)},
	}}); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
	if _, err := h.StartNodeJob(t.Context(), node, nodejob.StartRequest{}); err == nil {
		t.Fatal("empty idempotency key accepted")
	}
	if _, err := h.GetNodeJob(t.Context(), node, "", 0, 0, 0); err == nil {
		t.Fatal("empty Get ID accepted")
	}
	if _, err := h.CancelNodeJob(t.Context(), node, ""); err == nil {
		t.Fatal("empty Cancel ID accepted")
	}
	if err := h.DeleteNodeJob(t.Context(), node, ""); err == nil {
		t.Fatal("empty Delete ID accepted")
	}
}

func TestNodeJobResponseMappings(t *testing.T) {
	errorCodes := map[nodectrlv1.JobControlErrorCode]error{
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_FOUND:            nodejob.ErrNotFound,
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_CAPACITY:             nodejob.ErrCapacity,
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_CURSOR:       nodejob.ErrInvalidCursor,
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_TERMINAL:         nodejob.ErrNotTerminal,
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_IDEMPOTENCY_CONFLICT: nodejob.ErrIdempotencyConflict,
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_ARGUMENT:     errors.New("generic"),
		nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INTERNAL:             errors.New("generic"),
	}
	for code, want := range errorCodes {
		err := jobResponseError(&nodectrlv1.JobControlResponse{ErrorCode: code, Error: "detail"})
		if want.Error() == "generic" {
			if err == nil || !strings.Contains(err.Error(), "detail") {
				t.Errorf("code %s error = %v", code, err)
			}
		} else if !errors.Is(err, want) {
			t.Errorf("code %s error = %v, want %v", code, err, want)
		}
	}
	if err := jobResponseError(&nodectrlv1.JobControlResponse{}); err != nil {
		t.Fatalf("success response error = %v", err)
	}
	if _, err := jobResponseSnapshot(&nodectrlv1.JobControlResponse{}); err == nil {
		t.Fatal("missing snapshot accepted")
	}

	states := map[nodectrlv1.JobState]nodejob.State{
		nodectrlv1.JobState_JOB_STATE_STARTING:    nodejob.StateStarting,
		nodectrlv1.JobState_JOB_STATE_RUNNING:     nodejob.StateRunning,
		nodectrlv1.JobState_JOB_STATE_CANCELING:   nodejob.StateCanceling,
		nodectrlv1.JobState_JOB_STATE_SUCCEEDED:   nodejob.StateSucceeded,
		nodectrlv1.JobState_JOB_STATE_FAILED:      nodejob.StateFailed,
		nodectrlv1.JobState_JOB_STATE_CANCELED:    nodejob.StateCanceled,
		nodectrlv1.JobState_JOB_STATE_UNSPECIFIED: "",
	}
	for in, want := range states {
		if got := jobStateFromProto(in); got != want {
			t.Errorf("state %s = %q, want %q", in, got, want)
		}
	}
}

func TestPushJobControlConnectionAndBacklogErrors(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{7}
	req := &nodectrlv1.JobControlRequest{RequestId: "request"}
	if err := h.pushJobControl(node, req); !errors.Is(err, ErrNodeNotConnected) {
		t.Fatalf("disconnected push = %v", err)
	}
	events := h.attachStream(node)
	for range sandboxEventBacklog {
		events <- &nodectrlv1.NodeEvent{}
	}
	if err := h.pushJobControl(node, req); !errors.Is(err, ErrSandboxEventBacklog) {
		t.Fatalf("backlogged push = %v", err)
	}
}

func reportJobResponse(t *testing.T, h *NodeControlHandler, node identity.Fingerprint, response *nodectrlv1.JobControlResponse) {
	t.Helper()
	req := connect.NewRequest(response)
	req.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportJobControl(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}
