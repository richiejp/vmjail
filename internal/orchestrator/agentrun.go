package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/agentprofile"
	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1/agentrunv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodejob"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const (
	defaultAgentRunPageSize  = 50
	maxAgentRunPageSize      = 200
	defaultAgentRunSweep     = 2 * time.Second
	defaultAgentRunNodeGrace = 30 * time.Second
)

type agentJobCanceler interface {
	CancelNodeJob(context.Context, identity.Fingerprint, string) (nodejob.Snapshot, error)
}

type agentJobStarter interface {
	StartNodeJob(context.Context, identity.Fingerprint, nodejob.StartRequest) (nodejob.Snapshot, error)
}

type agentJobReconciler interface {
	agentJobStarter
	GetNodeJob(context.Context, identity.Fingerprint, string, uint64, uint64, int) (nodejob.Snapshot, error)
	DeleteNodeJob(context.Context, identity.Fingerprint, string) error
}

type agentSandboxLauncher interface {
	LaunchAgentSandbox(identity.Fingerprint, string, AgentSandboxPolicy) (workspacecatalog.SandboxRef, error)
}

type agentSandboxStopper interface {
	StopAgentSandbox(identity.Fingerprint, string, string) error
}

type AgentRunHandler struct {
	agentrunv1connect.UnimplementedAgentRunServiceHandler
	store           *agentruncatalog.Store
	canceler        agentJobCanceler
	starter         agentJobStarter
	reconciler      agentJobReconciler
	workspaces      *workspacecatalog.Store
	nodes           *nodectrl.Registry
	launcher        agentSandboxLauncher
	stopper         agentSandboxStopper
	now             func() time.Time
	nodeGrace       time.Duration
	missingMu       sync.Mutex
	missingSince    map[string]time.Time
	credentialMu    sync.Mutex
	credentialWaits map[string]struct{}
	bindMu          sync.Mutex
}

// Run periodically reconciles durable runs until ctx is canceled.
func (h *AgentRunHandler) Run(ctx context.Context) {
	ticker := time.NewTicker(defaultAgentRunSweep)
	defer ticker.Stop()
	_ = h.ReconcileOnce(ctx)
	//daft:lint:disable=forloopbound reason="exits on ctx.Done; retry cadence is bounded by defaultAgentRunSweep"
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = h.ReconcileOnce(ctx)
		}
	}
}

// SetStartDependencies wires the internal trusted start controller. Keeping
// these separate from the read/cancel constructor preserves small unit-test
// fakes while production always supplies the workspace and live-node stores.
func (h *AgentRunHandler) SetStartDependencies(workspaces *workspacecatalog.Store, nodes *nodectrl.Registry, starter agentJobStarter) {
	h.workspaces, h.nodes, h.starter = workspaces, nodes, starter
	h.reconciler, _ = starter.(agentJobReconciler)
}

func (h *AgentRunHandler) SetSandboxLauncher(launcher agentSandboxLauncher) {
	h.launcher = launcher
	h.stopper, _ = launcher.(agentSandboxStopper)
}

func NewAgentRunHandler(store *agentruncatalog.Store, canceler agentJobCanceler) *AgentRunHandler {
	return &AgentRunHandler{store: store, canceler: canceler, now: time.Now, nodeGrace: defaultAgentRunNodeGrace, missingSince: make(map[string]time.Time), credentialWaits: make(map[string]struct{})}
}

