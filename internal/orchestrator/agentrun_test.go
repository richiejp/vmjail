package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	"github.com/richiejp/darkaifactory/internal/controlstore"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1/agentrunv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodejob"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const (
	testAgentRunA      = "run-a"
	testAgentBootID    = "boot"
	testAgentSandbox   = "sandbox"
	testAgentProfile   = "codex"
	testAgentWorkspace = "agent-workspace"
	testAgentJob       = "agent-job"
	testAgentProject   = "project"
	testAgentDigest    = "sha256:profile"
	testAgentPrompt    = "fix it"
	testAgentImage     = "example.com/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type fakeAgentCanceler struct {
	fingerprint identity.Fingerprint
	jobID       string
	err         error
}

type fakeAgentStarter struct {
	fingerprint identity.Fingerprint
	request     nodejob.StartRequest
	snapshot    nodejob.Snapshot
	err         error
	getErr      error
	deleteErr   error
	get         nodejob.Snapshot
	deleted     string
}

type fakeAgentSandboxLauncher struct {
	owner     identity.Fingerprint
	workspace string
	policy    AgentSandboxPolicy
	ref       workspacecatalog.SandboxRef
	err       error
}

func (f *fakeAgentSandboxLauncher) LaunchAgentSandbox(owner identity.Fingerprint, workspace string, policy AgentSandboxPolicy) (workspacecatalog.SandboxRef, error) {
	f.owner, f.workspace, f.policy = owner, workspace, policy
	return f.ref, f.err
}

func (f *fakeAgentStarter) GetNodeJob(_ context.Context, _ identity.Fingerprint, _ string, _, _ uint64, _ int) (nodejob.Snapshot, error) {
	return f.get, f.getErr
}

func (f *fakeAgentStarter) DeleteNodeJob(_ context.Context, _ identity.Fingerprint, jobID string) error {
	f.deleted = jobID
	return f.deleteErr
}

func (f *fakeAgentStarter) StartNodeJob(_ context.Context, fingerprint identity.Fingerprint, request nodejob.StartRequest) (nodejob.Snapshot, error) {
	f.fingerprint, f.request = fingerprint, request
	return f.snapshot, f.err
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
	if start, ok := any(message).(*agentrunv1.StartAgentRunRequest); ok {
		if start.ImageRef == "" {
			start.ImageRef = testAgentImage
		}
		if start.Resources == nil {
			start.Resources = &agentrunv1.AgentRunResources{}
		}
		if start.Egress == nil {
			start.Egress = &agentrunv1.AgentRunEgress{}
		}
	}
	req := connect.NewRequest(message)
	req.Header().Set(peerFingerprintHeader, owner.String())
	return req
}

