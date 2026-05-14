package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
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

// WorkspaceCatalogHandler adapts the workspacecatalog.Store to the
// Connect-RPC WorkspaceCatalogService surface. Peer identity for
// every RPC is the mTLS leaf-cert fingerprint surfaced via
// withPeerMiddleware. For Slice B2a, any authenticated caller can
// mutate any workspace — the per-owner lock subsystem that gates
// mutation lands in B2d.
type WorkspaceCatalogHandler struct {
	workspacev1connect.UnimplementedWorkspaceCatalogServiceHandler
	store *workspacecatalog.Store
}

// NewWorkspaceCatalogHandler returns a handler backed by store.
// Returns an error if store is nil — the orchestrator's wiring
// would be malformed.
func NewWorkspaceCatalogHandler(store *workspacecatalog.Store) (*WorkspaceCatalogHandler, error) {
	if store == nil {
		return nil, errors.New("orchestrator: nil workspace catalog")
	}
	return &WorkspaceCatalogHandler{store: store}, nil
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
// caller's own workspaces. Filtering happens in the handler — the
// store's List is unfiltered to keep persistence-layer (B5) reads
// simple.
func (h *WorkspaceCatalogHandler) List(
	_ context.Context,
	req *connect.Request[workspacev1.ListWorkspacesRequest],
) (*connect.Response[workspacev1.ListWorkspacesResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	all := h.store.List()
	mineOnly := req.Msg.GetMineOnly()
	out := make([]*workspacev1.Workspace, 0, len(all))
	for i := range all {
		if mineOnly && all[i].OwnerFingerprint != fp {
			continue
		}
		out = append(out, workspaceToProto(all[i]))
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

// mutate is the shared auth + resolve + Update + project plumbing
// behind every per-field setter. Extracted so the simple setters
// (SetImage, SetTarget) don't repeat the 10-line dance — the
// add/append handlers keep their own bodies because they need
// pre-resolve input validation that doesn't fit a generic helper.
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
	updated, err := h.store.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		for i := range w.Mounts {
			if w.Mounts[i].Target == target {
				return connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("workspace: mount target %q already present", target))
			}
		}
		w.Mounts = append(w.Mounts, workspacecatalog.Mount{
			Target:   target,
			Source:   mount.GetSource(),
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
	if port.GetProtocol() == sandboxctrlv1.Protocol_PROTOCOL_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: port.protocol must be set"))
	}
	ws, err := h.resolveHandle(fp, req.Msg.GetHandle())
	if err != nil {
		return nil, err
	}
	// Wire-format port + protocol are preserved as-is on the
	// workspace; SandboxSpec projection at Launch time (B2c) reads
	// them back unchanged.
	containerPort := port.GetContainerPort()
	proto := int32(port.GetProtocol())
	updated, err := h.store.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		// Duplicate detection uses (port, protocol) since the same
		// container_port can legitimately be bound to TCP + UDP.
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
	// Already-mapped Connect errors round-trip unchanged.
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
	}
	if !ws.LockExpiresAt.IsZero() {
		out.LockExpiresAt = timestamppb.New(ws.LockExpiresAt)
	}
	out.Mounts = make([]*sandboxctrlv1.Mount, 0, len(ws.Mounts))
	for _, m := range ws.Mounts {
		out.Mounts = append(out.Mounts, &sandboxctrlv1.Mount{
			Target:   m.Target,
			Source:   m.Source,
			ReadOnly: m.ReadOnly,
		})
	}
	out.Env = make([]*sandboxctrlv1.EnvVar, 0, len(ws.Env))
	for _, e := range ws.Env {
		out.Env = append(out.Env, &sandboxctrlv1.EnvVar{
			Name:  e.Name,
			Value: e.Value,
		})
	}
	out.Ports = make([]*sandboxctrlv1.PortMapping, 0, len(ws.Ports))
	for _, p := range ws.Ports {
		out.Ports = append(out.Ports, &sandboxctrlv1.PortMapping{
			ContainerPort: p.ContainerPort,
			Protocol:      sandboxctrlv1.Protocol(p.Protocol),
		})
	}
	return out
}

// fingerprintStringOrEmpty renders a Fingerprint as its hex string,
// or "" when the value is the zero Fingerprint. Lets the wire form
// distinguish "no owner / no lock holder" from a real fingerprint
// without callers having to special-case the zero hash.
func fingerprintStringOrEmpty(fp identity.Fingerprint) string {
	var zero identity.Fingerprint
	if fp == zero {
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
