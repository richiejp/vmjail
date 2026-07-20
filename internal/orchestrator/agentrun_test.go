package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	"github.com/richiejp/darkaifactory/internal/controlstore"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1/agentrunv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const (
	testAgentRunA    = "run-a"
	testAgentBootID  = "boot"
	testAgentSandbox = "sandbox"
)

type fakeAgentCanceler struct {
	fingerprint identity.Fingerprint
	jobID       string
	err         error
}

func (f *fakeAgentCanceler) CancelNodeJob(_ context.Context, fingerprint identity.Fingerprint, jobID string) (nodejob.Snapshot, error) {
	f.fingerprint, f.jobID = fingerprint, jobID
	return nodejob.Snapshot{}, f.err
}

func agentRunStore(t *testing.T) (runs *agentruncatalog.Store, control *controlstore.Store) {
	t.Helper()
	control, err := controlstore.Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Close(); err != nil {
			t.Errorf("close control store: %v", err)
		}
	})
	store, err := agentruncatalog.Open(control.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, control
}

func agentRunRequest[T any](message *T, owner identity.Fingerprint) *connect.Request[T] {
	req := connect.NewRequest(message)
	req.Header().Set(peerFingerprintHeader, owner.String())
	return req
}

func createAgentRun(t *testing.T, store *agentruncatalog.Store, owner identity.Fingerprint, uuid string) agentruncatalog.Run {
	t.Helper()
	run, err := store.Create(agentruncatalog.CreateInput{UUID: uuid, WorkspaceUUID: "workspace", ProfileName: "codex", ProfileDigest: "sha256:profile", OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestAgentRunHandlerOwnerScopedReadsEventsAndPagination(t *testing.T) {
	store, _ := agentRunStore(t)
	owner, other := mustGenerate(t, identity.RoleOperator).Fingerprint(), mustGenerate(t, identity.RoleClient).Fingerprint()
	createAgentRun(t, store, owner, "run-b")
	createAgentRun(t, store, owner, testAgentRunA)
	createAgentRun(t, store, other, "foreign")
	handler := NewAgentRunHandler(store, &fakeAgentCanceler{})

	first, err := handler.List(t.Context(), agentRunRequest(&agentrunv1.ListAgentRunsRequest{PageSize: 1}, owner))
	if err != nil || len(first.Msg.GetRuns()) != 1 || first.Msg.GetRuns()[0].GetUuid() != testAgentRunA || first.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	second, err := handler.List(t.Context(), agentRunRequest(&agentrunv1.ListAgentRunsRequest{PageSize: 1, PageToken: first.Msg.GetNextPageToken()}, owner))
	if err != nil || len(second.Msg.GetRuns()) != 1 || second.Msg.GetRuns()[0].GetUuid() != "run-b" || second.Msg.GetNextPageToken() != "" {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	got, err := handler.Get(t.Context(), agentRunRequest(&agentrunv1.GetAgentRunRequest{Uuid: testAgentRunA}, owner))
	if err != nil || got.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_QUEUED || got.Msg.GetNodeFingerprint() != "" || got.Msg.GetCreatedAt() == nil {
		t.Fatalf("Get = %#v, %v", got, err)
	}
	events, err := handler.Events(t.Context(), agentRunRequest(&agentrunv1.AgentRunEventsRequest{Uuid: testAgentRunA}, owner))
	if err != nil || len(events.Msg.GetEvents()) != 1 || events.Msg.GetNextCursor() != 1 || events.Msg.GetLastEventCursor() != 1 || events.Msg.GetTerminal() {
		t.Fatalf("Events = %#v, %v", events, err)
	}
	if _, err := handler.Get(t.Context(), agentRunRequest(&agentrunv1.GetAgentRunRequest{Uuid: "foreign"}, owner)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign Get code = %v", connect.CodeOf(err))
	}
	if _, err := handler.List(t.Context(), agentRunRequest(&agentrunv1.ListAgentRunsRequest{PageSize: 201}, owner)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversize List code = %v", connect.CodeOf(err))
	}
}

func TestAgentRunHandlerCancellationIsIdempotentAndRoutesRunningJob(t *testing.T) {
	store, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	handler := NewAgentRunHandler(store, &fakeAgentCanceler{})
	createAgentRun(t, store, owner, "queued")
	first, err := handler.Cancel(t.Context(), agentRunRequest(&agentrunv1.CancelAgentRunRequest{Uuid: "queued"}, owner))
	if err != nil || first.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELED {
		t.Fatalf("cancel queued = %#v, %v", first, err)
	}
	second, err := handler.Cancel(t.Context(), agentRunRequest(&agentrunv1.CancelAgentRunRequest{Uuid: "queued"}, owner))
	if err != nil || second.Msg.GetLastEventCursor() != first.Msg.GetLastEventCursor() {
		t.Fatalf("repeat cancel = %#v, %v", second, err)
	}

	run := createAgentRun(t, store, owner, "running")
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run, err = store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateStarting}, To: agentruncatalog.StateRunning, NodeJobID: orchestratorTestJobID})
	if err != nil {
		t.Fatal(err)
	}
	canceler := &fakeAgentCanceler{}
	handler = NewAgentRunHandler(store, canceler)
	canceled, err := handler.Cancel(t.Context(), agentRunRequest(&agentrunv1.CancelAgentRunRequest{Uuid: "running"}, owner))
	if err != nil || canceled.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELING || canceler.fingerprint != node || canceler.jobID != orchestratorTestJobID {
		t.Fatalf("cancel running = %#v, %v, canceler=%#v", canceled, err, canceler)
	}
}

func TestAgentRunHandlerRejectsMissingStateAndMapsErrors(t *testing.T) {
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	if _, err := NewAgentRunHandler(nil, nil).Get(t.Context(), agentRunRequest(&agentrunv1.GetAgentRunRequest{}, owner)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("nil store code = %v", connect.CodeOf(err))
	}
	store, control := agentRunStore(t)
	handler := NewAgentRunHandler(store, nil)
	if _, err := handler.Get(t.Context(), connect.NewRequest(&agentrunv1.GetAgentRunRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing identity code = %v", connect.CodeOf(err))
	}
	if _, err := handler.Events(t.Context(), agentRunRequest(&agentrunv1.AgentRunEventsRequest{Uuid: "missing", Limit: 201}, owner)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversize events code = %v", connect.CodeOf(err))
	}
	createAgentRun(t, store, owner, "closed")
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Events(t.Context(), agentRunRequest(&agentrunv1.AgentRunEventsRequest{Uuid: "closed", Limit: 1}, owner)); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("closed store code = %v", connect.CodeOf(err))
	}
}

func TestAgentRunServiceOverMTLS(t *testing.T) {
	store, _ := agentRunStore(t)
	env := newOrchTestEnv(t, identity.RoleOperator, Config{AgentRunCatalog: store})
	if env.server.AgentRunCatalog() != store {
		t.Fatal("AgentRunCatalog() did not return the configured catalog")
	}
	run := createAgentRun(t, store, env.callerID.Fingerprint(), "wire-run")
	client := agentrunv1connect.NewAgentRunServiceClient(env.httpc, env.baseURL)
	response, err := client.Get(t.Context(), connect.NewRequest(&agentrunv1.GetAgentRunRequest{Uuid: run.UUID}))
	if err != nil || response.Msg.GetUuid() != run.UUID {
		t.Fatalf("wire Get = %#v, %v", response, err)
	}
}

func TestAgentRunErrorFallback(t *testing.T) {
	if connect.CodeOf(agentRunError(errors.New("boom"))) != connect.CodeInternal {
		t.Fatal("unknown error did not map to internal")
	}
	if connect.CodeOf(agentRunError(agentruncatalog.ErrInvalid)) != connect.CodeInvalidArgument {
		t.Fatal("invalid error did not map to invalid argument")
	}
	if connect.CodeOf(agentRunError(agentruncatalog.ErrConflict)) != connect.CodeFailedPrecondition {
		t.Fatal("conflict error did not map to failed precondition")
	}
}

func TestMapAgentRunState(t *testing.T) {
	cases := map[agentruncatalog.State]agentrunv1.AgentRunState{
		agentruncatalog.StateQueued:    agentrunv1.AgentRunState_AGENT_RUN_STATE_QUEUED,
		agentruncatalog.StateStarting:  agentrunv1.AgentRunState_AGENT_RUN_STATE_STARTING,
		agentruncatalog.StateRunning:   agentrunv1.AgentRunState_AGENT_RUN_STATE_RUNNING,
		agentruncatalog.StateCanceling: agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELING,
		agentruncatalog.StateSucceeded: agentrunv1.AgentRunState_AGENT_RUN_STATE_SUCCEEDED,
		agentruncatalog.StateFailed:    agentrunv1.AgentRunState_AGENT_RUN_STATE_FAILED,
		agentruncatalog.StateCanceled:  agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELED,
		agentruncatalog.StateLost:      agentrunv1.AgentRunState_AGENT_RUN_STATE_LOST,
		"unknown":                      agentrunv1.AgentRunState_AGENT_RUN_STATE_UNSPECIFIED,
	}
	for input, want := range cases {
		if got := mapAgentRunState(input); got != want {
			t.Errorf("mapAgentRunState(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestAgentRunHandlerCancelStartingAndCanceling(t *testing.T) {
	store, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run := createAgentRun(t, store, owner, "starting")
	run, err := store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{
		From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting,
		SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewAgentRunHandler(store, nil)
	response, err := handler.Cancel(t.Context(), agentRunRequest(&agentrunv1.CancelAgentRunRequest{Uuid: run.UUID}, owner))
	if err != nil || response.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELED {
		t.Fatalf("cancel starting = %#v, %v", response, err)
	}

	running := createAgentRun(t, store, owner, "canceling")
	running, err = store.Transition(owner, running.UUID, agentruncatalog.TransitionInput{
		From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting,
		SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err = store.Transition(owner, running.UUID, agentruncatalog.TransitionInput{
		From: []agentruncatalog.State{agentruncatalog.StateStarting}, To: agentruncatalog.StateRunning, NodeJobID: orchestratorTestJobID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Cancel(t.Context(), agentRunRequest(&agentrunv1.CancelAgentRunRequest{Uuid: running.UUID}, owner)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("nil canceler code = %v", connect.CodeOf(err))
	}
	running, err = store.Transition(owner, running.UUID, agentruncatalog.TransitionInput{
		From: []agentruncatalog.State{agentruncatalog.StateRunning}, To: agentruncatalog.StateCanceling,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err = handler.Cancel(t.Context(), agentRunRequest(&agentrunv1.CancelAgentRunRequest{Uuid: running.UUID}, owner))
	if err != nil || response.Msg.GetLastEventCursor() != running.LastEventCursor {
		t.Fatalf("repeat canceling = %#v, %v", response, err)
	}
}
