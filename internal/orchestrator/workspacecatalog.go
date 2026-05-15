package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// errUnauthenticated is returned when an inbound request has no
// mTLS peer-fingerprint header. Kept distinct from connect's own
// errors so the handler methods don't repeat the wrapping boilerplate.
var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated,
	errors.New("orchestrator: no mTLS peer identity"))

// Dispatcher is the orchestrator-side seam Launch uses to publish
// a SandboxWatchEvent to a Node. The production implementation is
// *NodeControlHandler; tests inject fakes that observe what Launch
// sent without needing a live Node stream.
type Dispatcher interface {
	PublishSandboxEvent(fp identity.Fingerprint, eventType sandboxv1.EventType, sandbox *sandboxv1.Sandbox) error
}

// WorkspaceCatalogHandler adapts the workspacecatalog.Store to the
// Connect-RPC WorkspaceCatalogService surface. Peer identity for
// every RPC is the mTLS leaf-cert fingerprint surfaced via
// withPeerMiddleware. For Slice B2a, any authenticated caller can
// mutate any workspace — the per-owner lock subsystem that gates
// mutation lands in B2d.
type WorkspaceCatalogHandler struct {
	workspacev1connect.UnimplementedWorkspaceCatalogServiceHandler
	store      *workspacecatalog.Store
	dispatcher Dispatcher
}

// NewWorkspaceCatalogHandler returns a handler backed by store and
// dispatcher. dispatcher may be nil — Launch will then refuse with
// Unimplemented, which is the right shape for catalog-only test
// rigs that exercise the read/mutate surface in isolation.
func NewWorkspaceCatalogHandler(store *workspacecatalog.Store, dispatcher Dispatcher) (*WorkspaceCatalogHandler, error) {
	if store == nil {
		return nil, errors.New("orchestrator: nil workspace catalog")
	}
	return &WorkspaceCatalogHandler{store: store, dispatcher: dispatcher}, nil
}

// Create implements WorkspaceCatalogService.Create. The
// caller-supplied name is required; the orchestrator mints the
// UUID and stamps the owner_fingerprint from the mTLS peer.
func (h *WorkspaceCatalogHandler) Create(
	_ context.Context,
	req *connect.Request[workspacev1.CreateWorkspaceRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: name is required"))
	}
	ws, err := h.store.Create(workspacecatalog.CreateInput{
		UUID:             mintWorkspaceUUID(),
		Name:             name,
		OwnerFingerprint: fp,
	})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(ws),
	}), nil
}

// Get resolves the handle and returns the workspace. Name lookups
// are scoped to the caller's fingerprint.
func (h *WorkspaceCatalogHandler) Get(
	_ context.Context,
	req *connect.Request[workspacev1.GetWorkspaceRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(ws),
	}), nil
}

// List returns the catalog snapshot, optionally filtered to the
// caller's own workspaces. The filter is pushed into Store.List so
// records the caller can't see never get copied out.
func (h *WorkspaceCatalogHandler) List(
	_ context.Context,
	req *connect.Request[workspacev1.ListWorkspacesRequest],
) (*connect.Response[workspacev1.ListWorkspacesResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	var keep func(*workspacecatalog.Workspace) bool
	if req.Msg.GetMineOnly() {
		keep = func(w *workspacecatalog.Workspace) bool { return w.OwnerFingerprint == fp }
	}
	all := h.store.List(keep)
	out := make([]*workspacev1.Workspace, len(all))
	for i := range all {
		out[i] = workspaceToProto(all[i])
	}
	return connect.NewResponse(&workspacev1.ListWorkspacesResponse{
		Workspaces: out,
	}), nil
}

// SetImage replaces the workspace's image_ref. Empty value clears
// the field.
func (h *WorkspaceCatalogHandler) SetImage(
	_ context.Context,
	req *connect.Request[workspacev1.SetImageRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	ref := req.Msg.GetImageRef()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			w.ImageRef = ref
			return nil
		})
}

// SetTarget pins the workspace to a sandbox-host fingerprint, or
// clears the pin when the value is empty.
func (h *WorkspaceCatalogHandler) SetTarget(
	_ context.Context,
	req *connect.Request[workspacev1.SetTargetRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	target := req.Msg.GetSandboxFingerprint()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			w.TargetSandboxFingerprint = target
			return nil
		})
}