func (h *AgentRunHandler) setReconcileClock(now func() time.Time, grace time.Duration) {
	h.now, h.nodeGrace = now, grace
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

func (h *AgentRunHandler) Start(ctx context.Context, req *connect.Request[agentrunv1.StartAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
	owner, err := h.owner(req.Header())
	if err != nil {
		return nil, err
	}
	policy, err := h.startPolicy(req.Msg)
	if err != nil {
		return nil, err
	}
	if response, found, findErr := h.findAgentRunRetry(owner, req.Msg); found || findErr != nil {
		return response, findErr
	}
	workspace, err := h.workspaces.Get(req.Msg.GetWorkspaceUuid())
	if err != nil || workspace.Metadata.OwnerFingerprint != owner {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("orchestrator: Workspace not found"))
	}
	binding := workspace.Status.CurrentSandbox
	if binding == nil {
		if h.launcher == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: AgentRun Sandbox launcher is not configured"))
		}
		launched, launchErr := h.launcher.LaunchAgentSandbox(owner, workspace.Metadata.UUID, policy)
		if launchErr != nil {
			return nil, launchErr
		}
		binding = &launched
	} else if binding.AgentProfileDigest != policy.ProfileDigest {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: Workspace current Sandbox is incompatible with the requested AgentProfile; stop it before starting the agent"))
	}
	node, ok := h.nodes.Lookup(binding.NodeFingerprint)
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("orchestrator: Workspace Node is not connected"))
	}
	// Keep the process-local recovery worker from observing the intentionally
	// brief QUEUED record between Create and its durable Sandbox binding. After
	// a process restart the lock is gone, so a genuinely stranded QUEUED record
	// is still failed by reconciliation as designed.
	h.bindMu.Lock()
	run, created, err := h.store.CreateOrGet(agentruncatalog.CreateInput{
		UUID: mintWorkspaceUUID(), RequestID: req.Msg.GetRequestId(), WorkspaceUUID: workspace.Metadata.UUID,
		ProfileName: req.Msg.GetProfileName(), ProfileDigest: req.Msg.GetProfileDigest(),
		Prompt: req.Msg.GetPrompt(), Command: req.Msg.GetCommand(), OwnerFingerprint: owner,
		ImageRef: policy.ImageRef, ResourcesEnable: policy.ResourcesEnable,
		CPUCores: policy.CPUCores, MemoryBytes: policy.MemoryBytes, EgressEnable: policy.EgressEnable,
		CredentialRequired: req.Msg.GetCredentialRequired(), CredentialProvider: req.Msg.GetCredentialProvider(),
	})
	if err != nil {
		h.bindMu.Unlock()
		return nil, agentRunError(err)
	}
	if !created {
		h.bindMu.Unlock()
		return connect.NewResponse(mapAgentRun(run)), nil
	}
	nextState := agentruncatalog.StateStarting
	message := "sandbox assigned"
	if run.CredentialRequired {
		nextState = agentruncatalog.StateAwaitingCredential
		message = "sandbox awaiting credential"
	}
	run, err = h.store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{
		From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: nextState,
		Message: message, SandboxUID: binding.UID,
		NodeFingerprint: binding.NodeFingerprint, NodeInstanceID: node.InstanceID,
	})
	h.bindMu.Unlock()
	if err != nil {
		return nil, agentRunError(err)
	}
	if run.State == agentruncatalog.StateAwaitingCredential {
		h.markCredentialWait(run.UUID)
		return connect.NewResponse(mapAgentRun(run)), nil
	}
	return h.startBoundRun(ctx, owner, run)
}

func (h *AgentRunHandler) startBoundRun(ctx context.Context, owner identity.Fingerprint, run agentruncatalog.Run) (*connect.Response[agentrunv1.AgentRun], error) {
	command := append([]string(nil), run.Command...)
	if run.Prompt != "" {
		command = append(command, run.Prompt)
	}
	job, startErr := h.starter.StartNodeJob(ctx, run.NodeFingerprint, nodejob.StartRequest{IdempotencyKey: run.UUID, SandboxUID: run.SandboxUID, Command: command})
	if startErr != nil {
		// STARTING is durable and carries the exact process-instance binding and
		// argv. Reconciliation can safely retry this ambiguous transport result
		// with the same Node idempotency key.
		return connect.NewResponse(mapAgentRun(run)), nil //nolint:nilerr // durable STARTING is the successful, retryable result
	}
	run, err := recordJobRunning(h.store, owner, run.UUID, job.ID, "job started")
	if err != nil {
		return nil, agentRunError(err)
	}
	return connect.NewResponse(mapAgentRun(run)), nil
}

