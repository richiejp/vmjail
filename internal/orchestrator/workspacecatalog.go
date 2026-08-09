package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	pathpkg "path"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
	"github.com/richiejp/darkaifactory/internal/workspacedisk"
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

// WorkspaceNodeLister is the seam Launch uses to auto-pin a target
// when a workspace declares none and the cluster has exactly one
// registered node — the single-node-cluster convenience that lets
// `daft workspace launch` (and `up`) work without a manual
// `daft workspace target`. Production impl is *nodectrl.Registry
// (RegisteredFingerprints); nil disables auto-pinning, leaving
// "target" a required field.
type WorkspaceNodeLister interface {
	RegisteredFingerprints() []identity.Fingerprint
}

// AgentSandboxPolicy is the immutable AgentProfile subset that affects VM
// composition. It is resolved on the trusted host and is never accepted from
// the public MCP schema.
type AgentSandboxPolicy struct {
	ProfileDigest      string
	ImageRef           string
	ResourcesEnable    bool
	CPUCores           uint32
	MemoryBytes        uint64
	EgressEnable       bool
	CredentialProvider string
}

// WorkspacePortBridger is the seam ForwardPort uses to open a
// labeled byte channel into the Node bound to a Workspace. The
// production implementation is *NodeControlHandler (its
// OpenPortBridge method); tests inject fakes that return a
// pre-paired net.Conn so the splice loop can be exercised
// without standing up a real sandboxd.
type WorkspacePortBridger interface {
	OpenPortBridge(ctx context.Context, fp identity.Fingerprint, workspaceUUID string, containerPort uint32) (net.Conn, error)
}

// WorkspaceAttachBridger is the seam AttachSandbox uses to open
// an attach session into the Sandbox bound to a Workspace. The
// production implementation is *NodeControlHandler (its
// OpenAttachBridge method); tests inject fakes that return a
// pre-paired sandboxgateway.AttachSession.
type WorkspaceAttachBridger interface {
	OpenAttachBridge(ctx context.Context, fp identity.Fingerprint, req sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error)
}

// WorkspaceCatalogHandler adapts the workspacecatalog.Store to the
// Connect-RPC WorkspaceCatalogService surface. Peer identity for
// every RPC is the mTLS leaf-cert fingerprint surfaced via
// withPeerMiddleware. For Slice B2a, any authenticated caller can
// mutate any workspace — the per-owner lock subsystem that gates
// mutation lands in B2d.
type WorkspaceCatalogHandler struct {
	workspacev1connect.UnimplementedWorkspaceCatalogServiceHandler
	store           *workspacecatalog.Store
	dispatcher      Dispatcher
	nodes           WorkspaceNodeLister
	preAction       preActionReconciler
	portBridger     WorkspacePortBridger
	attachBridger   WorkspaceAttachBridger
	observerBridger WorkspaceObserverBridger
	lifecycleMu     sync.Mutex
	lifecycleLocks  map[string]*workspaceLifecycleLock
}

type workspaceLifecycleLock struct {
	mu   sync.Mutex
	refs uint64
}

// SetNodeLister wires the registry Launch consults to auto-pin the
// sole node as a workspace's target on a single-node cluster. Nil
// (the default) leaves "target" a required field.
func (h *WorkspaceCatalogHandler) SetNodeLister(l WorkspaceNodeLister) {
	h.nodes = l
}

// SetPortBridger wires the WorkspacePortBridger ForwardPort calls
// route to. Nil ⇒ ForwardPort returns Unimplemented, the right
// shape for catalog-only test rigs that don't exercise ingress.
func (h *WorkspaceCatalogHandler) SetPortBridger(b WorkspacePortBridger) {
	h.portBridger = b
}

// SetAttachBridger wires the WorkspaceAttachBridger AttachSandbox
// calls route to. Nil ⇒ AttachSandbox returns Unimplemented.
func (h *WorkspaceCatalogHandler) SetAttachBridger(b WorkspaceAttachBridger) {
	h.attachBridger = b
}

// NewWorkspaceCatalogHandler returns a handler backed by store and
// dispatcher. dispatcher may be nil — Launch will then refuse with
// Unimplemented, which is the right shape for catalog-only test
// rigs that exercise the read/mutate surface in isolation.
func NewWorkspaceCatalogHandler(store *workspacecatalog.Store, dispatcher Dispatcher) (*WorkspaceCatalogHandler, error) {
	if store == nil {
		return nil, errors.New("orchestrator: nil workspace catalog")
	}
	return &WorkspaceCatalogHandler{
		store: store, dispatcher: dispatcher,
		lifecycleLocks: make(map[string]*workspaceLifecycleLock),
	}, nil
}

func (h *WorkspaceCatalogHandler) lockLifecycle(workspaceUUID string) func() {
	h.lifecycleMu.Lock()
	entry := h.lifecycleLocks[workspaceUUID]
	if entry == nil {
		entry = &workspaceLifecycleLock{}
		h.lifecycleLocks[workspaceUUID] = entry
	}
	entry.refs++
	h.lifecycleMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		h.lifecycleMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(h.lifecycleLocks, workspaceUUID)
		}
		h.lifecycleMu.Unlock()
	}
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
		keep = func(w *workspacecatalog.Workspace) bool { return w.Metadata.OwnerFingerprint == fp }
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

// Apply atomically replaces the complete declared Workspace spec. Ownership,
// lock state, equality, and the generation precondition are checked together
// by the store so concurrent overseers cannot overwrite each other.
func (h *WorkspaceCatalogHandler) Apply(
	_ context.Context,
	req *connect.Request[workspacev1.ApplyWorkspaceRequest],
) (*connect.Response[workspacev1.ApplyWorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	handle := req.Msg.GetHandle()
	if handle == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: handle is required"))
	}
	desired, err := workspaceSpecFromProto(req.Msg.GetSpec())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	var updated workspacecatalog.Workspace
	var created, changed bool
	switch value := handle.GetHandle().(type) {
	case *workspacev1.WorkspaceHandle_Name:
		if value.Name == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: handle.name is empty"))
		}
		updated, created, changed, err = h.store.ApplySpecByName(
			mintWorkspaceUUID(), value.Name, fp, req.Msg.GetExpectedGeneration(), desired,
		)
	case *workspacev1.WorkspaceHandle_Uuid:
		if value.Uuid == "" || req.Msg.GetExpectedGeneration() == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("workspace: non-empty handle.uuid and expected_generation are required"))
		}
		updated, changed, err = h.store.ApplySpec(value.Uuid, fp, req.Msg.GetExpectedGeneration(), desired)
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: handle.uuid or handle.name required"))
	}
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.ApplyWorkspaceResponse{
		Workspace: workspaceToProto(updated),
		Changed:   changed,
		Created:   created,
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
			w.Spec.ImageRef = ref
			return nil
		})
}