// mutate is the shared auth → resolve → Update → project plumbing
// behind every per-field setter. Add* handlers don't use it because
// they need pre-resolve input validation that doesn't fit a generic
// helper.
func (h *WorkspaceCatalogHandler) mutate(
	header http.Header,
	handle *workspacev1.WorkspaceHandle,
	mut func(*workspacecatalog.Workspace) error,
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(header)
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, handle)
	if err != nil {
		return nil, err
	}
	updated, err := h.store.Update(ws.UUID, mut)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// AddMount appends a mount to the workspace. Validates absolute
// target + non-duplicate before touching the store.
func (h *WorkspaceCatalogHandler) AddMount(
	_ context.Context,
	req *connect.Request[workspacev1.AddMountRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	mount := req.Msg.GetMount()
	if mount == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: mount is required"))
	}
	target := mount.GetTarget()
	if target == "" || target[0] != '/' {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("workspace: mount target %q must be absolute", target))
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	source := mount.GetSource()
	if source == "" {
		// Empty Source defaults to the workspace's own UUID — the
		// daft-fuse view of the workspace itself. Default applied
		// server-side so the client doesn't need an extra Get RPC
		// (and a TOCTOU window) just to learn its own UUID.
		source = ws.UUID
	}
	updated, err := h.store.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		for i := range w.Mounts {
			if w.Mounts[i].Target == target {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: mount target %q already present", target))
			}
		}
		w.Mounts = append(w.Mounts, workspacecatalog.Mount{
			Target:   target,
			Source:   source,
			ReadOnly: mount.GetReadOnly(),
		})
		return nil
	})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// AddEnv appends an env var. Empty name + duplicate names rejected.
func (h *WorkspaceCatalogHandler) AddEnv(
	_ context.Context,
	req *connect.Request[workspacev1.AddEnvRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	env := req.Msg.GetEnv()
	if env == nil || env.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: env.name is required"))
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	name := env.GetName()
	updated, err := h.store.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		for i := range w.Env {
			if w.Env[i].Name == name {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: env %q already set", name))
			}
		}
		w.Env = append(w.Env, workspacecatalog.EnvVar{
			Name:  name,
			Value: env.GetValue(),
		})
		return nil
	})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// AddPort appends a PortMapping. UNSPECIFIED protocol is rejected
// so Phase C ingress can shape correctly.
func (h *WorkspaceCatalogHandler) AddPort(
	_ context.Context,
	req *connect.Request[workspacev1.AddPortRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	port := req.Msg.GetPort()
	if port == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: port is required"))
	}
	if port.GetProtocol() == sandboxv1.Protocol_PROTOCOL_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: port.protocol must be set"))
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	containerPort := port.GetContainerPort()
	proto := int32(port.GetProtocol())
	updated, err := h.store.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		for i := range w.Ports {
			if w.Ports[i].ContainerPort == containerPort &&
				w.Ports[i].Protocol == proto {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: port %d/%s already mapped",
						containerPort, port.GetProtocol()))
			}
		}
		w.Ports = append(w.Ports, workspacecatalog.PortMapping{
			ContainerPort: containerPort,
			Protocol:      proto,
		})
		return nil
	})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// removeMatching drops every element of s for which match returns
// true, returning the new slice and whether anything changed.
// Add* handlers enforce uniqueness so at most one element matches —
// the "every" semantic from slices.DeleteFunc is safe.
func removeMatching[T any](s []T, match func(T) bool) ([]T, bool) {
	out := slices.DeleteFunc(s, match)
	return out, len(out) != len(s)
}

// RemoveMount drops the mount with matching target. NotFound when
// no mount has that target — clients can distinguish a missing
// workspace (also NotFound, but on the handle resolution) from a
// missing mount by checking the error message.
//
//nolint:dupl // RemoveMount/RemoveEnv/RemovePort are structurally parallel by design; removeMatching factors the loop body, the residual frame is essential.
func (h *WorkspaceCatalogHandler) RemoveMount(
	_ context.Context,
	req *connect.Request[workspacev1.RemoveMountRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	target := req.Msg.GetTarget()
	if target == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: target is required"))
	}
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			next, ok := removeMatching(w.Mounts, func(m workspacecatalog.Mount) bool {
				return m.Target == target
			})
			if !ok {
				return connect.NewError(connect.CodeNotFound,
					fmt.Errorf("workspace: no mount with target %q", target))
			}
			w.Mounts = next
			return nil
		})
}