func (h *AgentRunHandler) startPolicy(req *agentrunv1.StartAgentRunRequest) (AgentSandboxPolicy, error) {
	if !canonicalUUID(req.GetRequestId()) {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: request_id must be a canonical UUID"))
	}
	policy, err := agentSandboxPolicy(req)
	if err != nil {
		return AgentSandboxPolicy{}, err
	}
	if h.workspaces == nil || h.nodes == nil || h.starter == nil {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: AgentRun start controller is not configured"))
	}
	return policy, nil
}

func (h *AgentRunHandler) findAgentRunRetry(owner identity.Fingerprint, req *agentrunv1.StartAgentRunRequest) (*connect.Response[agentrunv1.AgentRun], bool, error) {
	existing, err := h.store.GetByRequest(owner, req.GetRequestId())
	if errors.Is(err, agentruncatalog.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, agentRunError(err)
	}
	if !sameAgentStart(existing, req) {
		return nil, true, connect.NewError(connect.CodeAlreadyExists, errors.New("orchestrator: request_id was already used for different AgentRun inputs"))
	}
	return connect.NewResponse(mapAgentRun(existing)), true, nil
}

func sameAgentStart(run agentruncatalog.Run, req *agentrunv1.StartAgentRunRequest) bool {
	return run.RequestID == req.GetRequestId() &&
		run.WorkspaceUUID == req.GetWorkspaceUuid() &&
		run.ProfileName == req.GetProfileName() &&
		run.ProfileDigest == req.GetProfileDigest() &&
		run.ImageRef == req.GetImageRef() &&
		run.ResourcesEnable == req.GetResources().GetEnable() &&
		run.CPUCores == req.GetResources().GetCpuCores() &&
		run.MemoryBytes == req.GetResources().GetMemoryBytes() &&
		run.EgressEnable == req.GetEgress().GetEnable() &&
		run.Prompt == req.GetPrompt() &&
		slices.Equal(run.Command, req.GetCommand()) &&
		run.CredentialRequired == req.GetCredentialRequired() &&
		run.CredentialProvider == req.GetCredentialProvider()
}

func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func agentSandboxPolicy(req *agentrunv1.StartAgentRunRequest) (AgentSandboxPolicy, error) {
	resources := req.GetResources()
	if req.GetProfileName() == "" || req.GetProfileDigest() == "" || len(req.GetCommand()) == 0 || req.GetImageRef() == "" || req.GetEgress() == nil {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: incomplete resolved AgentProfile"))
	}
	if resources == nil {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: AgentProfile resources section is required"))
	}
	if req.GetCredentialRequired() != (req.GetCredentialProvider() != "") || (req.GetCredentialProvider() != "" && req.GetCredentialProvider() != agentruncatalog.CredentialProviderCodex) {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: credential provider must be codex exactly when credentials are required"))
	}
	if resources.GetEnable() != (resources.GetCpuCores() != 0 && resources.GetMemoryBytes() != 0) {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: AgentProfile resource values must both be positive exactly when enabled"))
	}
	if resources.GetCpuCores() > agentprofile.MaxCPUCores || resources.GetMemoryBytes() > agentprofile.MaxMemoryBytes {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("orchestrator: AgentProfile resources exceed %d CPU cores or %d memory bytes", agentprofile.MaxCPUCores, agentprofile.MaxMemoryBytes))
	}
	if _, err := name.NewDigest(req.GetImageRef(), name.StrictValidation); err != nil {
		return AgentSandboxPolicy{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("orchestrator: AgentProfile image must be digest-pinned: %w", err))
	}
	return AgentSandboxPolicy{
		ProfileDigest: req.GetProfileDigest(), ImageRef: req.GetImageRef(),
		ResourcesEnable: resources.GetEnable(), CPUCores: resources.GetCpuCores(), MemoryBytes: resources.GetMemoryBytes(),
		EgressEnable:       req.GetEgress().GetEnable(),
		CredentialProvider: req.GetCredentialProvider(),
	}, nil
}