// SetDevcontainer records the path to a devcontainer.json within the
// workspace content, or clears it when empty. Setting it makes the
// workspace launchable (it provisionally satisfies the image+mount
// requirement, see missingFields); the node reads and overlays it from
// the materialised content at dispatch (slice 2d).
func (h *WorkspaceCatalogHandler) SetDevcontainer(
	_ context.Context,
	req *connect.Request[workspacev1.SetDevcontainerRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	path := req.Msg.GetDevcontainerPath()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			w.Spec.DevcontainerPath = path
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
			w.Spec.TargetSandboxFingerprint = target
			return nil
		})
}

// SetResources records the guest VM's RAM/vCPU sizing. Zero in
// either field reverts it to the executor default.
func (h *WorkspaceCatalogHandler) SetResources(
	_ context.Context,
	req *connect.Request[workspacev1.SetResourcesRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	memBytes := req.Msg.GetMemoryBytes()
	cpuCores := req.Msg.GetCpuCores()
	diskBytes := req.Msg.GetDiskBytes()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			oldDiskBytes := w.Spec.DiskBytes
			if oldDiskBytes == 0 {
				oldDiskBytes = workspacedisk.DefaultDiskBytes
			}
			newDiskBytes := diskBytes
			if newDiskBytes == 0 {
				newDiskBytes = workspacedisk.DefaultDiskBytes
			}
			if w.Status.CurrentSandbox != nil && newDiskBytes < oldDiskBytes {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
					"workspace: live disk cannot shrink: current=%d requested=%d", oldDiskBytes, newDiskBytes,
				))
			}
			liveDiskOnlyGrowth := w.Status.CurrentSandbox != nil && newDiskBytes > oldDiskBytes &&
				w.Spec.MemoryBytes == memBytes && w.Spec.CPUCores == cpuCores
			w.Spec.MemoryBytes = memBytes
			w.Spec.CPUCores = cpuCores
			w.Spec.DiskBytes = diskBytes
			if liveDiskOnlyGrowth {
				// The public client commits SetResources only after the Node has
				// resized qcow2, virtio-blk, and XFS. MutateSpec increments the
				// generation after this callback, so stamp that exact generation.
				w.Status.CurrentSandbox.WorkspaceGeneration = w.Metadata.Generation + 1
			}
			return nil
		})
}

// SetNestedVirtualization toggles the workspace's nested-virt grant.
// Enabling binds /dev/kvm into the workload container at Launch;
// disabling reverts to the unprivileged default.
func (h *WorkspaceCatalogHandler) SetNestedVirtualization(
	_ context.Context,
	req *connect.Request[workspacev1.SetNestedVirtualizationRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	enable := req.Msg.GetEnable()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			w.Spec.NestedVirt = enable
			return nil
		})
}

// SetPrivilegedFuseMounts toggles the workspace's privileged-FUSE
// grant. Enabling binds /dev/fuse into the workload container and adds
// CAP_SYS_ADMIN at Launch; disabling reverts to the unprivileged
// default.
func (h *WorkspaceCatalogHandler) SetPrivilegedFuseMounts(
	_ context.Context,
	req *connect.Request[workspacev1.SetPrivilegedFuseMountsRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	enable := req.Msg.GetEnable()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			w.Spec.PrivilegedFuseMounts = enable
			return nil
		})
}