func createAgentRun(t *testing.T, store *agentruncatalog.Store, owner identity.Fingerprint, uuid string) agentruncatalog.Run {
	t.Helper()
	run, err := store.Create(agentruncatalog.CreateInput{Command: []string{testAgentProfile}, UUID: uuid, WorkspaceUUID: testAgentWorkspace, ProfileName: testAgentProfile, ProfileDigest: testAgentDigest, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestAgentRunHandlerStartBindsAndStartsIdempotentJob(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	workspaces := workspacecatalog.NewStore(nil)
	workspace, err := workspaces.Create(workspacecatalog.CreateInput{UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspaces.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: testAgentDigest}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	starter := &fakeAgentStarter{snapshot: nodejob.Snapshot{ID: "job"}}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspaces, nodes, starter)
	response, err := handler.Start(t.Context(), agentRunRequest(&agentrunv1.StartAgentRunRequest{
		WorkspaceUuid: workspace.Metadata.UUID, ProfileName: testAgentProfile, ProfileDigest: testAgentDigest,
		Command: []string{testAgentProfile, "exec"}, Prompt: testAgentPrompt,
	}, owner))
	if err != nil || response.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_RUNNING {
		t.Fatalf("Start = %#v, %v", response, err)
	}
	if starter.fingerprint != node || starter.request.IdempotencyKey != response.Msg.GetUuid() || starter.request.SandboxUID != testAgentSandbox {
		t.Fatalf("start request = %#v, node=%s", starter.request, starter.fingerprint)
	}
	if got := starter.request.Command; len(got) != 3 || got[2] != "fix it" {
		t.Fatalf("command = %#v", got)
	}
	stored, err := runs.Get(owner, response.Msg.GetUuid())
	if err != nil || len(stored.Command) != 2 || stored.Command[1] != "exec" {
		t.Fatalf("stored launch snapshot = %#v, %v", stored, err)
	}
}

func TestAgentRunHandlerStartLaunchesMissingSandboxWithFrozenPolicy(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	workspaces := workspacecatalog.NewStore(nil)
	workspace, err := workspaces.Create(workspacecatalog.CreateInput{UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	starter := &fakeAgentStarter{snapshot: nodejob.Snapshot{ID: testAgentJob}}
	launcher := &fakeAgentSandboxLauncher{ref: workspacecatalog.SandboxRef{UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: testAgentDigest}}
	handler := NewAgentRunHandler(runs, &fakeAgentCanceler{})
	handler.SetStartDependencies(workspaces, nodes, starter)
	handler.SetSandboxLauncher(launcher)

	request := agentRunRequest(&agentrunv1.StartAgentRunRequest{
		WorkspaceUuid: workspace.Metadata.UUID,
		ProfileName:   testAgentProfile, ProfileDigest: testAgentDigest,
		Command: []string{testAgentProfile}, Prompt: testAgentPrompt,
		Resources: &agentrunv1.AgentRunResources{Enable: true, CpuCores: 4, MemoryBytes: 8 << 30},
		Egress:    &agentrunv1.AgentRunEgress{Enable: true},
	}, owner)
	response, err := handler.Start(t.Context(), request)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if launcher.owner != owner || launcher.workspace != workspace.Metadata.UUID {
		t.Errorf("launcher binding = (%s, %q), want (%s, %q)", launcher.owner, launcher.workspace, owner, workspace.Metadata.UUID)
	}
	wantPolicy := AgentSandboxPolicy{ProfileDigest: testAgentDigest, ImageRef: testAgentImage, ResourcesEnable: true, CPUCores: 4, MemoryBytes: 8 << 30, EgressEnable: true}
	if launcher.policy != wantPolicy {
		t.Errorf("launcher policy = %#v, want %#v", launcher.policy, wantPolicy)
	}
	if response.Msg.GetSandboxUid() != testAgentSandbox || starter.request.SandboxUID != testAgentSandbox {
		t.Errorf("sandbox binding = response %q, job %q", response.Msg.GetSandboxUid(), starter.request.SandboxUID)
	}
}

func TestAgentRunHandlerStartRejectsUnusableSandboxBindings(t *testing.T) {
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	launchFailure := connect.NewError(connect.CodeResourceExhausted, errors.New("node full"))
	tests := []struct {
		name     string
		binding  *workspacecatalog.SandboxRef
		launcher agentSandboxLauncher
		wantCode connect.Code
	}{
		{name: "launcher not configured", wantCode: connect.CodeFailedPrecondition},
		{name: "launcher failure", launcher: &fakeAgentSandboxLauncher{err: launchFailure}, wantCode: connect.CodeResourceExhausted},
		{
			name: "incompatible running sandbox",
			binding: &workspacecatalog.SandboxRef{
				UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: "sha256:different",
			},
			wantCode: connect.CodeFailedPrecondition,
		},
		{
			name: "bound node disconnected",
			binding: &workspacecatalog.SandboxRef{
				UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: testAgentDigest,
			},
			wantCode: connect.CodeUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runs, _ := agentRunStore(t)
			workspaces := workspacecatalog.NewStore(nil)
			workspace, err := workspaces.Create(workspacecatalog.CreateInput{
				UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: owner,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.binding != nil {
				_, err = workspaces.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
					value.Status.CurrentSandbox = test.binding
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			handler := NewAgentRunHandler(runs, nil)
			handler.SetStartDependencies(workspaces, nodectrl.NewRegistry(nil), &fakeAgentStarter{})
			handler.SetSandboxLauncher(test.launcher)
			_, err = handler.Start(t.Context(), agentRunRequest(&agentrunv1.StartAgentRunRequest{
				WorkspaceUuid: workspace.Metadata.UUID,
				ProfileName:   testAgentProfile, ProfileDigest: testAgentDigest,
				Command: []string{testAgentProfile},
			}, owner))
			if connect.CodeOf(err) != test.wantCode {
				t.Fatalf("Start error = %v (code %s), want code %s", err, connect.CodeOf(err), test.wantCode)
			}
		})
	}
}

func TestAgentRunHandlerStartRetainsStartingOnAmbiguousFailure(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	workspaces := workspacecatalog.NewStore(nil)
	workspace, err := workspaces.Create(workspacecatalog.CreateInput{UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspaces.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: testAgentDigest}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	starter := &fakeAgentStarter{err: context.DeadlineExceeded}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspaces, nodes, starter)
	response, err := handler.Start(t.Context(), agentRunRequest(&agentrunv1.StartAgentRunRequest{WorkspaceUuid: workspace.Metadata.UUID, ProfileName: testAgentProfile, ProfileDigest: testAgentDigest, Command: []string{testAgentProfile}}, owner))
	if err != nil || response.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_STARTING {
		t.Fatalf("Start ambiguous = %#v, %v", response, err)
	}
}

func TestAgentRunHandlerCredentialRunDoesNotStartBeforeDelivery(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	workspaces := workspacecatalog.NewStore(nil)
	workspace, err := workspaces.Create(workspacecatalog.CreateInput{UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspaces.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: testAgentDigest}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	starter := &fakeAgentStarter{snapshot: nodejob.Snapshot{ID: testAgentJob}}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspaces, nodes, starter)
	response, err := handler.Start(t.Context(), agentRunRequest(&agentrunv1.StartAgentRunRequest{WorkspaceUuid: workspace.Metadata.UUID, ProfileName: testAgentProfile, ProfileDigest: testAgentDigest, Command: []string{testAgentProfile}, CredentialRequired: true, CredentialProvider: agentruncatalog.CredentialProviderCodex}, owner))
	if err != nil || response.Msg.GetState() != agentrunv1.AgentRunState_AGENT_RUN_STATE_AWAITING_CREDENTIAL || starter.request.SandboxUID != "" {
		t.Fatalf("credential Start = %#v, starter=%#v, %v", response, starter.request, err)
	}
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(owner, response.Msg.GetUuid())
	if err != nil || got.State != agentruncatalog.StateAwaitingCredential {
		t.Fatalf("live reconciliation advanced run = %#v, %v", got, err)
	}

	restarted := NewAgentRunHandler(runs, nil)
	restarted.SetStartDependencies(workspaces, nodes, starter)
	if err := restarted.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = runs.Get(owner, response.Msg.GetUuid())
	if err != nil || got.State != agentruncatalog.StateLost || got.Reason == "" {
		t.Fatalf("restart reconciliation = %#v, %v", got, err)
	}
}

func TestAgentRunReconcileRecoversStartAndCollectsResult(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run := createAgentRun(t, runs, owner, "recover")
	run, err := runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	jobs := &fakeAgentStarter{snapshot: nodejob.Snapshot{ID: testAgentJob}}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodes, jobs)
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err = runs.Get(owner, run.UUID)
	if err != nil || run.State != agentruncatalog.StateRunning || run.NodeJobID != testAgentJob {
		t.Fatalf("recovered = %#v, %v", run, err)
	}
	jobs.get = nodejob.Snapshot{ID: testAgentJob, State: nodejob.StateSucceeded, Stdout: nodejob.Chunk{Data: []byte("done")}, ExitCodeSet: true}
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err = runs.Get(owner, run.UUID)
	if err != nil || run.State != agentruncatalog.StateSucceeded || run.Result != "done" || jobs.deleted != testAgentJob {
		t.Fatalf("completed = %#v, deleted=%q, %v", run, jobs.deleted, err)
	}
}

func TestAgentRunReconcileMarksRestartedNodeLost(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run := createAgentRun(t, runs, owner, "lost")
	_, err := runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, "new-process", "node", "v1", nodectrl.Resources{})
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodes, &fakeAgentStarter{})
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(owner, run.UUID)
	if err != nil || got.State != agentruncatalog.StateLost {
		t.Fatalf("lost = %#v, %v", got, err)
	}
}

func TestAgentRunStartPreconditions(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner, other := mustGenerate(t, identity.RoleOperator).Fingerprint(), mustGenerate(t, identity.RoleClient).Fingerprint()
	handler := NewAgentRunHandler(runs, nil)
	request := agentRunRequest(&agentrunv1.StartAgentRunRequest{WorkspaceUuid: testAgentWorkspace, ProfileName: testAgentProfile, ProfileDigest: testAgentDigest, Command: []string{testAgentProfile}}, owner)
	if _, err := handler.Start(t.Context(), request); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unconfigured = %v", err)
	}
	workspaces := workspacecatalog.NewStore(nil)
	if _, err := workspaces.Create(workspacecatalog.CreateInput{UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: other}); err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	handler.SetStartDependencies(workspaces, nodes, &fakeAgentStarter{})
	if _, err := handler.Start(t.Context(), request); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign workspace = %v", err)
	}
	owned, err := workspaces.Create(workspacecatalog.CreateInput{UUID: "owned", Name: "owned", OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	request.Msg.WorkspaceUuid = owned.Metadata.UUID
	if _, err := handler.Start(t.Context(), request); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("missing Sandbox = %v", err)
	}
	_, err = workspaces.Update(owned.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: testAgentSandbox, NodeFingerprint: other, AgentProfileDigest: testAgentDigest}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Start(t.Context(), request); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("missing Node = %v", err)
	}
}

func TestAgentRunReconcileConfigurationAndRunCancellation(t *testing.T) {
	handler := NewAgentRunHandler(nil, nil)
	if err := handler.ReconcileOnce(t.Context()); err == nil {
		t.Fatal("unconfigured reconcile succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		handler.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

func TestAgentRunReconcileHandlesOutputExpiryAndDisconnectedNode(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run := createAgentRun(t, runs, owner, "expired")
	run, err := runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	run, err = runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateStarting}, To: agentruncatalog.StateRunning, NodeJobID: testAgentJob})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	jobs := &fakeAgentStarter{get: nodejob.Snapshot{ID: testAgentJob, State: nodejob.StateSucceeded, Stdout: nodejob.Chunk{CursorExpired: true}, ExitCodeSet: true}}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodes, jobs)
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	unchanged, err := runs.Get(owner, run.UUID)
	if err != nil || unchanged.State != agentruncatalog.StateRunning {
		t.Fatalf("disconnected = %#v, %v", unchanged, err)
	}
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	jobs.get = nodejob.Snapshot{ID: testAgentJob, State: nodejob.StateRunning}
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	jobs.get = nodejob.Snapshot{ID: testAgentJob, State: nodejob.StateSucceeded, Stdout: nodejob.Chunk{CursorExpired: true}, ExitCodeSet: true}
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := runs.Get(owner, run.UUID)
	if err != nil || failed.State != agentruncatalog.StateFailed || failed.Reason == "" || failed.Result != "" {
		t.Fatalf("expired = %#v, %v", failed, err)
	}
}

func TestAgentRunReconcileCanceledJobReportsDeleteFailure(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run := createAgentRun(t, runs, owner, "canceled-result")
	run, err := runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	run, err = runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateStarting}, To: agentruncatalog.StateRunning, NodeJobID: testAgentJob})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateRunning}, To: agentruncatalog.StateCanceling})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	jobs := &fakeAgentStarter{get: nodejob.Snapshot{ID: testAgentJob, State: nodejob.StateCanceled}, deleteErr: errors.New("delete failed")}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodes, jobs)
	if err := handler.ReconcileOnce(t.Context()); err == nil {
		t.Fatal("delete failure was not reported")
	}
	got, err := runs.Get(owner, run.UUID)
	if err != nil || got.State != agentruncatalog.StateCanceled {
		t.Fatalf("canceled = %#v, %v", got, err)
	}
}