// RemoveEnv drops the env var with the given name.
//
//nolint:dupl // RemoveMount/RemoveEnv/RemovePort are structurally parallel by design; removeMatching factors the loop body, the residual frame is essential.
func (h *WorkspaceCatalogHandler) RemoveEnv(
	_ context.Context,
	req *connect.Request[workspacev1.RemoveEnvRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: name is required"))
	}
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			next, ok := removeMatching(w.Env, func(e workspacecatalog.EnvVar) bool {
				return e.Name == name
			})
			if !ok {
				return connect.NewError(connect.CodeNotFound,
					fmt.Errorf("workspace: no env %q", name))
			}
			w.Env = next
			return nil
		})
}

// RemovePort drops the (container_port, protocol) mapping.
func (h *WorkspaceCatalogHandler) RemovePort(
	_ context.Context,
	req *connect.Request[workspacev1.RemovePortRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	if req.Msg.GetProtocol() == sandboxv1.Protocol_PROTOCOL_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: protocol must be set"))
	}
	port := req.Msg.GetContainerPort()
	proto := int32(req.Msg.GetProtocol())
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			next, ok := removeMatching(w.Ports, func(p workspacecatalog.PortMapping) bool {
				return p.ContainerPort == port && p.Protocol == proto
			})
			if !ok {
				return connect.NewError(connect.CodeNotFound,
					fmt.Errorf("workspace: no port %d/%s", port, req.Msg.GetProtocol()))
			}
			w.Ports = next
			return nil
		})
}

// resolveHandle turns a WorkspaceHandle into a workspace, scoping
// name lookups to ownerFP. Returns Connect errors so callers can
// surface them directly.
func (h *WorkspaceCatalogHandler) resolveHandle(
	ownerFP identity.Fingerprint,
	handle *workspacev1.WorkspaceHandle,
) (workspacecatalog.Workspace, error) {
	if handle == nil {
		return workspacecatalog.Workspace{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: handle is required"))
	}
	switch t := handle.GetHandle().(type) {
	case *workspacev1.WorkspaceHandle_Uuid:
		if t.Uuid == "" {
			return workspacecatalog.Workspace{}, connect.NewError(connect.CodeInvalidArgument,
				errors.New("workspace: handle.uuid is empty"))
		}
		ws, err := h.store.Get(t.Uuid)
		return ws, mapCatalogErr(err)
	case *workspacev1.WorkspaceHandle_Name:
		if t.Name == "" {
			return workspacecatalog.Workspace{}, connect.NewError(connect.CodeInvalidArgument,
				errors.New("workspace: handle.name is empty"))
		}
		ws, err := h.store.LookupByAlias(ownerFP, t.Name)
		return ws, mapCatalogErr(err)
	default:
		return workspacecatalog.Workspace{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: handle.uuid or handle.name required"))
	}
}

// Validate recomputes readiness and auto-transitions DRAFT↔READY.
// Other statuses are left untouched.
func (h *WorkspaceCatalogHandler) Validate(
	_ context.Context,
	req *connect.Request[workspacev1.ValidateRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			// Store.Update recomputes Missing[] before we see this
			// closure return, so the readiness flip can read straight
			// off the current field values.
			ready := w.ImageRef != "" || w.PendingBuildID != ""
			ready = ready && w.TargetSandboxFingerprint != "" && len(w.Mounts) > 0
			switch w.Status {
			case workspacecatalog.StatusDraft:
				if ready {
					w.Status = workspacecatalog.StatusReady
				}
			case workspacecatalog.StatusReady:
				if !ready {
					w.Status = workspacecatalog.StatusDraft
				}
			}
			return nil
		})
}

// Launch composes a SandboxSpec from the workspace and ships it via
// the configured Dispatcher. Returns FailedPrecondition for state-
// machine / readiness violations; Unavailable when the sandbox is
// momentarily backlogged; Unimplemented when no dispatcher is
// wired (catalog-only test rigs).
func (h *WorkspaceCatalogHandler) Launch(
	_ context.Context,
	req *connect.Request[workspacev1.LaunchRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	if h.dispatcher == nil {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("orchestrator: no dispatcher configured"))
	}
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	if !launchableStatus(ws.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: cannot launch from status %s", ws.Status))
	}
	if len(ws.Missing) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: not ready, missing: %v", ws.Missing))
	}
	if ws.TargetSandboxFingerprint == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("workspace: target_sandbox_fingerprint required for Launch"))
	}
	targetFP, err := identity.ParseFingerprint(ws.TargetSandboxFingerprint)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: invalid target fingerprint: %w", err))
	}

	sandboxUID := mintWorkspaceUUID()
	sandbox := workspaceToSandbox(ws, sandboxUID, targetFP)
	if pubErr := h.dispatcher.PublishSandboxEvent(targetFP, sandboxv1.EventType_EVENT_TYPE_ADDED, sandbox); pubErr != nil {
		switch {
		case errors.Is(pubErr, ErrNodeNotConnected):
			return nil, connect.NewError(connect.CodeFailedPrecondition, pubErr)
		case errors.Is(pubErr, ErrSandboxEventBacklog):
			return nil, connect.NewError(connect.CodeUnavailable, pubErr)
		}
		return nil, connect.NewError(connect.CodeInternal, pubErr)
	}

	updated, err := h.store.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		w.Status = workspacecatalog.StatusRunning
		w.CurrentSandbox = &workspacecatalog.SandboxRef{
			UID:             sandboxUID,
			NodeFingerprint: targetFP,
		}
		return nil
	})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// Stop transitions a RUNNING or FROZEN workspace to STOPPED and