// SetPrivilegedBuild toggles the workspace's build capability grant.
// Enabling gives the workload container the standard container cap set at
// Launch (so an in-sandbox buildah can build); disabling reverts to the
// zero-cap default.
func (h *WorkspaceCatalogHandler) SetPrivilegedBuild(
	_ context.Context,
	req *connect.Request[workspacev1.SetPrivilegedBuildRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	enable := req.Msg.GetEnable()
	return h.mutate(req.Header(), req.Msg.GetHandle(),
		func(w *workspacecatalog.Workspace) error {
			w.Spec.PrivilegedBuild = enable
			return nil
		})
}

// mutate is the shared auth → resolve → Update → project plumbing
// behind every per-field setter. Defaults to MutateSpec (bumps
// generation + recomputes Missing/Ready). Add* handlers don't use
// it because they need pre-resolve input validation that doesn't
// fit a generic helper.
func (h *WorkspaceCatalogHandler) mutate(
	header http.Header,
	handle *workspacev1.WorkspaceHandle,
	mut func(*workspacecatalog.Workspace) error,
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	return h.mutateKind(header, handle, workspacecatalog.MutateSpec, mut)
}

// mutateKind is mutate with an explicit MutateKind so handlers
// touching Status (Launch, Stop) don't accidentally bump the
// workspace's spec generation. Gates on the workspace's write
// lock: a held lock owned by another fingerprint refuses
// mutation with FailedPrecondition (reason LockHeld). Unheld /
// expired / self-held locks pass.
func (h *WorkspaceCatalogHandler) mutateKind(
	header http.Header,
	handle *workspacev1.WorkspaceHandle,
	kind workspacecatalog.MutateKind,
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
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	updated, err := h.store.Update(ws.Metadata.UUID, kind, mut)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// guardLock refuses mutation when the workspace is locked by a
// different caller. Unheld or expired locks fall through. Hoisted
// so the Add* handlers (which don't use mutateKind) can call it
// after their own pre-resolve input validation.
func guardLock(ws workspacecatalog.Workspace, fp identity.Fingerprint) error {
	if ws.Status.Lock.HeldByOther(fp, time.Now()) {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: locked by %s", ws.Status.Lock.OwnerFingerprint))
	}
	return nil
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
	unlock := h.lockLifecycle(ws.Metadata.UUID)
	defer unlock()
	ws, err = h.store.Get(ws.Metadata.UUID)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	source := mount.GetSource()
	if source == "" {
		// Empty Source defaults to the workspace's own UUID — the
		// daft-fuse view of the workspace itself. Default applied
		// server-side so the client doesn't need an extra Get RPC
		// (and a TOCTOU window) just to learn its own UUID.
		source = ws.Metadata.UUID
	}
	updated, err := h.store.Update(ws.Metadata.UUID, workspacecatalog.MutateSpec, func(w *workspacecatalog.Workspace) error {
		for i := range w.Spec.Mounts {
			if w.Spec.Mounts[i].Target == target {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: mount target %q already present", target))
			}
		}
		w.Spec.Mounts = append(w.Spec.Mounts, workspacecatalog.Mount{
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
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	name := env.GetName()
	updated, err := h.store.Update(ws.Metadata.UUID, workspacecatalog.MutateSpec, func(w *workspacecatalog.Workspace) error {
		for i := range w.Spec.Env {
			if w.Spec.Env[i].Name == name {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: env %q already set", name))
			}
		}
		w.Spec.Env = append(w.Spec.Env, workspacecatalog.EnvVar{
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
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	containerPort := port.GetContainerPort()
	proto := int32(port.GetProtocol())
	updated, err := h.store.Update(ws.Metadata.UUID, workspacecatalog.MutateSpec, func(w *workspacecatalog.Workspace) error {
		for i := range w.Spec.Ports {
			if w.Spec.Ports[i].ContainerPort == containerPort &&
				w.Spec.Ports[i].Protocol == proto {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: port %d/%s already mapped",
						containerPort, port.GetProtocol()))
			}
		}
		w.Spec.Ports = append(w.Spec.Ports, workspacecatalog.PortMapping{
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
			next, ok := removeMatching(w.Spec.Mounts, func(m workspacecatalog.Mount) bool {
				return m.Target == target
			})
			if !ok {
				return connect.NewError(connect.CodeNotFound,
					fmt.Errorf("workspace: no mount with target %q", target))
			}
			w.Spec.Mounts = next
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
			next, ok := removeMatching(w.Spec.Env, func(e workspacecatalog.EnvVar) bool {
				return e.Name == name
			})
			if !ok {
				return connect.NewError(connect.CodeNotFound,
					fmt.Errorf("workspace: no env %q", name))
			}
			w.Spec.Env = next
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
			next, ok := removeMatching(w.Spec.Ports, func(p workspacecatalog.PortMapping) bool {
				return p.ContainerPort == port && p.Protocol == proto
			})
			if !ok {
				return connect.NewError(connect.CodeNotFound,
					fmt.Errorf("workspace: no port %d/%s", port, req.Msg.GetProtocol()))
			}
			w.Spec.Ports = next
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
		if err == nil && ws.Metadata.OwnerFingerprint != ownerFP {
			err = workspacecatalog.ErrNotFound
		}
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

// Validate returns the store-derived readiness projection without mutating the
// spec, timestamps, or generation. Every spec mutation derives readiness in
// the same atomic store transaction, so an extra write here would only create
// false generation drift.
func (h *WorkspaceCatalogHandler) Validate(
	_ context.Context,
	req *connect.Request[workspacev1.ValidateRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: workspaceToProto(ws)}), nil
}

// Launch composes a Sandbox resource from the workspace and ships
// it via the configured Dispatcher. Returns FailedPrecondition for
// readiness / already-dispatched violations; Unavailable when the
// Node is momentarily backlogged; Unimplemented when no dispatcher
// is wired (catalog-only test rigs).
func (h *WorkspaceCatalogHandler) Launch(
	ctx context.Context,
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
	unlock := h.lockLifecycle(ws.Metadata.UUID)
	defer unlock()
	ws, err = h.store.Get(ws.Metadata.UUID)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	h.reconcileBefore(ws.Metadata.UUID)
	ws, err = h.store.Get(ws.Metadata.UUID)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	// Persist automatic node selection only after publication succeeds.
	autoTarget, autoSelected := h.autoSelectSoleNode(ws)
	if !ws.Status.IsTrue(workspacecatalog.ConditionReady) && !autoSelected {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: not ready, missing: %v", ws.Status.Missing))
	}
	dispatched := ws.Status.IsTrue(workspacecatalog.ConditionDispatched)
	if dispatched && ws.Status.CurrentSandbox != nil {
		if ws.Status.CurrentSandbox.AgentProfileDigest != "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("workspace: current Sandbox belongs to an AgentRun; stop it before launching the Workspace"))
		}
		changes, stopWatching := h.store.Watch()
		defer stopWatching()
		return h.waitForSandboxReadyResponse(ctx, ws, changes)
	}
	if dispatched || ws.Status.CurrentSandbox != nil {
		return nil, connect.NewError(connect.CodeInternal,
			errors.New("workspace: inconsistent dispatched Sandbox binding"))
	}
	targetFP, err := launchTarget(ws, autoTarget, autoSelected)
	if err != nil {
		return nil, err
	}
	return h.launchNewSandbox(ctx, ws, targetFP, autoSelected)
}

func (h *WorkspaceCatalogHandler) launchNewSandbox(
	ctx context.Context,
	ws workspacecatalog.Workspace,
	targetFP identity.Fingerprint,
	autoSelected bool,
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	sandboxUID := mintWorkspaceUUID()
	sandbox := workspaceToSandbox(ws, sandboxUID, targetFP)
	changes, stopWatching := h.store.Watch()
	defer stopWatching()
	if pubErr := h.dispatcher.PublishSandboxEvent(targetFP, sandboxv1.EventType_EVENT_TYPE_ADDED, sandbox); pubErr != nil {
		return nil, mapSandboxPublishError(pubErr)
	}

	now := time.Now()
	mutation := workspacecatalog.MutateStatus
	if autoSelected {
		mutation = workspacecatalog.MutateSpec
	}
	updated, err := h.store.Update(ws.Metadata.UUID, mutation, func(w *workspacecatalog.Workspace) error {
		if autoSelected {
			w.Spec.TargetSandboxFingerprint = targetFP.String()
		}
		w.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
			UID:                 sandboxUID,
			NodeFingerprint:     targetFP,
			WorkspaceGeneration: ws.Metadata.Generation,
		}
		w.Status.SetCondition(
			workspacecatalog.ConditionDispatched,
			workspacecatalog.ConditionStatusTrue,
			"Launched", "",
			now,
		)
		w.Status.SetCondition(
			workspacecatalog.ConditionReconciled,
			workspacecatalog.ConditionStatusFalse,
			"LaunchPending", "",
			now,
		)
		return nil
	})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return h.waitForSandboxReadyResponse(ctx, updated, changes)
}

func (h *WorkspaceCatalogHandler) waitForSandboxReadyResponse(
	ctx context.Context,
	ws workspacecatalog.Workspace,
	changes <-chan uint64,
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ready, err := h.waitForSandboxReady(waitCtx, ws.Metadata.UUID, *ws.Status.CurrentSandbox, changes)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: workspaceToProto(ready)}), nil
}

func (h *WorkspaceCatalogHandler) waitForSandboxReady(
	ctx context.Context,
	workspaceUUID string,
	expected workspacecatalog.SandboxRef,
	changes <-chan uint64,
) (workspacecatalog.Workspace, error) {
	//daft:lint:disable=forloopbound reason="bounded by the 30s caller context; exits on exact running observation, terminal state, binding replacement, catalog error, or deadline"
	for {
		ws, err := h.store.Get(workspaceUUID)
		if err != nil {
			return workspacecatalog.Workspace{}, mapCatalogErr(err)
		}
		current := ws.Status.CurrentSandbox
		if current == nil {
			return workspacecatalog.Workspace{}, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("workspace: Sandbox %s terminated before becoming ready", expected.UID))
		}
		if current.UID != expected.UID || current.WorkspaceGeneration != expected.WorkspaceGeneration {
			return workspacecatalog.Workspace{}, connect.NewError(connect.CodeInternal,
				fmt.Errorf("workspace: Sandbox binding changed while waiting for %s readiness", expected.UID))
		}
		if ws.Status.ObservedGeneration == expected.WorkspaceGeneration &&
			ws.Status.IsTrue(workspacecatalog.ConditionReconciled) {
			return ws, nil
		}
		select {
		case <-ctx.Done():
			return workspacecatalog.Workspace{}, connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("workspace: timed out waiting for Sandbox %s readiness: %w", expected.UID, ctx.Err()))
		case <-changes:
		}
	}
}

func launchTarget(
	ws workspacecatalog.Workspace,
	autoTarget identity.Fingerprint,
	autoSelected bool,
) (identity.Fingerprint, error) {
	if autoSelected {
		return autoTarget, nil
	}
	target, err := identity.ParseFingerprint(ws.Spec.TargetSandboxFingerprint)
	if err != nil {
		return identity.Fingerprint{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: invalid target fingerprint: %w", err))
	}
	return target, nil
}

// LaunchAgentSandbox composes an agent-specific Sandbox without mutating the
// Workspace's project spec. An existing Sandbox is reusable only when it was
// launched from the exact same immutable AgentProfile digest.
func (h *WorkspaceCatalogHandler) LaunchAgentSandbox(owner identity.Fingerprint, workspaceUUID string, policy AgentSandboxPolicy) (workspacecatalog.SandboxRef, error) {
	if h.dispatcher == nil {
		return workspacecatalog.SandboxRef{}, connect.NewError(connect.CodeUnimplemented, errors.New("orchestrator: no dispatcher configured"))
	}
	if owner.IsZero() || workspaceUUID == "" || policy.ProfileDigest == "" || policy.ImageRef == "" {
		return workspacecatalog.SandboxRef{}, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: incomplete AgentProfile launch policy"))
	}
	unlock := h.lockLifecycle(workspaceUUID)
	defer unlock()
	ws, err := h.store.Get(workspaceUUID)
	if err != nil || ws.Metadata.OwnerFingerprint != owner {
		return workspacecatalog.SandboxRef{}, connect.NewError(connect.CodeNotFound, errors.New("orchestrator: Workspace not found"))
	}
	if err := guardLock(ws, owner); err != nil {
		return workspacecatalog.SandboxRef{}, err
	}
	h.reconcileBefore(workspaceUUID)
	ws, err = h.store.Get(workspaceUUID)
	if err != nil {
		return workspacecatalog.SandboxRef{}, mapCatalogErr(err)
	}
	if current := ws.Status.CurrentSandbox; current != nil {
		if current.AgentProfileDigest == policy.ProfileDigest {
			return *current, nil
		}
		return workspacecatalog.SandboxRef{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: Workspace current Sandbox was not launched from the requested AgentProfile; stop it before starting the agent"))
	}

	targetFP, autoSelected := h.autoSelectAgentNode(ws)
	if !autoSelected {
		targetFP, err = identity.ParseFingerprint(ws.Spec.TargetSandboxFingerprint)
		if err != nil {
			return workspacecatalog.SandboxRef{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("workspace: invalid target fingerprint: %w", err))
		}
	}
	missing := agentLaunchMissing(ws)
	missingOnlyAutoTarget := autoSelected && len(missing) == 1 && missing[0] == workspacecatalog.MissingTarget
	if len(missing) != 0 && !missingOnlyAutoTarget {
		return workspacecatalog.SandboxRef{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("workspace: not ready for agent launch, missing: %v", missing))
	}

	launch := ws
	launch.Spec.ImageRef = policy.ImageRef
	launch.Spec.PendingBuildID = ""
	if policy.ResourcesEnable {
		launch.Spec.CPUCores = policy.CPUCores
		launch.Spec.MemoryBytes = policy.MemoryBytes
	} else {
		launch.Spec.CPUCores = 0
		launch.Spec.MemoryBytes = 0
	}
	sandboxUID := mintWorkspaceUUID()
	sandbox := workspaceToSandbox(launch, sandboxUID, targetFP)
	sandbox.Spec.EgressPolicy = &sandboxv1.EgressPolicy{Enable: policy.EgressEnable}
	if policy.CredentialProvider != "" {
		sandbox.Spec.AgentCredential = &sandboxv1.AgentCredential{Provider: policy.CredentialProvider}
	}
	if pubErr := h.dispatcher.PublishSandboxEvent(targetFP, sandboxv1.EventType_EVENT_TYPE_ADDED, sandbox); pubErr != nil {
		return workspacecatalog.SandboxRef{}, mapSandboxPublishError(pubErr)
	}

	ref := workspacecatalog.SandboxRef{
		UID: sandboxUID, NodeFingerprint: targetFP, AgentProfileDigest: policy.ProfileDigest,
		WorkspaceGeneration: ws.Metadata.Generation,
	}
	mutation := workspacecatalog.MutateStatus
	if autoSelected {
		mutation = workspacecatalog.MutateSpec
	}
	_, err = h.store.Update(workspaceUUID, mutation, func(w *workspacecatalog.Workspace) error {
		if autoSelected {
			w.Spec.TargetSandboxFingerprint = targetFP.String()
		}
		w.Status.CurrentSandbox = &ref
		w.Status.SetCondition(workspacecatalog.ConditionDispatched, workspacecatalog.ConditionStatusTrue, "AgentLaunched", "", time.Now())
		return nil
	})
	if err != nil {
		return workspacecatalog.SandboxRef{}, mapCatalogErr(err)
	}
	return ref, nil
}

func (h *WorkspaceCatalogHandler) autoSelectAgentNode(ws workspacecatalog.Workspace) (identity.Fingerprint, bool) {
	if h.nodes == nil || ws.Spec.TargetSandboxFingerprint != "" {
		return identity.Fingerprint{}, false
	}
	fps := h.nodes.RegisteredFingerprints()
	if len(fps) != 1 {
		return identity.Fingerprint{}, false
	}
	return fps[0], true
}

func agentLaunchMissing(ws workspacecatalog.Workspace) []string {
	missing := slices.Clone(ws.Status.Missing)
	return slices.DeleteFunc(missing, func(field string) bool { return field == "image" })
}

func mapSandboxPublishError(err error) error {
	switch {
	case errors.Is(err, ErrNodeNotConnected):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrSandboxEventBacklog):
		return connect.NewError(connect.CodeUnavailable, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// autoSelectSoleNode returns the sole live node when target is the only
// readiness gap. Selection is deliberately side-effect free: Launch persists
// it atomically with the dispatched status only after publication succeeds.
func (h *WorkspaceCatalogHandler) autoSelectSoleNode(ws workspacecatalog.Workspace) (identity.Fingerprint, bool) {
	if h.nodes == nil || ws.Spec.TargetSandboxFingerprint != "" {
		return identity.Fingerprint{}, false
	}
	if len(ws.Status.Missing) != 1 || ws.Status.Missing[0] != workspacecatalog.MissingTarget {
		return identity.Fingerprint{}, false
	}
	fps := h.nodes.RegisteredFingerprints()
	if len(fps) != 1 {
		return identity.Fingerprint{}, false
	}
	return fps[0], true
}

// preActionReconciler is the seam Launch/Stop call before reading
// workspace status — ensures any pending threshold-crossed
// transitions (SandboxLost) are applied first. The production
// implementation is *reconciler.Reconciler; nil is acceptable
// (catalog-only test rigs).
type preActionReconciler interface {
	ReconcileBefore(uuid string)
}

// SetPreActionReconciler wires the optional pre-action reconcile
// hook. Called by Server.New after the reconciler is constructed.
func (h *WorkspaceCatalogHandler) SetPreActionReconciler(r preActionReconciler) {
	h.preAction = r
}

// reconcileBefore is a nil-safe helper. Internal-only.
func (h *WorkspaceCatalogHandler) reconcileBefore(uuid string) {
	if h.preAction != nil {
		h.preAction.ReconcileBefore(uuid)
	}
}

// Stop publishes an exact Sandbox DELETED event before clearing the binding.
// A publish failure leaves the catalog dispatched so a retry cannot silently
// strand a VM. The Node-side delete is idempotent.
func (h *WorkspaceCatalogHandler) Stop(
	ctx context.Context,
	req *connect.Request[workspacev1.StopRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	unlock := h.lockLifecycle(ws.Metadata.UUID)
	defer unlock()
	ws, err = h.store.Get(ws.Metadata.UUID)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	h.reconcileBefore(ws.Metadata.UUID)
	ws, err = h.store.Get(ws.Metadata.UUID)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	dispatched := ws.Status.IsTrue(workspacecatalog.ConditionDispatched)
	if !dispatched && ws.Status.CurrentSandbox == nil {
		return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: workspaceToProto(ws)}), nil
	}
	if !dispatched || ws.Status.CurrentSandbox == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("workspace: inconsistent dispatched Sandbox binding"))
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sandboxUID := ws.Status.CurrentSandbox.UID
	changes, stopWatching := h.store.Watch()
	defer stopWatching()
	if err := h.publishSandboxStop(ws.Status.CurrentSandbox); err != nil {
		return nil, err
	}
	updated, err := h.waitForSandboxStop(waitCtx, ws.Metadata.UUID, sandboxUID, changes)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: workspaceToProto(updated)}), nil
}

func (h *WorkspaceCatalogHandler) Delete(
	_ context.Context,
	req *connect.Request[workspacev1.DeleteWorkspaceRequest],
) (*connect.Response[workspacev1.DeleteWorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	unlock := h.lockLifecycle(ws.Metadata.UUID)
	defer unlock()
	ws, err = h.store.Get(ws.Metadata.UUID)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	if err := guardLock(ws, fp); err != nil {
		return nil, err
	}
	if req.Msg.GetExpectedGeneration() == 0 || req.Msg.GetExpectedGeneration() != ws.Metadata.Generation {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("workspace: generation changed before delete"))
	}
	if ws.Status.IsTrue(workspacecatalog.ConditionDispatched) || ws.Status.CurrentSandbox != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("workspace: stop the workspace before delete"))
	}
	if err := h.store.Delete(ws.Metadata.UUID); err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.DeleteWorkspaceResponse{}), nil
}

var errSandboxBindingChanged = errors.New("workspace: Sandbox binding changed")

const maxWorkspaceStorageReports = 1024

// ReconcileWorkspaceStorage persists bounded reports only for Workspaces still
// bound to the authenticated Node. Stale or foreign reports are ignored.
func (h *WorkspaceCatalogHandler) ReconcileWorkspaceStorage(
	nodeFP identity.Fingerprint,
	reports []*nodectrlv1.WorkspaceStorageReport,
) {
	if nodeFP.IsZero() {
		return
	}
	for _, report := range reports[:min(len(reports), maxWorkspaceStorageReports)] {
		if report == nil || report.GetWorkspaceUuid() == "" {
			continue
		}
		_, _ = h.store.Update(report.GetWorkspaceUuid(), workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
			current := value.Status.CurrentSandbox
			if current == nil || current.NodeFingerprint != nodeFP || value.Spec.TargetSandboxFingerprint != nodeFP.String() {
				return errSandboxBindingChanged
			}
			value.Status.DiskUsedBytes = report.GetUsedBytes()
			value.Status.DiskAllowanceBytes = report.GetAllowanceBytes()
			value.Status.ProtectedBytes = report.GetProtectedBytes()
			if report.GetManifestGeneration() >= value.Status.ManifestGeneration {
				value.Status.ManifestDigest = report.GetManifestDigest()
				value.Status.ManifestGeneration = report.GetManifestGeneration()
			}
			conditionStatus := workspacecatalog.ConditionStatusFalse
			reason := "BelowThreshold"
			message := "workspace disk is writable"
			if report.GetFrozen() {
				conditionStatus = workspacecatalog.ConditionStatusTrue
				reason = "UsageThresholdExceeded"
				message = "workspace disk is paused pending remediation"
			}
			value.Status.SetCondition(workspacecatalog.ConditionStorageFrozen, conditionStatus, reason, message, time.Now())
			return nil
		})
	}
}

// StopAgentSandbox idempotently tears down one exact Workspace Sandbox. A
// stale AgentRun can never clear or stop a newer binding because both UUIDs are
// checked before publication and again in the catalog transaction.
func (h *WorkspaceCatalogHandler) StopAgentSandbox(owner identity.Fingerprint, workspaceUUID, sandboxUID string) error {
	if owner.IsZero() || workspaceUUID == "" || sandboxUID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: incomplete Sandbox stop binding"))
	}
	unlock := h.lockLifecycle(workspaceUUID)
	defer unlock()
	return h.stopAgentSandbox(owner, workspaceUUID, sandboxUID)
}

func (h *WorkspaceCatalogHandler) stopAgentSandbox(owner identity.Fingerprint, workspaceUUID, sandboxUID string) error {
	if h.dispatcher == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("orchestrator: no dispatcher configured"))
	}
	ws, err := h.store.Get(workspaceUUID)
	if err != nil || ws.Metadata.OwnerFingerprint != owner {
		return connect.NewError(connect.CodeNotFound, errors.New("orchestrator: Workspace not found"))
	}
	current := ws.Status.CurrentSandbox
	if current == nil || current.UID != sandboxUID {
		return nil
	}
	if err := h.publishSandboxStop(current); err != nil {
		return err
	}
	_, err = h.store.Update(workspaceUUID, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		if w.Status.CurrentSandbox == nil || w.Status.CurrentSandbox.UID != sandboxUID {
			return errSandboxBindingChanged
		}
		w.Status.CurrentSandbox = nil
		w.Status.SetCondition(workspacecatalog.ConditionDispatched, workspacecatalog.ConditionStatusFalse, "Stopped", "", time.Now())
		return nil
	})
	if errors.Is(err, errSandboxBindingChanged) {
		return nil
	}
	if err != nil {
		return mapCatalogErr(err)
	}
	return nil
}