func TestAgentRunReconcileRetriesTransientNodeErrors(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	starting := createAgentRun(t, runs, owner, "retry-start")
	starting, err := runs.Transition(owner, starting.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	jobs := &fakeAgentStarter{err: context.DeadlineExceeded}
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodes, jobs)
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(owner, starting.UUID)
	if err != nil || got.State != agentruncatalog.StateStarting {
		t.Fatalf("transient start = %#v, %v", got, err)
	}
	jobs.err = nil
	jobs.snapshot.ID = testAgentJob
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	jobs.getErr = context.DeadlineExceeded
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = runs.Get(owner, starting.UUID)
	if err != nil || got.State != agentruncatalog.StateRunning {
		t.Fatalf("transient get = %#v, %v", got, err)
	}
}

func TestAgentRunStartRejectsInvalidResolvedProfile(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	workspaces := workspacecatalog.NewStore(nil)
	workspace, err := workspaces.Create(workspacecatalog.CreateInput{UUID: testAgentWorkspace, Name: testAgentProject, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspaces.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: testAgentSandbox, NodeFingerprint: node, AgentProfileDigest: testAgentDigest}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, testAgentBootID, "node", "v1", nodectrl.Resources{})
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspaces, nodes, &fakeAgentStarter{})
	request := agentRunRequest(&agentrunv1.StartAgentRunRequest{WorkspaceUuid: workspace.Metadata.UUID, ProfileName: testAgentProfile, ProfileDigest: testAgentDigest}, owner)
	if _, err := handler.Start(t.Context(), request); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid resolved profile = %v", err)
	}
}