// ReconcileOnce advances every durable active run using only its frozen launch
// snapshot and exact Node process binding. Transport errors are retryable; a
// changed process instance is definitive evidence that process-local jobs were
// lost.
func (h *AgentRunHandler) ReconcileOnce(ctx context.Context) error {
	if h.store == nil || h.nodes == nil || h.reconciler == nil {
		return errors.New("orchestrator: AgentRun reconciliation is not configured")
	}
	var joined error
	h.bindMu.Lock()
	active := h.store.Active()
	h.bindMu.Unlock()
	for i := range active {
		run := active[i]
		if err := h.reconcileRun(ctx, run); err != nil {
			joined = errors.Join(joined, fmt.Errorf("AgentRun %s: %w", run.UUID, err))
		}
	}
	return joined
}

func (h *AgentRunHandler) reconcileRun(ctx context.Context, run agentruncatalog.Run) error {
	if run.State.Terminal() && run.TeardownPending {
		return h.reconcileTerminalTeardown(ctx, run)
	}
	if run.State == agentruncatalog.StateQueued {
		_, err := h.store.Transition(run.OwnerFingerprint, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateFailed, Message: "startup binding unavailable", Reason: "orchestrator restarted before the run was bound to a Sandbox"})
		return err
	}
	if run.State == agentruncatalog.StateAwaitingCredential {
		return h.reconcileCredentialWait(run)
	}
	node, connected := h.nodes.Lookup(run.NodeFingerprint)
	if !connected {
		if !h.nodeAbsenceExpired(run.UUID) {
			return nil
		}
		_, err := h.store.Transition(run.OwnerFingerprint, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateLost, Message: "node unavailable", Reason: "Node remained disconnected beyond the AgentRun recovery grace period"})
		return err
	}
	h.clearNodeAbsence(run.UUID)
	if node.InstanceID != run.NodeInstanceID {
		_, err := h.store.Transition(run.OwnerFingerprint, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateLost, Message: "node process restarted", Reason: "detached job was lost when the Node process restarted"})
		return err
	}
	if run.State == agentruncatalog.StateStarting {
		return h.reconcileStarting(ctx, run)
	}
	if run.State != agentruncatalog.StateRunning && run.State != agentruncatalog.StateCanceling {
		return nil
	}
	job, err := h.reconciler.GetNodeJob(ctx, run.NodeFingerprint, run.NodeJobID, 0, 0, agentruncatalog.MaxResultBytes)
	if err != nil {
		return nil //nolint:nilerr // a disconnected control stream is retried on the next sweep
	}
	if !job.State.Terminal() {
		return nil
	}
	to := agentruncatalog.StateFailed
	switch job.State {
	case nodejob.StateSucceeded:
		to = agentruncatalog.StateSucceeded
	case nodejob.StateCanceled:
		to = agentruncatalog.StateCanceled
	}
	if run.State == agentruncatalog.StateCanceling && to == agentruncatalog.StateSucceeded {
		to = agentruncatalog.StateCanceled
	}
	result := string(job.Stdout.Data)
	if job.Stderr.CursorExpired || job.Stdout.CursorExpired {
		to, result = agentruncatalog.StateFailed, ""
		job.Reason = "detached job output expired before collection"
	}
	terminal, err := h.store.Transition(run.OwnerFingerprint, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: to, Message: "job completed", ExitCode: job.ExitCode, ExitCodeSet: job.ExitCodeSet, Reason: job.Reason, Result: result})
	if err != nil {
		return err
	}
	return h.reconcileTerminalTeardown(ctx, terminal)
}

func (h *AgentRunHandler) reconcileStarting(ctx context.Context, run agentruncatalog.Run) error {
	if len(run.Command) == 0 {
		_, err := h.store.Transition(run.OwnerFingerprint, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateLost, Message: "launch snapshot unavailable", Reason: "launch snapshot unavailable after upgrade"})
		return err
	}
	command := append([]string(nil), run.Command...)
	if run.Prompt != "" {
		command = append(command, run.Prompt)
	}
	job, err := h.reconciler.StartNodeJob(ctx, run.NodeFingerprint, nodejob.StartRequest{IdempotencyKey: run.UUID, SandboxUID: run.SandboxUID, Command: command})
	if err != nil {
		return nil //nolint:nilerr // an ambiguous idempotent start remains retryable
	}
	_, err = recordJobRunning(h.store, run.OwnerFingerprint, run.UUID, job.ID, "job recovered")
	return err
}