func (h *WorkspaceCatalogHandler) publishSandboxStop(current *workspacecatalog.SandboxRef) error {
	sandbox := &sandboxv1.Sandbox{Metadata: &sandboxv1.SandboxMetadata{Uid: current.UID}}
	if err := h.dispatcher.PublishSandboxEvent(current.NodeFingerprint, sandboxv1.EventType_EVENT_TYPE_DELETED, sandbox); err != nil {
		return mapSandboxPublishError(err)
	}
	return nil
}

func (h *WorkspaceCatalogHandler) waitForSandboxStop(
	ctx context.Context,
	workspaceUUID, sandboxUID string,
	changes <-chan uint64,
) (workspacecatalog.Workspace, error) {
	//daft:lint:disable=forloopbound reason="bounded by the 30s caller context; exits on teardown notification, binding replacement, catalog error, or deadline"
	for {
		ws, err := h.store.Get(workspaceUUID)
		if err != nil {
			return workspacecatalog.Workspace{}, mapCatalogErr(err)
		}
		if ws.Status.CurrentSandbox == nil {
			return ws, nil
		}
		if ws.Status.CurrentSandbox.UID != sandboxUID {
			return ws, nil
		}
		select {
		case <-ctx.Done():
			return workspacecatalog.Workspace{}, connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("workspace: timed out waiting for Sandbox %s teardown acknowledgment: %w", sandboxUID, ctx.Err()))
		case <-changes:
		}
	}
}