func TestAgentSandboxPolicyValidation(t *testing.T) {
	valid := func() *agentrunv1.StartAgentRunRequest {
		return &agentrunv1.StartAgentRunRequest{
			ProfileName: testAgentProfile, ProfileDigest: testAgentDigest,
			Command: []string{testAgentProfile}, ImageRef: testAgentImage,
			Resources: &agentrunv1.AgentRunResources{}, Egress: &agentrunv1.AgentRunEgress{},
		}
	}
	tests := []struct {
		name   string
		mutate func(*agentrunv1.StartAgentRunRequest)
	}{
		{"incomplete", func(request *agentrunv1.StartAgentRunRequest) { request.ImageRef = "" }},
		{"resources missing", func(request *agentrunv1.StartAgentRunRequest) { request.Resources = nil }},
		{"resources partially enabled", func(request *agentrunv1.StartAgentRunRequest) {
			request.Resources = &agentrunv1.AgentRunResources{Enable: true, CpuCores: 1}
		}},
		{"resources excessive", func(request *agentrunv1.StartAgentRunRequest) {
			request.Resources = &agentrunv1.AgentRunResources{Enable: true, CpuCores: 1025, MemoryBytes: 1}
		}},
		{"image unpinned", func(request *agentrunv1.StartAgentRunRequest) { request.ImageRef = "example.com/agent:latest" }},
		{"credential required without provider", func(request *agentrunv1.StartAgentRunRequest) { request.CredentialRequired = true }},
		{"credential provider without requirement", func(request *agentrunv1.StartAgentRunRequest) {
			request.CredentialProvider = agentruncatalog.CredentialProviderCodex
		}},
		{"unsupported credential provider", func(request *agentrunv1.StartAgentRunRequest) {
			request.CredentialRequired, request.CredentialProvider = true, "claude"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid()
			test.mutate(request)
			if _, err := agentSandboxPolicy(request); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("agentSandboxPolicy error = %v", err)
			}
		})
	}
	want := AgentSandboxPolicy{ProfileDigest: testAgentDigest, ImageRef: testAgentImage}
	if got, err := agentSandboxPolicy(valid()); err != nil || got != want {
		t.Fatalf("agentSandboxPolicy valid = %#v, %v; want %#v", got, err, want)
	}
}