// clears the current_sandbox ref. Catalog-only — the wire teardown
// signal to the Node (a SandboxWatchEvent DELETED) follows in B5
// alongside the reconciler.
func (h *WorkspaceCatalogHandler) Stop(
	_ context.Context,
	req *connect.Request[workspacev1.StopRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			if w.Status != workspacecatalog.StatusRunning &&
				w.Status != workspacecatalog.StatusFrozen {
				return connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("workspace: cannot stop from status %s", w.Status))
			}
			w.Status = workspacecatalog.StatusStopped
			w.CurrentSandbox = nil
			return nil
		})
}

// launchableStatus reports whether a workspace's current status
// admits a Launch (READY or STOPPED). DRAFT requires Validate
// first; RUNNING/FROZEN are already-running; ARCHIVED is terminal.
func launchableStatus(s workspacecatalog.Status) bool {
	return s == workspacecatalog.StatusReady || s == workspacecatalog.StatusStopped
}

// workspaceToSandboxSpec composes the wire workload spec that the
// Node's executor consumes. The caller wraps this in a Sandbox
// resource (with metadata + status) via workspaceToSandbox.
func workspaceToSandboxSpec(ws workspacecatalog.Workspace, nodeFP identity.Fingerprint) *sandboxv1.SandboxSpec {
	return &sandboxv1.SandboxSpec{
		ImageRef:      ws.ImageRef,
		WorkspaceUuid: ws.UUID,
		Node:          &sandboxv1.NodeRef{Fingerprint: nodeFP.String()},
		Mounts:        mountsToProto(ws.Mounts),
		Env:           envToProto(ws.Env),
		Ports:         portsToProto(ws.Ports),
	}
}

// mountsToProto / envToProto / portsToProto project the catalog's
// Go-native slices onto their wire forms. Shared by workspaceToProto
// (catalog → CLI) and workspaceToSandboxSpec (catalog → Node).
func mountsToProto(in []workspacecatalog.Mount) []*sandboxv1.Mount {
	out := make([]*sandboxv1.Mount, len(in))
	for i, m := range in {
		out[i] = &sandboxv1.Mount{
			Target:   m.Target,
			Source:   m.Source,
			ReadOnly: m.ReadOnly,
		}
	}
	return out
}

func envToProto(in []workspacecatalog.EnvVar) []*sandboxv1.EnvVar {
	out := make([]*sandboxv1.EnvVar, len(in))
	for i, e := range in {
		out[i] = &sandboxv1.EnvVar{Name: e.Name, Value: e.Value}
	}
	return out
}

func portsToProto(in []workspacecatalog.PortMapping) []*sandboxv1.PortMapping {
	out := make([]*sandboxv1.PortMapping, len(in))
	for i, p := range in {
		out[i] = &sandboxv1.PortMapping{
			ContainerPort: p.ContainerPort,
			Protocol:      sandboxv1.Protocol(p.Protocol),
		}
	}
	return out
}

// workspaceToSandbox composes the full Sandbox resource Launch
// publishes to the Node. uid is the minted RFC-4122 v4; the
// workspace ref + node ref + spec are populated from the workspace
// snapshot. Initial status.phase is "Pending" — the Node will
// transition it to Running once the VM boots (B4d).
func workspaceToSandbox(ws workspacecatalog.Workspace, uid string, nodeFP identity.Fingerprint) *sandboxv1.Sandbox {
	now := timestamppb.Now()
	return &sandboxv1.Sandbox{
		Metadata: &sandboxv1.SandboxMetadata{
			Uid: uid,
			Workspace: &sandboxv1.WorkspaceRef{
				Uuid: ws.UUID,
				Name: ws.Name,
			},
			CreatedAt: now,
		},
		Spec: workspaceToSandboxSpec(ws, nodeFP),
		Status: &sandboxv1.SandboxStatus{
			Phase:              sandboxPhasePending,
			LastTransitionTime: now,
		},
	}
}