// Acquire takes or refreshes the workspace's write lock. Refresh
// is just an Acquire by the current holder: the expiry advances,
// the holder doesn't change. Refusal is FailedPrecondition with
// the prior owner's fingerprint in the error message so the
// client can tell who's holding it.
func (h *WorkspaceCatalogHandler) Acquire(
	_ context.Context,
	req *connect.Request[workspacev1.AcquireRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	ttl := req.Msg.GetTtl().AsDuration()
	if ttl <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: ttl must be positive"))
	}
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if ws.Status.Lock.HeldByOther(fp, now) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: locked by %s until %s",
				ws.Status.Lock.OwnerFingerprint, ws.Status.Lock.ExpiresAt.Format(time.RFC3339)))
	}
	updated, err := h.store.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus,
		func(w *workspacecatalog.Workspace) error {
			w.Status.Lock = workspacecatalog.Lock{
				OwnerFingerprint: fp,
				ExpiresAt:        now.Add(ttl),
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

// Release clears the workspace's write lock if held by the
// caller. Idempotent on an unheld workspace; refuses with
// FailedPrecondition when held by someone else.
func (h *WorkspaceCatalogHandler) Release(
	_ context.Context,
	req *connect.Request[workspacev1.ReleaseRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if ws.Status.Lock.HeldByOther(fp, now) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: locked by %s; cannot release another caller's lock",
				ws.Status.Lock.OwnerFingerprint))
	}
	updated, err := h.store.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus,
		func(w *workspacecatalog.Workspace) error {
			w.Status.Lock = workspacecatalog.Lock{}
			return nil
		})
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{
		Workspace: workspaceToProto(updated),
	}), nil
}