func TestAgentRunReconcileBoundsNodeAbsence(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	node := mustGenerate(t, identity.RoleSandboxHost).Fingerprint()
	run := createAgentRun(t, runs, owner, "missing-node")
	run, err := runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateStarting, SandboxUID: testAgentSandbox, NodeFingerprint: node, NodeInstanceID: testAgentBootID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodectrl.NewRegistry(nil), &fakeAgentStarter{})
	handler.setReconcileClock(func() time.Time { return now }, time.Minute)
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(owner, run.UUID)
	if err != nil || got.State != agentruncatalog.StateStarting {
		t.Fatalf("within grace = %#v, %v", got, err)
	}
	now = now.Add(time.Minute)
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = runs.Get(owner, run.UUID)
	if err != nil || got.State != agentruncatalog.StateLost {
		t.Fatalf("after grace = %#v, %v", got, err)
	}
}

func TestAgentRunReconcileFailsCrashStrandedQueuedRun(t *testing.T) {
	runs, _ := agentRunStore(t)
	owner := mustGenerate(t, identity.RoleOperator).Fingerprint()
	run := createAgentRun(t, runs, owner, "stranded-queued")
	handler := NewAgentRunHandler(runs, nil)
	handler.SetStartDependencies(workspacecatalog.NewStore(nil), nodectrl.NewRegistry(nil), &fakeAgentStarter{})
	if err := handler.ReconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(owner, run.UUID)
	if err != nil || got.State != agentruncatalog.StateFailed {
		t.Fatalf("stranded = %#v, %v", got, err)
	}
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
		agentruncatalog.StateQueued:             agentrunv1.AgentRunState_AGENT_RUN_STATE_QUEUED,
		agentruncatalog.StateAwaitingCredential: agentrunv1.AgentRunState_AGENT_RUN_STATE_AWAITING_CREDENTIAL,
		agentruncatalog.StateStarting:           agentrunv1.AgentRunState_AGENT_RUN_STATE_STARTING,
		agentruncatalog.StateRunning:            agentrunv1.AgentRunState_AGENT_RUN_STATE_RUNNING,
		agentruncatalog.StateCanceling:          agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELING,
		agentruncatalog.StateSucceeded:          agentrunv1.AgentRunState_AGENT_RUN_STATE_SUCCEEDED,
		agentruncatalog.StateFailed:             agentrunv1.AgentRunState_AGENT_RUN_STATE_FAILED,
		agentruncatalog.StateCanceled:           agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELED,
		agentruncatalog.StateLost:               agentrunv1.AgentRunState_AGENT_RUN_STATE_LOST,
		"unknown":                               agentrunv1.AgentRunState_AGENT_RUN_STATE_UNSPECIFIED,
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