func recordJobRunning(store *agentruncatalog.Store, owner identity.Fingerprint, runUUID, jobID, message string) (agentruncatalog.Run, error) {
	run, err := store.Transition(owner, runUUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateStarting}, To: agentruncatalog.StateRunning, Message: message, NodeJobID: jobID})
	if !errors.Is(err, agentruncatalog.ErrConflict) {
		return run, err
	}
	current, getErr := store.Get(owner, runUUID)
	if getErr != nil {
		return agentruncatalog.Run{}, getErr
	}
	// StartNodeJob is idempotent by AgentRun UUID. A concurrent recovery pass
	// may have recorded and even completed this exact job while the original
	// request was waiting on Sandbox readiness. That is forward progress, not a
	// conflicting launch.
	if current.NodeJobID == jobID && current.State != agentruncatalog.StateStarting {
		return current, nil
	}
	return agentruncatalog.Run{}, err
}

func (h *AgentRunHandler) reconcileTerminalTeardown(ctx context.Context, run agentruncatalog.Run) error {
	if h.stopper == nil {
		return errors.New("orchestrator: AgentRun Sandbox teardown is not configured")
	}
	if run.NodeJobID != "" {
		if node, connected := h.nodes.Lookup(run.NodeFingerprint); connected && node.InstanceID == run.NodeInstanceID {
			if err := h.reconciler.DeleteNodeJob(ctx, run.NodeFingerprint, run.NodeJobID); err != nil && !errors.Is(err, nodejob.ErrNotFound) {
				return err
			}
		}
	}
	if err := h.stopper.StopAgentSandbox(run.OwnerFingerprint, run.WorkspaceUUID, run.SandboxUID); err != nil {
		return err
	}
	_, err := h.store.CompleteTeardown(run.OwnerFingerprint, run.UUID)
	return err
}

func (h *AgentRunHandler) reconcileCredentialWait(run agentruncatalog.Run) error {
	if h.hasCredentialWait(run.UUID) {
		return nil
	}
	_, err := h.store.Transition(run.OwnerFingerprint, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateLost, Message: "credential delivery lost", Reason: "orchestrator restarted before ephemeral credential delivery completed"})
	return err
}

func (h *AgentRunHandler) nodeAbsenceExpired(uuid string) bool {
	now := h.now()
	h.missingMu.Lock()
	defer h.missingMu.Unlock()
	since, exists := h.missingSince[uuid]
	if !exists {
		h.missingSince[uuid] = now
		return false
	}
	return !now.Before(since.Add(h.nodeGrace))
}

func (h *AgentRunHandler) clearNodeAbsence(uuid string) {
	h.missingMu.Lock()
	delete(h.missingSince, uuid)
	h.missingMu.Unlock()
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
	case agentruncatalog.StateQueued, agentruncatalog.StateAwaitingCredential, agentruncatalog.StateStarting:
		run, err = h.store.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{run.State}, To: agentruncatalog.StateCanceled, Message: "run canceled"})
		h.clearCredentialWait(run.UUID)
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
	case agentruncatalog.StateAwaitingCredential:
		return agentrunv1.AgentRunState_AGENT_RUN_STATE_AWAITING_CREDENTIAL
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

func (h *AgentRunHandler) markCredentialWait(uuid string) {
	h.credentialMu.Lock()
	h.credentialWaits[uuid] = struct{}{}
	h.credentialMu.Unlock()
}

func (h *AgentRunHandler) hasCredentialWait(uuid string) bool {
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	_, ok := h.credentialWaits[uuid]
	return ok
}

func (h *AgentRunHandler) clearCredentialWait(uuid string) {
	h.credentialMu.Lock()
	delete(h.credentialWaits, uuid)
	h.credentialMu.Unlock()
}