// workspaceToSandboxSpec composes the wire workload spec that the
// Node's executor consumes. The caller wraps this in a Sandbox
// resource (with metadata + status) via workspaceToSandbox.
func workspaceToSandboxSpec(ws workspacecatalog.Workspace, nodeFP identity.Fingerprint) *sandboxv1.SandboxSpec {
	return &sandboxv1.SandboxSpec{
		ImageRef:         ws.Spec.ImageRef,
		DevcontainerPath: ws.Spec.DevcontainerPath,
		WorkspaceUuid:    ws.Metadata.UUID,
		Node:             &sandboxv1.NodeRef{Fingerprint: nodeFP.String()},
		Mounts:           mountsToProto(ws.Spec.Mounts),
		Env:              envToProto(ws.Spec.Env),
		Ports:            portsToProto(ws.Spec.Ports),
		MemoryBytes:      ws.Spec.MemoryBytes,
		CpuCores:         ws.Spec.CPUCores,
		DiskBytes:        ws.Spec.DiskBytes,
		NestedVirtualization: &sandboxv1.NestedVirtualization{
			Enable: ws.Spec.NestedVirt,
		},
		PrivilegedFuseMounts: &sandboxv1.PrivilegedFuseMounts{
			Enable: ws.Spec.PrivilegedFuseMounts,
		},
		PrivilegedBuild: &sandboxv1.PrivilegedBuild{
			Enable: ws.Spec.PrivilegedBuild,
		},
	}
}