// sandboxPhasePending is the PascalCase phase token assigned when
// the orchestrator mints a Sandbox before the Node has reported
// liveness for it. See proto/sandbox/v1/sandbox.proto.
const sandboxPhasePending = "Pending"

// mapCatalogErr maps workspacecatalog sentinel errors to Connect
// codes. Returns nil unchanged so handlers can use it
// unconditionally on every store-call result.
func mapCatalogErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspacecatalog.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, workspacecatalog.ErrAliasInUse):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, workspacecatalog.ErrImmutableField):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	return connect.NewError(connect.CodeInternal, err)
}

// mintWorkspaceUUID returns 16 random bytes formatted as a
// canonical RFC-4122 v4 UUID. We don't bring in google/uuid for
// this one call site — the trace package already uses the same
// crypto/rand idiom for trace/span IDs.
func mintWorkspaceUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on Linux is documented infallible.
		// Loud panic is the only sane response — silently
		// emitting a duplicate UUID would corrupt the catalog
		// invariants we just spent B1 enforcing.
		//nolint:forbidigo // unrecoverable: duplicate UUIDs would collide in the catalog; loud fail is correct.
		panic(fmt.Sprintf("workspacecatalog: rand.Read: %v", err))
	}
	// Version + variant per RFC-4122 §4.4.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := hex.EncodeToString(b[:])
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:32]
}

// workspaceToProto projects a catalog Workspace onto its wire form.
func workspaceToProto(ws workspacecatalog.Workspace) *workspacev1.Workspace {
	out := &workspacev1.Workspace{
		Uuid:                     ws.UUID,
		Name:                     ws.Name,
		OwnerFingerprint:         fingerprintStringOrEmpty(ws.OwnerFingerprint),
		ImageRef:                 ws.ImageRef,
		PendingBuildId:           ws.PendingBuildID,
		TargetSandboxFingerprint: ws.TargetSandboxFingerprint,
		LockOwnerFingerprint:     fingerprintStringOrEmpty(ws.LockOwnerFingerprint),
		Status:                   statusToProto(ws.Status),
		Missing:                  append([]string(nil), ws.Missing...),
		CreatedAt:                timestamppb.New(ws.CreatedAt),
		UpdatedAt:                timestamppb.New(ws.UpdatedAt),
		CurrentSandbox:           sandboxRefToProto(ws.CurrentSandbox),
	}
	if !ws.LockExpiresAt.IsZero() {
		out.LockExpiresAt = timestamppb.New(ws.LockExpiresAt)
	}
	out.Mounts = mountsToProto(ws.Mounts)
	out.Env = envToProto(ws.Env)
	out.Ports = portsToProto(ws.Ports)
	return out
}

// sandboxRefToProto projects a catalog SandboxRef onto the wire
// shape. Nil maps to nil so a never-launched workspace serialises
// with an unset current_sandbox field.
func sandboxRefToProto(ref *workspacecatalog.SandboxRef) *sandboxv1.SandboxRef {
	if ref == nil {
		return nil
	}
	return &sandboxv1.SandboxRef{
		Uid:  ref.UID,
		Node: &sandboxv1.NodeRef{Fingerprint: ref.NodeFingerprint.String()},
	}
}

// fingerprintStringOrEmpty renders a Fingerprint as its hex string,
// or "" when the value is zero. Thin wrapper because the catalog →
// proto projection needs the empty form, but most other callers
// rely on String() always returning a valid-looking hash.
func fingerprintStringOrEmpty(fp identity.Fingerprint) string {
	if fp.IsZero() {
		return ""
	}
	return fp.String()
}

// statusToProto maps the catalog's Go status enum onto the proto
// WorkspaceStatus values. The two are intentionally kept distinct
// (see the workspacecatalog package doc) so the catalog isn't
// coupled to generated code.
func statusToProto(s workspacecatalog.Status) workspacev1.WorkspaceStatus {
	switch s {
	case workspacecatalog.StatusDraft:
		return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_DRAFT
	case workspacecatalog.StatusReady:
		return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_READY
	case workspacecatalog.StatusRunning:
		return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_RUNNING
	case workspacecatalog.StatusFrozen:
		return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_FROZEN
	case workspacecatalog.StatusStopped:
		return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_STOPPED
	case workspacecatalog.StatusArchived:
		return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_ARCHIVED
	}
	return workspacev1.WorkspaceStatus_WORKSPACE_STATUS_UNSPECIFIED
}
