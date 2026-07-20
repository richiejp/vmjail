package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1/agentrunv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const (
	defaultAgentRunPageSize = 50
	maxAgentRunPageSize     = 200
)

type agentJobCanceler interface {
	CancelNodeJob(context.Context, identity.Fingerprint, string) (nodejob.Snapshot, error)
}

type AgentRunHandler struct {
	agentrunv1connect.UnimplementedAgentRunServiceHandler
	store    *agentruncatalog.Store
	canceler agentJobCanceler
}

func NewAgentRunHandler(store *agentruncatalog.Store, canceler agentJobCanceler) *AgentRunHandler {
	return &AgentRunHandler{store: store, canceler: canceler}
}

func (h *AgentRunHandler) owner(reqHeader interface{ Get(string) string }) (identity.Fingerprint, error) {
	if h.store == nil {
		return identity.Fingerprint{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: AgentRun persistence is not configured"))
	}
	value := reqHeader.Get(peerFingerprintHeader)
	fp, err := identity.ParseFingerprint(value)
	if err != nil {
		return identity.Fingerprint{}, connect.NewError(connect.CodeUnauthenticated, errors.New("orchestrator: no mTLS peer identity"))
	}
	return fp, nil
}

func (h *AgentRunHandler) Get(_ context.Context, req *connect.Request[agentrunv1.GetAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
	owner, err := h.owner(req.Header())
	if err != nil {
		return nil, err
	}
	run, err := h.store.Get(owner, req.Msg.GetUuid())
	if err != nil {
		return nil, agentRunError(err)
	}
	return connect.NewResponse(mapAgentRun(run)), nil
}

func (h *AgentRunHandler) List(_ context.Context, req *connect.Request[agentrunv1.ListAgentRunsRequest]) (*connect.Response[agentrunv1.ListAgentRunsResponse], error) {
	owner, err := h.owner(req.Header())
	if err != nil {
		return nil, err
	}
	pageSize, err := agentRunPageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	runs := h.store.List(owner)
	start := sort.Search(len(runs), func(i int) bool { return runs[i].UUID > req.Msg.GetPageToken() })
	end := min(start+pageSize, len(runs))
	out := &agentrunv1.ListAgentRunsResponse{Runs: make([]*agentrunv1.AgentRun, 0, end-start)}
	for i := start; i < end; i++ {
		out.Runs = append(out.Runs, mapAgentRun(runs[i]))
	}
	if end < len(runs) {
		out.NextPageToken = runs[end-1].UUID
	}
	return connect.NewResponse(out), nil
}

func (h *AgentRunHandler) Events(_ context.Context, req *connect.Request[agentrunv1.AgentRunEventsRequest]) (*connect.Response[agentrunv1.AgentRunEventsResponse], error) {
	owner, err := h.owner(req.Header())
	if err != nil {
		return nil, err
	}
	limit, err := agentRunPageSize(req.Msg.GetLimit())
	if err != nil {
		return nil, err
	}
	run, err := h.store.Get(owner, req.Msg.GetUuid())
	if err != nil {
		return nil, agentRunError(err)
	}
	events, err := h.store.Events(owner, run.UUID, req.Msg.GetAfterCursor(), limit)
	if err != nil {
		return nil, agentRunError(err)
	}
	out := &agentrunv1.AgentRunEventsResponse{Events: make([]*agentrunv1.AgentRunEvent, len(events)), NextCursor: req.Msg.GetAfterCursor(), LastEventCursor: run.LastEventCursor, Terminal: run.State.Terminal()}
	for i, event := range events {
		out.Events[i] = &agentrunv1.AgentRunEvent{Cursor: event.Cursor, Timestamp: timestamppb.New(event.Timestamp), State: mapAgentRunState(event.State), Message: event.Message}
		out.NextCursor = event.Cursor
	}
	return connect.NewResponse(out), nil
}

func (h *AgentRunHandler) Cancel(ctx context.Context, req *connect.Request[agentrunv1.CancelAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
	owner, err := h.owner(req.Header())
	if err != nil {
		return nil, err
	}
	run, err := h.store.Get(owner, req.Msg.GetUuid())
	if err != nil {
		return nil, agentRunError(err)
	}
	switch run.State {
	case agentruncatalog.StateQueued, agentruncatalog.StateStarting:
		run, err = h.store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateCanceled, Message: "run canceled"})
	case agentruncatalog.StateRunning:
		if h.canceler == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: AgentRun job cancellation is not configured"))
		}
		run, err = h.store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateRunning}, To: agentruncatalog.StateCanceling, Message: "cancellation requested"})
		if err == nil {
			_, err = h.canceler.CancelNodeJob(ctx, run.NodeFingerprint, run.NodeJobID)
		}
	case agentruncatalog.StateCanceling:
		return connect.NewResponse(mapAgentRun(run)), nil
	default:
		if run.State.Terminal() {
			return connect.NewResponse(mapAgentRun(run)), nil
		}
		err = agentruncatalog.ErrConflict
	}
	if err != nil {
		return nil, agentRunError(err)
	}
	return connect.NewResponse(mapAgentRun(run)), nil
}

func agentRunPageSize(value uint32) (int, error) {
	if value == 0 {
		return defaultAgentRunPageSize, nil
	}
	if value > maxAgentRunPageSize {
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("orchestrator: page size exceeds %d", maxAgentRunPageSize))
	}
	return int(value), nil
}

func agentRunError(err error) error {
	switch {
	case errors.Is(err, agentruncatalog.ErrNotFound), errors.Is(err, agentruncatalog.ErrNotOwner):
		return connect.NewError(connect.CodeNotFound, errors.New("orchestrator: AgentRun not found"))
	case errors.Is(err, agentruncatalog.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, agentruncatalog.ErrConflict):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func mapAgentRun(run agentruncatalog.Run) *agentrunv1.AgentRun {
	return &agentrunv1.AgentRun{
		Uuid: run.UUID, WorkspaceUuid: run.WorkspaceUUID, ProfileName: run.ProfileName, ProfileDigest: run.ProfileDigest,
		State: mapAgentRunState(run.State), SandboxUid: run.SandboxUID, NodeFingerprint: fingerprintOrEmpty(run.NodeFingerprint), NodeInstanceId: run.NodeInstanceID, NodeJobId: run.NodeJobID,
		CreatedAt: timestampOrNil(run.CreatedAt), UpdatedAt: timestampOrNil(run.UpdatedAt), StartedAt: timestampOrNil(run.StartedAt), CompletedAt: timestampOrNil(run.CompletedAt),
		ExitCode: run.ExitCode, ExitCodeSet: run.ExitCodeSet, Reason: run.Reason, Result: run.Result, LastEventCursor: run.LastEventCursor,
	}
}

func fingerprintOrEmpty(value identity.Fingerprint) string {
	if value.IsZero() {
		return ""
	}
	return value.String()
}

func timestampOrNil(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value)
}

func mapAgentRunState(state agentruncatalog.State) agentrunv1.AgentRunState {
	switch state {
	case agentruncatalog.StateQueued:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_QUEUED
	case agentruncatalog.StateStarting:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_STARTING
	case agentruncatalog.StateRunning:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_RUNNING
	case agentruncatalog.StateCanceling:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELING
	case agentruncatalog.StateSucceeded:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_SUCCEEDED
	case agentruncatalog.StateFailed:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_FAILED
	case agentruncatalog.StateCanceled:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELED
	case agentruncatalog.StateLost:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_LOST
	default:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_UNSPECIFIED
	}
}