// mountsToProto / envToProto / portsToProto project the catalog's
// Go-native slices onto their wire forms. Shared by workspaceToProto
// (catalog → CLI) and workspaceToSandboxSpec (catalog → Node).
func workspaceSpecFromProto(in *workspacev1.WorkspaceSpec) (workspacecatalog.Spec, error) {
	if in == nil {
		return workspacecatalog.Spec{}, errors.New("workspace: spec is required")
	}
	if in.GetImageRef() != "" && in.GetPendingBuildId() != "" {
		return workspacecatalog.Spec{}, errors.New("workspace: image_ref and pending_build_id are mutually exclusive")
	}
	if target := in.GetTargetSandboxFingerprint(); target != "" {
		if _, err := identity.ParseFingerprint(target); err != nil {
			return workspacecatalog.Spec{}, fmt.Errorf("workspace: invalid target fingerprint: %w", err)
		}
	}
	if dc := in.GetDevcontainerPath(); dc != "" && (pathpkg.IsAbs(dc) || pathpkg.Clean(dc) == ".." || len(pathpkg.Clean(dc)) >= 3 && pathpkg.Clean(dc)[:3] == "../") {
		return workspacecatalog.Spec{}, fmt.Errorf("workspace: devcontainer_path %q must stay within workspace content", dc)
	}
	if in.GetIsolation() != sandboxv1.Isolation_ISOLATION_UNSPECIFIED &&
		in.GetIsolation() != sandboxv1.Isolation_ISOLATION_SHARED &&
		in.GetIsolation() != sandboxv1.Isolation_ISOLATION_STRICT {
		return workspacecatalog.Spec{}, fmt.Errorf("workspace: invalid isolation %d", in.GetIsolation())
	}
	out := workspacecatalog.Spec{
		ImageRef:                 in.GetImageRef(),
		PendingBuildID:           in.GetPendingBuildId(),
		DevcontainerPath:         in.GetDevcontainerPath(),
		TargetSandboxFingerprint: in.GetTargetSandboxFingerprint(),
		Isolation:                int32(in.GetIsolation()),
		MemoryBytes:              in.GetMemoryBytes(),
		CPUCores:                 in.GetCpuCores(),
		DiskBytes:                in.GetDiskBytes(),
	}
	if gpu := in.GetGpuRequest(); gpu != nil {
		out.GpuCount = gpu.GetCount()
		out.GpuMinVRAM = gpu.GetMinVramBytes()
	}
	if nested := in.GetNestedVirtualization(); nested != nil {
		out.NestedVirt = nested.GetEnable()
	}
	if fuse := in.GetPrivilegedFuseMounts(); fuse != nil {
		out.PrivilegedFuseMounts = fuse.GetEnable()
	}
	if build := in.GetPrivilegedBuild(); build != nil {
		out.PrivilegedBuild = build.GetEnable()
	}
	var err error
	out.Mounts, err = workspaceMountsFromProto(in.GetMounts())
	if err != nil {
		return workspacecatalog.Spec{}, err
	}
	out.Env, err = workspaceEnvFromProto(in.GetEnv())
	if err != nil {
		return workspacecatalog.Spec{}, err
	}
	out.Ports, err = workspacePortsFromProto(in.GetPorts())
	if err != nil {
		return workspacecatalog.Spec{}, err
	}
	return out, nil
}

func workspaceMountsFromProto(in []*sandboxv1.Mount) ([]workspacecatalog.Mount, error) {
	out := make([]workspacecatalog.Mount, 0, len(in))
	mountTargets := make(map[string]struct{}, len(in))
	for _, mount := range in {
		if mount == nil || !pathpkg.IsAbs(mount.GetTarget()) {
			return nil, fmt.Errorf("workspace: mount target %q must be absolute", mount.GetTarget())
		}
		if _, duplicate := mountTargets[mount.GetTarget()]; duplicate {
			return nil, fmt.Errorf("workspace: duplicate mount target %q", mount.GetTarget())
		}
		mountTargets[mount.GetTarget()] = struct{}{}
		out = append(out, workspacecatalog.Mount{Target: mount.GetTarget(), Source: mount.GetSource(), ReadOnly: mount.GetReadOnly()})
	}
	return out, nil
}

func workspaceEnvFromProto(in []*sandboxv1.EnvVar) ([]workspacecatalog.EnvVar, error) {
	out := make([]workspacecatalog.EnvVar, 0, len(in))
	envNames := make(map[string]struct{}, len(in))
	for _, env := range in {
		if env == nil || env.GetName() == "" {
			return nil, errors.New("workspace: env.name is required")
		}
		if _, duplicate := envNames[env.GetName()]; duplicate {
			return nil, fmt.Errorf("workspace: duplicate env %q", env.GetName())
		}
		envNames[env.GetName()] = struct{}{}
		out = append(out, workspacecatalog.EnvVar{Name: env.GetName(), Value: env.GetValue()})
	}
	return out, nil
}

func workspacePortsFromProto(in []*sandboxv1.PortMapping) ([]workspacecatalog.PortMapping, error) {
	out := make([]workspacecatalog.PortMapping, 0, len(in))
	type portKey struct {
		port     uint32
		protocol sandboxv1.Protocol
	}
	ports := make(map[portKey]struct{}, len(in))
	for _, portMapping := range in {
		if portMapping == nil || (portMapping.GetProtocol() != sandboxv1.Protocol_PROTOCOL_TCP && portMapping.GetProtocol() != sandboxv1.Protocol_PROTOCOL_UDP) {
			return nil, errors.New("workspace: port.protocol must be TCP or UDP")
		}
		if portMapping.GetContainerPort() > 65535 {
			return nil, fmt.Errorf("workspace: container port %d exceeds 65535", portMapping.GetContainerPort())
		}
		key := portKey{port: portMapping.GetContainerPort(), protocol: portMapping.GetProtocol()}
		if _, duplicate := ports[key]; duplicate {
			return nil, fmt.Errorf("workspace: duplicate port %d/%s", key.port, key.protocol)
		}
		ports[key] = struct{}{}
		out = append(out, workspacecatalog.PortMapping{ContainerPort: key.port, Protocol: int32(key.protocol)})
	}
	return out, nil
}

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
				Uuid: ws.Metadata.UUID,
				Name: ws.Metadata.Name,
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

// sandboxPhase* are the PascalCase phase tokens the orchestrator
// recognises on Sandbox status and HeartbeatRequest.live_sandboxes.
// See proto/sandbox/v1/sandbox.proto for the wire-side documentation.
// Sandboxd's sandboxexec package has parallel constants on its side
// of the wire — keeping the two unsynchronised would surface as the
// reconciler silently doing nothing on a typo.
const (
	sandboxPhasePending = "Pending"
	sandboxPhaseRunning = "Running"
	sandboxPhaseExited  = "Exited"
	sandboxPhaseKilled  = "Killed"
)

// mapCatalogErr maps workspacecatalog sentinel errors to Connect
// codes. Returns nil unchanged so handlers can use it
// unconditionally on every store-call result.
func mapCatalogErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspacecatalog.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, workspacecatalog.ErrNotOwner):
		return connect.NewError(connect.CodeNotFound, workspacecatalog.ErrNotFound)
	case errors.Is(err, workspacecatalog.ErrAliasInUse):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, workspacecatalog.ErrGenerationConflict):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, workspacecatalog.ErrGenerationOverflow),
		errors.Is(err, workspacecatalog.ErrLockHeldByOther):
		return connect.NewError(connect.CodeFailedPrecondition, err)
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

// workspaceToProto projects a catalog Workspace onto its wire
// triple (metadata + spec + status). The projection is mechanical:
// each Go-native sub-struct maps to the matching proto message.
func workspaceToProto(ws workspacecatalog.Workspace) *workspacev1.Workspace {
	return &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{
			Uuid:             ws.Metadata.UUID,
			Name:             ws.Metadata.Name,
			OwnerFingerprint: fingerprintStringOrEmpty(ws.Metadata.OwnerFingerprint),
			Generation:       ws.Metadata.Generation,
			CreatedAt:        timestamppb.New(ws.Metadata.CreatedAt),
			UpdatedAt:        timestamppb.New(ws.Metadata.UpdatedAt),
		},
		Spec: &workspacev1.WorkspaceSpec{
			ImageRef:                 ws.Spec.ImageRef,
			PendingBuildId:           ws.Spec.PendingBuildID,
			DevcontainerPath:         ws.Spec.DevcontainerPath,
			Mounts:                   mountsToProto(ws.Spec.Mounts),
			Env:                      envToProto(ws.Spec.Env),
			Ports:                    portsToProto(ws.Spec.Ports),
			TargetSandboxFingerprint: ws.Spec.TargetSandboxFingerprint,
			GpuRequest:               gpuRequestToProto(ws.Spec),
			Isolation:                sandboxv1.Isolation(ws.Spec.Isolation),
			MemoryBytes:              ws.Spec.MemoryBytes,
			CpuCores:                 ws.Spec.CPUCores,
			DiskBytes:                ws.Spec.DiskBytes,
			NestedVirtualization: &sandboxv1.NestedVirtualization{
				Enable: ws.Spec.NestedVirt,
			},
			PrivilegedFuseMounts: &sandboxv1.PrivilegedFuseMounts{
				Enable: ws.Spec.PrivilegedFuseMounts,
			},
			PrivilegedBuild: &sandboxv1.PrivilegedBuild{
				Enable: ws.Spec.PrivilegedBuild,
			},
		},
		Status: &workspacev1.WorkspaceStatus{
			ObservedGeneration:  ws.Status.ObservedGeneration,
			Conditions:          conditionsToProto(ws.Status.Conditions),
			CurrentSandbox:      sandboxRefToProto(ws.Status.CurrentSandbox),
			Missing:             append([]string(nil), ws.Status.Missing...),
			Lock:                lockToProto(ws.Status.Lock),
			DiskUsedBytes:       ws.Status.DiskUsedBytes,
			DiskAllowanceBytes:  ws.Status.DiskAllowanceBytes,
			ProtectedBytes:      ws.Status.ProtectedBytes,
			ManifestDigest:      ws.Status.ManifestDigest,
			ManifestGeneration:  ws.Status.ManifestGeneration,
			LastTerminalSandbox: terminalSandboxToProto(ws.Status.LastTerminalSandbox),
		},
	}
}

func terminalSandboxToProto(terminal *workspacecatalog.TerminalSandbox) *workspacev1.TerminalSandbox {
	if terminal == nil {
		return nil
	}
	return &workspacev1.TerminalSandbox{
		Uid: terminal.UID, NodeFingerprint: terminal.NodeFingerprint.String(),
		WorkspaceGeneration: terminal.WorkspaceGeneration,
		Phase:               terminal.Phase, ExitCode: terminal.ExitCode, Message: terminal.Message,
		ObservedAt: timestamppb.New(terminal.ObservedAt),
	}
}

// conditionsToProto maps the Go-native condition slice onto the
// wire shape. Shared by workspaceToProto so the Connect adapter
// stays the only place that knows the proto types.
func conditionsToProto(in []workspacecatalog.Condition) []*sandboxv1.Condition {
	out := make([]*sandboxv1.Condition, len(in))
	for i, c := range in {
		out[i] = &sandboxv1.Condition{
			Type:               c.Type,
			Status:             conditionStatusToProto(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: timestamppb.New(c.LastTransitionTime),
		}
	}
	return out
}

func conditionStatusToProto(s workspacecatalog.ConditionStatus) sandboxv1.ConditionStatus {
	switch s {
	case workspacecatalog.ConditionStatusTrue:
		return sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE
	case workspacecatalog.ConditionStatusFalse:
		return sandboxv1.ConditionStatus_CONDITION_STATUS_FALSE
	case workspacecatalog.ConditionStatusUnknown:
		return sandboxv1.ConditionStatus_CONDITION_STATUS_UNKNOWN
	}
	return sandboxv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
}

// lockToProto projects the catalog Lock onto the wire Lock. Zero
// (unheld) lock maps to nil so the wire field is omitted.
func lockToProto(l workspacecatalog.Lock) *workspacev1.Lock {
	if !l.IsHeld() {
		return nil
	}
	out := &workspacev1.Lock{
		OwnerFingerprint: fingerprintStringOrEmpty(l.OwnerFingerprint),
	}
	if !l.ExpiresAt.IsZero() {
		out.ExpiresAt = timestamppb.New(l.ExpiresAt)
	}
	return out
}

// gpuRequestToProto returns nil when no GPU is requested so the
// wire field stays unset. count==0 and min_vram==0 both indicate
// "no GPU"; only emit the message when at least one is non-zero.
func gpuRequestToProto(spec workspacecatalog.Spec) *sandboxv1.GpuRequest {
	if spec.GpuCount == 0 && spec.GpuMinVRAM == 0 {
		return nil
	}
	return &sandboxv1.GpuRequest{
		Count:        spec.GpuCount,
		MinVramBytes: spec.GpuMinVRAM,
	}
}

// sandboxRefToProto projects a catalog SandboxRef onto the wire
// shape. Nil maps to nil so a never-launched workspace serialises
// with an unset current_sandbox field.
func sandboxRefToProto(ref *workspacecatalog.SandboxRef) *sandboxv1.SandboxRef {
	if ref == nil {
		return nil
	}
	return &sandboxv1.SandboxRef{
		Uid:                 ref.UID,
		Node:                &sandboxv1.NodeRef{Fingerprint: ref.NodeFingerprint.String()},
		AgentProfileDigest:  ref.AgentProfileDigest,
		WorkspaceGeneration: ref.WorkspaceGeneration,
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
