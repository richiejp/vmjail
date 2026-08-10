package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/casindex"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// Server implements WorkspaceServiceHandler over a CAS plus an
// in-memory registry of named manifests. The laptop running
// `daft workspace serve` constructs one of these, calls Add for
// each workspace it wants to expose, and hands it to mtls.Server.
type Server struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
	store         *cas.Store
	index         *casindex.Index
	cleanup       *casindex.Worker
	mu            sync.RWMutex
	manifests     map[string]*workspacev1.Manifest
	states        map[string]manifestState
	pending       map[string]pendingManifest
	reservedBytes uint64
	diskControl   WorkspaceDiskController

	// live maps a sync key to the host backing dir of a *running*
	// workspace VM (Slice S4). While an entry is present, GetManifest
	// re-BuildManifests that live dir on every pull so in-VM writes
	// surface, rather than serving the last-pushed snapshot from
	// manifests. The sandbox executor registers an entry when it
	// materialises a mount for a launch and clears it on teardown.
	liveMu sync.RWMutex
	live   map[string]liveMount
}

type WorkspaceDiskStatus struct {
	UsedBytes      uint64
	AvailableBytes uint64
	AllowanceBytes uint64
	Frozen         bool
	Running        bool
}

type WorkspaceDiskController interface {
	Status(string) (WorkspaceDiskStatus, error)
	Resize(context.Context, string, uint64) error
	Unfreeze(context.Context, string) error
	Delete(context.Context, string) error
}

const (
	defaultPruneCandidates = 50
	maxPruneCandidates     = 200
	maxMetadataFileBytes   = 1 << 20
)

type boundedMetadataWriter struct {
	buf bytes.Buffer
}

func (w *boundedMetadataWriter) Write(p []byte) (int, error) {
	if len(p) > maxMetadataFileBytes-w.buf.Len() {
		return 0, errors.New("workspace: metadata content exceeds maximum size")
	}
	return w.buf.Write(p)
}

type manifestState struct {
	digest     string
	generation uint64
}

type pendingManifest struct {
	manifest *workspacev1.Manifest
	state    manifestState
	missing  map[string]struct{}
	reserved uint64
}

type liveMount struct {
	dir     string
	refresh func(context.Context, string) error
	guard   *sync.Mutex
}

// NewServer returns an empty Server backed by store. Call Add to
// register workspaces before the server begins accepting traffic.
func NewServer(store *cas.Store) (*Server, error) {
	return newServer(store, nil)
}

// NewIndexedServer restores and transactionally persists manifests through
// index. Node production uses this constructor; lightweight clients may keep
// using NewServer without owning a Badger lifecycle.
func NewIndexedServer(store *cas.Store, index *casindex.Index) (*Server, error) {
	if index == nil {
		return nil, errors.New("workspace: nil cas index")
	}
	server, err := newServer(store, index)
	if err != nil {
		return nil, err
	}
	server.cleanup, err = casindex.NewWorker(casindex.WorkerConfig{Index: index})
	if err != nil {
		return nil, err
	}
	return server, nil
}

func newServer(store *cas.Store, index *casindex.Index) (*Server, error) {
	if store == nil {
		return nil, errors.New("workspace: nil cas store")
	}
	server := &Server{
		store:     store,
		index:     index,
		manifests: map[string]*workspacev1.Manifest{},
		states:    map[string]manifestState{},
		pending:   map[string]pendingManifest{},
		live:      map[string]liveMount{},
	}
	if index == nil {
		return server, nil
	}
	currents, err := index.CurrentManifests()
	if err != nil {
		return nil, fmt.Errorf("workspace: restore manifests: %w", err)
	}
	for currentIndex := range currents {
		current := &currents[currentIndex]
		var manifest workspacev1.Manifest
		if err := proto.Unmarshal(current.Payload, &manifest); err != nil {
			return nil, fmt.Errorf("workspace: restore manifest %q: %w", current.SyncKey, err)
		}
		server.manifests[current.SyncKey] = &manifest
		server.states[current.SyncKey] = manifestState{digest: current.Digest, generation: current.Generation}
	}
	return server, nil
}

// SetLiveMount marks dir as the live host backing directory of a
// running workspace VM, keyed by key (a SyncKey). While the entry is
// present, GetManifest(key) re-builds the manifest from dir on every
// pull so writes the VM has made surface, instead of returning the
// last-pushed snapshot. The sandbox executor registers this when a
// launch materialises the mount and clears it on teardown (Slice S4).
func (s *Server) SetLiveMount(key, dir string) {
	s.liveMu.Lock()
	s.live[key] = liveMount{dir: dir, guard: &sync.Mutex{}}
	s.liveMu.Unlock()
}

func (s *Server) SetWorkspaceDiskController(controller WorkspaceDiskController) {
	s.mu.Lock()
	s.diskControl = controller
	s.mu.Unlock()
}

// SetLiveMountRefresher replaces the live mount's refresh operation. Disk
// workspaces use it to stream the configured XFS directory out of the guest
// immediately before a pull or durable stop snapshot.
func (s *Server) SetLiveMountRefresher(key string, refresh func(context.Context, string) error) {
	s.liveMu.Lock()
	entry, ok := s.live[key]
	if ok {
		entry.refresh = refresh
		s.live[key] = entry
	}
	s.liveMu.Unlock()
}

// PreserveLiveMount snapshots a running mount into the durable manifest/CAS
// lane, then unregisters that exact live directory. It is idempotent when no
// live mount exists. The registration remains intact on failure so callers can
// retry or pull directly instead of losing the only reference to guest writes.
func (s *Server) PreserveLiveMount(ctx context.Context, key string) error {
	entry, ok := s.liveEntry(key)
	if !ok {
		return nil
	}
	entry.guard.Lock()
	defer entry.guard.Unlock()
	if entry.refresh != nil {
		if err := entry.refresh(ctx, entry.dir); err != nil {
			return fmt.Errorf("workspace: refresh live mount %q: %w", key, err)
		}
	}
	m, err := BuildManifest(entry.dir, s.store, BuildOptions{})
	if err != nil {
		return fmt.Errorf("workspace: preserve live mount %q: %w", key, err)
	}
	return s.commitPreservedLiveMount(key, entry.dir, m)
}

func (s *Server) commitPreservedLiveMount(key, dir string, m *workspacev1.Manifest) error {
	s.mu.Lock()
	s.liveMu.Lock()
	if current, exists := s.live[key]; !exists || current.dir != dir {
		s.liveMu.Unlock()
		s.mu.Unlock()
		return fmt.Errorf("workspace: live mount %q changed during preservation", key)
	}
	if _, err := s.publishManifestLocked(key, m, casindex.OriginSandbox, casindex.RecoverabilityProtected); err != nil {
		s.liveMu.Unlock()
		s.mu.Unlock()
		return err
	}
	delete(s.live, key)
	s.liveMu.Unlock()
	s.mu.Unlock()
	return nil
}

// ClearLiveMount removes the live-dir registration for key. Idempotent:
// clearing an absent key is a no-op, so the executor can clear keys it
// may never have set without bookkeeping.
func (s *Server) ClearLiveMount(key string) {
	s.liveMu.Lock()
	delete(s.live, key)
	s.liveMu.Unlock()
}

// liveDir returns the registered live backing dir for key, if any.
func (s *Server) liveDir(key string) (string, bool) {
	entry, ok := s.liveEntry(key)
	return entry.dir, ok
}

func (s *Server) liveEntry(key string) (liveMount, bool) {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	entry, ok := s.live[key]
	return entry, ok
}

// Add registers a manifest under name. Replacing an existing
// manifest is allowed (and intended — re-running serve with an
// updated tree should publish the new shape). The manifest is
// cloned so later callers can't mutate the registered copy.
func (s *Server) Add(name string, m *workspacev1.Manifest) error {
	if name == "" {
		return errors.New("workspace: empty workspace name")
	}
	if m == nil {
		return errors.New("workspace: nil manifest")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.publishManifestLocked(name, m, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable)
	return err
}

// Names lists registered workspaces in undefined order. Useful for
// `daft workspace ls` style introspection.
func (s *Server) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.manifests))
	for n := range s.manifests {
		out = append(out, n)
	}
	return out
}

// MaterializeMount reconstructs the tree previously pushed under key
// into dest, using only this server's local CAS — no network. dest is
// created if absent. It returns applied=false when nothing was pushed
// under key (dest is left as an empty directory, so an unpushed mount
// boots as an empty read-write tree the VM can fill); applied=true once
// the manifest has been laid down. A missing chunk is a hard error: the
// CAS is the sole source at launch, so an incomplete store is a bug, not
// a fetch opportunity.
//
// This remains the launch path for read-only workspace shares. Writable XFS
// mounts use StreamMount so their first-launch seed never needs a host tree.
func (s *Server) MaterializeMount(ctx context.Context, key, dest string) (applied bool, err error) {
	if dest == "" {
		return false, errors.New("workspace: empty dest")
	}
	s.mu.RLock()
	m, ok := s.manifests[key]
	s.mu.RUnlock()
	if !ok {
		// Nothing pushed for this mount yet — ensure an empty backing
		// dir exists so virtiofsd has something to export.
		if mkErr := os.MkdirAll(dest, 0o700); mkErr != nil {
			return false, fmt.Errorf("workspace: mkdir mount backing dir: %w", mkErr)
		}
		return false, nil
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return false, fmt.Errorf("workspace: mkdir mount backing dir: %w", err)
	}
	// Local-only apply: nil client means every chunk must already be in
	// store, which it is — push installed them before publishing the
	// manifest.
	if err := ApplyManifest(ctx, nil, s.store, m, dest, ApplyOptions{}); err != nil {
		return false, err
	}
	return true, nil
}

// ReadMountFile reads one small regular file directly from a published
// manifest. It is used for launch metadata (not workspace contents), so a
// writable XFS mount need not be reconstructed into a host-side virtiofs
// backing tree merely to inspect its devcontainer configuration.
func (s *Server) ReadMountFile(ctx context.Context, key, path string) (data []byte, found bool, err error) {
	if !SanitisePath(path) {
		return nil, false, fmt.Errorf("workspace: rejected metadata path %q", path)
	}
	s.mu.RLock()
	m, ok := s.manifests[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	for _, entry := range m.GetEntries() {
		if entry.GetPath() != path {
			continue
		}
		if entry.GetKind() != workspacev1.EntryKind_ENTRY_KIND_FILE {
			return nil, false, fmt.Errorf("workspace: metadata path %q is not a regular file", path)
		}
		if entry.GetSize() > maxMetadataFileBytes {
			return nil, false, fmt.Errorf("workspace: metadata file %q is %d bytes; maximum is %d", path, entry.GetSize(), maxMetadataFileBytes)
		}
		// The check above bounds this conversion to 1 MiB on every target.
		size := int(entry.GetSize()) //nolint:gosec // G115: explicitly bounded by maxMetadataFileBytes.
		var dst boundedMetadataWriter
		dst.buf.Grow(size)
		if err := streamChunks(&dst, s.store, entry.GetChunks()); err != nil {
			return nil, false, err
		}
		if dst.buf.Len() != size {
			return nil, false, fmt.Errorf("workspace: metadata file %q size mismatch: manifest=%d actual=%d", path, entry.GetSize(), dst.buf.Len())
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		return dst.buf.Bytes(), true, nil
	}
	return nil, false, nil
}

// GetManifest implements the proto endpoint. Unknown workspaces
// return CodeNotFound; never the empty manifest, because an empty
// reply is indistinguishable from "the workspace genuinely has no
// files" and we want callers to be able to tell the two apart.
//
// We hand the registered manifest pointer straight back: Add
// already cloned on the way in, and Connect serialises the
// response onto the wire before the handler returns, so no caller
// can mutate it.
func (s *Server) GetManifest(ctx context.Context, req *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	name := req.Msg.GetWorkspace()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: empty workspace name"))
	}
	// A live workspace VM wins: re-manifest its backing dir so a pull
	// reflects in-VM writes (Slice S4). BuildManifest installs any new
	// chunks into the CAS, so the client's follow-up GetChunk calls
	// resolve. When the VM isn't running we fall through to the
	// last-pushed snapshot.
	if entry, ok := s.liveEntry(name); ok {
		entry.guard.Lock()
		current, stillLive := s.liveEntry(name)
		if !stillLive || current.guard != entry.guard {
			entry.guard.Unlock()
		} else {
			defer entry.guard.Unlock()
			if entry.refresh != nil {
				if err := entry.refresh(ctx, entry.dir); err != nil {
					return nil, connect.NewError(connect.CodeUnavailable,
						fmt.Errorf("workspace: refresh live mount %q: %w", name, err))
				}
			}
			m, err := BuildManifest(entry.dir, s.store, BuildOptions{})
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("workspace: re-manifest live mount %q: %w", name, err))
			}
			if s.index == nil {
				return connect.NewResponse(&workspacev1.GetManifestResponse{Manifest: m}), nil
			}
			state, err := s.publishManifest(name, m, casindex.OriginSandbox, casindex.RecoverabilityProtected)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			return connect.NewResponse(&workspacev1.GetManifestResponse{
				Manifest: m, ManifestDigest: state.digest, Generation: state.generation,
			}), nil
		}
	}
	s.mu.RLock()
	m, ok := s.manifests[name]
	state := s.states[name]
	s.mu.RUnlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("workspace: unknown workspace %q", name))
	}
	return connect.NewResponse(&workspacev1.GetManifestResponse{
		Manifest: m, ManifestDigest: state.digest, Generation: state.generation,
	}), nil
}

// GetChunk implements the proto endpoint. Returns the raw
// zstd-compressed payload from the CAS — the client decompresses
// and verifies on its end, so we never trust the server's framing.
// Missing chunks return CodeNotFound; malformed digests return
// CodeInvalidArgument; everything else is CodeInternal.
func (s *Server) GetChunk(_ context.Context, req *connect.Request[workspacev1.GetChunkRequest]) (*connect.Response[workspacev1.GetChunkResponse], error) {
	digest := req.Msg.GetDigest()
	data, err := s.store.GetCompressed(digest)
	if err != nil {
		if errors.Is(err, cas.ErrInvalidDigest) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, mapStoreError(err)
	}
	if s.index != nil {
		s.index.Touch(digest)
	}
	return connect.NewResponse(&workspacev1.GetChunkResponse{
		Compressed: data,
	}), nil
}

// PutManifest publishes the request's manifest under its workspace
// key and replies with the digests of referenced chunks the store
// doesn't yet hold, so the client uploads only the gaps. Publishing
// before the chunks land is deliberate: a GetChunk for a missing
// digest fails cleanly with CodeNotFound, and the alternative
// (buffer the manifest until every chunk arrives) would force the
// server to track partial-upload state it doesn't need.
func (s *Server) PutManifest(ctx context.Context, req *connect.Request[workspacev1.PutManifestRequest]) (*connect.Response[workspacev1.PutManifestResponse], error) {
	name := req.Msg.GetWorkspace()
	m := req.Msg.GetManifest()
	// Compute the gap before publishing: if the store can't be probed
	// we must NOT register a manifest the client believes failed —
	// that would leave a GETtable but unbacked workspace with no
	// signal to re-push.
	missing, err := s.missingDigests(m)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := s.prepareClientPush(ctx, name); err != nil {
		return nil, err
	}
	state, err := s.stageClientManifest(ctx, name, m, missing)
	if err != nil {
		code := connect.CodeInvalidArgument
		if errors.Is(err, casindex.ErrInsufficientReclaim) {
			code = connect.CodeResourceExhausted
		}
		return nil, connect.NewError(code, err)
	}
	return connect.NewResponse(&workspacev1.PutManifestResponse{
		MissingDigests: missing, ManifestDigest: state.digest, Generation: state.generation,
	}), nil
}

// prepareClientPush resolves the retained-disk/CAS authority conflict before
// accepting a client manifest. A push cannot update a running disk, and it
// must never overwrite a stopped disk whose sandbox snapshot is still
// protected. Once every current snapshot is recoverable, removing the stopped
// disk is safe and forces the next launch to seed XFS from the newly pushed
// manifests instead of silently reusing stale bytes.
func (s *Server) prepareClientPush(ctx context.Context, syncKey string) error {
	controller := s.workspaceDiskController()
	if controller == nil || s.index == nil {
		return nil
	}
	workspaceID, _, ok := strings.Cut(syncKey, syncKeySep)
	if !ok || workspaceID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: invalid workspace sync key"))
	}
	status, err := controller.Status(workspaceID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("workspace: inspect retained disk before push: %w", err))
	}
	if status.Running {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("workspace: stop the running workspace before push"))
	}
	if status.AllowanceBytes == 0 {
		return nil
	}
	indexed, err := s.index.WorkspaceStatus(workspaceID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("workspace: inspect manifest protection before push: %w", err))
	}
	for manifestIndex := range indexed.Manifests {
		if indexed.Manifests[manifestIndex].Recoverability == casindex.RecoverabilityProtected {
			return connect.NewError(connect.CodeFailedPrecondition,
				errors.New("workspace: retained disk has unadopted sandbox changes; pull and acknowledge, reject, or delete them before push"))
		}
	}
	if err := controller.Delete(ctx, workspaceID); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workspace: delete recoverable retained disk before push: %w", err))
	}
	return nil
}

// missingDigests returns the deduplicated digests referenced by m
// that the store doesn't have. Order follows first appearance so the
// client uploads roughly in manifest order.
func (s *Server) missingDigests(m *workspacev1.Manifest) ([]string, error) {
	var missing []string
	seen := map[string]struct{}{}
	for _, e := range m.GetEntries() {
		for _, c := range e.GetChunks() {
			d := c.GetDigest()
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			have, err := s.store.Has(d)
			if err != nil {
				return nil, err
			}
			if !have {
				missing = append(missing, d)
			}
		}
	}
	return missing, nil
}

// PutChunk installs one compressed chunk. PutCompressed verifies the
// uncompressed sha256 against the asserted digest before committing,
// so a corrupt or mislabelled chunk is rejected as CodeInvalidArgument
// rather than poisoning the store.
func (s *Server) PutChunk(_ context.Context, req *connect.Request[workspacev1.PutChunkRequest]) (*connect.Response[workspacev1.PutChunkResponse], error) {
	err := s.store.PutCompressed(req.Msg.GetDigest(), req.Msg.GetCompressed())
	switch {
	case err == nil:
		if commitErr := s.completePendingManifests(req.Msg.GetDigest()); commitErr != nil {
			return nil, connect.NewError(connect.CodeInternal, commitErr)
		}
		return connect.NewResponse(&workspacev1.PutChunkResponse{}), nil
	case errors.Is(err, cas.ErrInvalidDigest), errors.Is(err, cas.ErrCorrupt):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
}

// AcknowledgeManifest adopts only the exact manifest returned by GetManifest.
// Pull calls this after local materialisation succeeds.
func (s *Server) AcknowledgeManifest(
	ctx context.Context,
	req *connect.Request[workspacev1.AcknowledgeManifestRequest],
) (*connect.Response[workspacev1.AcknowledgeManifestResponse], error) {
	name := req.Msg.GetWorkspace()
	digest := req.Msg.GetManifestDigest()
	generation := req.Msg.GetGeneration()
	s.mu.RLock()
	state, exists := s.states[name]
	s.mu.RUnlock()
	if !exists || state.digest != digest || state.generation != generation {
		return nil, connect.NewError(connect.CodeFailedPrecondition, casindex.ErrStaleManifest)
	}
	if s.index != nil {
		if err := s.index.Acknowledge(name, digest, generation); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}
	// Once a stopped Sandbox manifest is exactly adopted, CAS is the durable
	// recoverable copy and the retained qcow2 no longer protects unique work.
	// A running disk must remain: it may already contain writes newer than the
	// acknowledged generation.
	workspaceID, _, isMount := strings.Cut(name, syncKeySep)
	if controller := s.workspaceDiskController(); controller != nil && isMount {
		status, err := controller.Status(workspaceID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("workspace: inspect adopted disk: %w", err))
		}
		if !status.Running {
			if err := controller.Delete(ctx, workspaceID); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("workspace: delete adopted disk: %w", err))
			}
		}
	}
	return connect.NewResponse(&workspacev1.AcknowledgeManifestResponse{}), nil
}

func (s *Server) RejectManifest(
	ctx context.Context,
	req *connect.Request[workspacev1.RejectManifestRequest],
) (*connect.Response[workspacev1.ManifestControlResponse], error) {
	name := req.Msg.GetWorkspace()
	digest := req.Msg.GetManifestDigest()
	generation := req.Msg.GetGeneration()
	if err := s.requireExactIndexedManifest(name, digest, generation); err != nil {
		return nil, err
	}
	indexed, err := s.index.Manifest(name, digest, generation)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if indexed.Origin != casindex.OriginSandbox || indexed.Recoverability != casindex.RecoverabilityProtected {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("workspace: manifest has no unadopted sandbox changes"))
	}
	if controller := s.workspaceDiskController(); controller != nil {
		uuid, _, ok := strings.Cut(name, "\x00")
		if !ok || uuid == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: invalid workspace sync key"))
		}
		// Rejection is destructive by definition: remove the stopped disk before
		// changing CAS provenance so rejected XFS bytes cannot reappear on the
		// next launch. A running disk is refused by the production controller.
		if err := controller.Delete(ctx, uuid); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}
	if err := s.index.Reject(name, digest, generation); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	state, present, err := s.reloadCurrentManifest(name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&workspacev1.ManifestControlResponse{
		ManifestDigest: state.digest, Generation: state.generation, CurrentManifestPresent: present,
	}), nil
}

func (s *Server) DeleteManifest(
	_ context.Context,
	req *connect.Request[workspacev1.DeleteManifestRequest],
) (*connect.Response[workspacev1.ManifestControlResponse], error) {
	name := req.Msg.GetWorkspace()
	if err := s.requireExactIndexedManifest(name, req.Msg.GetManifestDigest(), req.Msg.GetGeneration()); err != nil {
		return nil, err
	}
	if err := s.index.DeleteManifest(name, req.Msg.GetManifestDigest(), req.Msg.GetGeneration()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.mu.Lock()
	delete(s.manifests, name)
	delete(s.states, name)
	if pending, exists := s.pending[name]; exists {
		s.reservedBytes -= min(s.reservedBytes, pending.reserved)
		delete(s.pending, name)
	}
	s.mu.Unlock()
	return connect.NewResponse(&workspacev1.ManifestControlResponse{}), nil
}

func (s *Server) GetStorageStatus(
	_ context.Context,
	_ *connect.Request[workspacev1.GetStorageStatusRequest],
) (*connect.Response[workspacev1.GetStorageStatusResponse], error) {
	status, err := s.StorageStatus()
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&workspacev1.GetStorageStatusResponse{
		UsageBytes: status.UsageBytes, ProtectedBytes: status.ProtectedBytes,
		RecoverableBytes: status.RecoverableBytes, QueuedBytes: status.QueuedBytes,
		ChunkCount: uint64(status.ChunkCount), QueuedCount: uint64(status.QueuedCount), //nolint:gosec // counts originate from non-negative slice lengths.
		HardBytes: status.Policy.HardBytes, SoftBytes: status.Policy.SoftBytes,
		TargetBytes: status.Policy.TargetBytes, ExpirySeconds: int64(status.Policy.Expiry.Seconds()),
	}), nil
}

func (s *Server) PruneStorage(
	ctx context.Context,
	req *connect.Request[workspacev1.PruneStorageRequest],
) (*connect.Response[workspacev1.PruneStorageResponse], error) {
	if !req.Msg.GetDryRun() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("workspace: public prune is dry-run only"))
	}
	limit := int(req.Msg.GetMaxCandidates())
	if limit == 0 {
		limit = defaultPruneCandidates
	}
	if limit > maxPruneCandidates {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workspace: max_candidates exceeds %d", maxPruneCandidates))
	}
	plan, err := s.Prune(ctx, true)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	response := &workspacev1.PruneStorageResponse{UsageBefore: plan.UsageBefore, UsageAfter: plan.UsageAfter}
	response.Truncated = len(plan.Candidates) > limit
	for _, candidate := range plan.Candidates[:min(len(plan.Candidates), limit)] {
		response.Candidates = append(response.Candidates, &workspacev1.PruneCandidate{
			Digest: candidate.Digest, Size: candidate.Size, Score: candidate.Score,
			Disposition: string(candidate.Disposition), LastUseUnix: candidate.LastUse.Unix(),
		})
	}
	return connect.NewResponse(response), nil
}

func (s *Server) GetWorkspaceDiskStatus(
	_ context.Context,
	req *connect.Request[workspacev1.GetWorkspaceDiskStatusRequest],
) (*connect.Response[workspacev1.GetWorkspaceDiskStatusResponse], error) {
	controller := s.workspaceDiskController()
	if controller == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace: disk controller unavailable"))
	}
	uuid := req.Msg.GetWorkspaceUuid()
	if uuid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: workspace UUID is required"))
	}
	status, err := controller.Status(uuid)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	response, err := s.workspaceDiskStatusResponse(uuid, status)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (s *Server) UnfreezeWorkspaceDisk(
	ctx context.Context,
	req *connect.Request[workspacev1.UnfreezeWorkspaceDiskRequest],
) (*connect.Response[workspacev1.GetWorkspaceDiskStatusResponse], error) {
	controller := s.workspaceDiskController()
	if controller == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace: disk controller unavailable"))
	}
	uuid := req.Msg.GetWorkspaceUuid()
	if err := s.validateManifestExpectations(uuid, req.Msg.GetExpectedManifests()); err != nil {
		return nil, err
	}
	if err := controller.Unfreeze(ctx, uuid); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	status, err := controller.Status(uuid)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response, err := s.workspaceDiskStatusResponse(uuid, status)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (s *Server) ResizeWorkspaceDisk(
	ctx context.Context,
	req *connect.Request[workspacev1.ResizeWorkspaceDiskRequest],
) (*connect.Response[workspacev1.GetWorkspaceDiskStatusResponse], error) {
	controller := s.workspaceDiskController()
	if controller == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace: disk controller unavailable"))
	}
	uuid := req.Msg.GetWorkspaceUuid()
	if req.Msg.GetDiskBytes() == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: disk allowance is required"))
	}
	if err := s.validateManifestExpectations(uuid, req.Msg.GetExpectedManifests()); err != nil {
		return nil, err
	}
	if err := controller.Resize(ctx, uuid, req.Msg.GetDiskBytes()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	status, err := controller.Status(uuid)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response, err := s.workspaceDiskStatusResponse(uuid, status)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (s *Server) DeleteWorkspaceData(
	ctx context.Context,
	req *connect.Request[workspacev1.DeleteWorkspaceDataRequest],
) (*connect.Response[workspacev1.DeleteWorkspaceDataResponse], error) {
	controller := s.workspaceDiskController()
	if controller == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace: disk controller unavailable"))
	}
	uuid := req.Msg.GetWorkspaceUuid()
	if err := s.validateManifestExpectations(uuid, req.Msg.GetExpectedManifests()); err != nil {
		return nil, err
	}
	if err := controller.Delete(ctx, uuid); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err := s.index.DeleteWorkspace(casindex.WorkspaceID(uuid)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, expected := range req.Msg.GetExpectedManifests() {
		s.mu.Lock()
		delete(s.manifests, expected.GetWorkspace())
		delete(s.states, expected.GetWorkspace())
		if pending, exists := s.pending[expected.GetWorkspace()]; exists {
			s.reservedBytes -= min(s.reservedBytes, pending.reserved)
			delete(s.pending, expected.GetWorkspace())
		}
		s.mu.Unlock()
	}
	return connect.NewResponse(&workspacev1.DeleteWorkspaceDataResponse{}), nil
}

func (s *Server) workspaceDiskController() WorkspaceDiskController {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diskControl
}

func (s *Server) workspaceDiskStatusResponse(uuid string, status WorkspaceDiskStatus) (*workspacev1.GetWorkspaceDiskStatusResponse, error) {
	response := &workspacev1.GetWorkspaceDiskStatusResponse{
		WorkspaceUuid: uuid, UsedBytes: status.UsedBytes, AvailableBytes: status.AvailableBytes,
		AllowanceBytes: status.AllowanceBytes, Frozen: status.Frozen, Running: status.Running,
	}
	if s.index == nil {
		return response, nil
	}
	indexed, err := s.index.WorkspaceStatus(uuid)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response.ProtectedBytes = indexed.ProtectedBytes
	for index := range indexed.Manifests {
		manifest := &indexed.Manifests[index]
		response.CurrentManifests = append(response.CurrentManifests, &workspacev1.ManifestExpectation{
			Workspace: manifest.SyncKey, ManifestDigest: manifest.Digest, Generation: manifest.Generation,
		})
	}
	return response, nil
}

func (s *Server) validateManifestExpectations(uuid string, expected []*workspacev1.ManifestExpectation) error {
	if s.index == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("workspace: durable index unavailable"))
	}
	if uuid == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: workspace UUID is required"))
	}
	prefix := uuid + "\x00"
	provided := make(map[string]manifestState, len(expected))
	for _, item := range expected {
		if item == nil || !strings.HasPrefix(item.GetWorkspace(), prefix) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: manifest expectation is outside workspace"))
		}
		if _, duplicate := provided[item.GetWorkspace()]; duplicate {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("workspace: duplicate manifest expectation"))
		}
		provided[item.GetWorkspace()] = manifestState{digest: item.GetManifestDigest(), generation: item.GetGeneration()}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	currentCount := 0
	for key, state := range s.states {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		currentCount++
		if provided[key] != state {
			return connect.NewError(connect.CodeFailedPrecondition, casindex.ErrStaleManifest)
		}
	}
	if currentCount != len(provided) {
		return connect.NewError(connect.CodeFailedPrecondition, casindex.ErrStaleManifest)
	}
	return nil
}

func (s *Server) requireExactIndexedManifest(name, digest string, generation uint64) error {
	if s.index == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("workspace: durable index unavailable"))
	}
	s.mu.RLock()
	state, exists := s.states[name]
	s.mu.RUnlock()
	if name == "" || digest == "" || generation == 0 || !exists || state.digest != digest || state.generation != generation {
		return connect.NewError(connect.CodeFailedPrecondition, casindex.ErrStaleManifest)
	}
	return nil
}

func (s *Server) reloadCurrentManifest(name string) (manifestState, bool, error) {
	current, err := s.index.CurrentManifests()
	if err != nil {
		return manifestState{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.manifests, name)
	delete(s.states, name)
	for index := range current {
		indexed := &current[index]
		if indexed.SyncKey != name {
			continue
		}
		manifest := &workspacev1.Manifest{}
		if err := proto.Unmarshal(indexed.Payload, manifest); err != nil {
			return manifestState{}, false, fmt.Errorf("workspace: decode restored manifest %q: %w", name, err)
		}
		state := manifestState{digest: indexed.Digest, generation: indexed.Generation}
		s.manifests[name] = manifest
		s.states[name] = state
		return state, true, nil
	}
	return manifestState{}, false, nil
}

func (s *Server) stageClientManifest(ctx context.Context, name string, manifest *workspacev1.Manifest, missing []string) (manifestState, error) {
	if name == "" || manifest == nil {
		return manifestState{}, errors.New("workspace: manifest identity is required")
	}
	payload, digest, err := encodeManifest(manifest)
	if err != nil {
		return manifestState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.nextManifestStateLocked(name, digest)
	if pending, exists := s.pending[name]; exists && pending.state == state {
		return state, nil
	}
	if len(missing) == 0 {
		return s.publishPreparedManifestLocked(name, manifest, state, payload,
			casindex.OriginClientUpload, casindex.RecoverabilityRecoverable)
	}
	missingSet := make(map[string]struct{}, len(missing))
	for _, missingDigest := range missing {
		missingSet[missingDigest] = struct{}{}
	}
	reserved := missingChunkBytes(manifest, missingSet)
	if s.cleanup != nil {
		if _, err := s.cleanup.AdmitWrite(ctx, saturatingWorkspaceBytes(s.reservedBytes, reserved)); err != nil {
			return manifestState{}, err
		}
	}
	if previous, exists := s.pending[name]; exists {
		s.reservedBytes -= min(s.reservedBytes, previous.reserved)
	}
	clone, _ := proto.Clone(manifest).(*workspacev1.Manifest)
	s.pending[name] = pendingManifest{manifest: clone, state: state, missing: missingSet, reserved: reserved}
	s.reservedBytes = saturatingWorkspaceBytes(s.reservedBytes, reserved)
	return state, nil
}

func (s *Server) completePendingManifests(digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, pending := range s.pending {
		delete(pending.missing, digest)
		if len(pending.missing) != 0 {
			s.pending[name] = pending
			continue
		}
		payload, _, err := encodeManifest(pending.manifest)
		if err != nil {
			return err
		}
		if _, err := s.publishPreparedManifestLocked(name, pending.manifest, pending.state, payload,
			casindex.OriginClientUpload, casindex.RecoverabilityRecoverable); err != nil {
			return err
		}
		delete(s.pending, name)
	}
	return nil
}

func (s *Server) publishManifest(name string, manifest *workspacev1.Manifest, origin casindex.Origin, recoverability casindex.Recoverability) (manifestState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishManifestLocked(name, manifest, origin, recoverability)
}

func (s *Server) publishManifestLocked(name string, manifest *workspacev1.Manifest, origin casindex.Origin, recoverability casindex.Recoverability) (manifestState, error) {
	payload, digest, err := encodeManifest(manifest)
	if err != nil {
		return manifestState{}, err
	}
	state := s.nextManifestStateLocked(name, digest)
	return s.publishPreparedManifestLocked(name, manifest, state, payload, origin, recoverability)
}

func (s *Server) publishPreparedManifestLocked(
	name string,
	manifest *workspacev1.Manifest,
	state manifestState,
	payload []byte,
	origin casindex.Origin,
	recoverability casindex.Recoverability,
) (manifestState, error) {
	if current, exists := s.states[name]; exists && current == state {
		return current, nil
	}
	if s.index != nil {
		if err := s.index.CommitManifest(casindex.CommitRequest{
			SyncKey: name, Digest: state.digest, Generation: state.generation,
			Chunks: manifestChunkRefs(manifest), Origin: origin, Recoverability: recoverability, Payload: payload,
		}); err != nil {
			return manifestState{}, err
		}
	}
	clone, _ := proto.Clone(manifest).(*workspacev1.Manifest)
	s.manifests[name] = clone
	s.states[name] = state
	if pending, exists := s.pending[name]; exists {
		s.reservedBytes -= min(s.reservedBytes, pending.reserved)
	}
	delete(s.pending, name)
	return state, nil
}

func (s *Server) nextManifestStateLocked(name, digest string) manifestState {
	if current, exists := s.states[name]; exists && current.digest == digest {
		return current
	}
	if pending, exists := s.pending[name]; exists && pending.state.digest == digest {
		return pending.state
	}
	return manifestState{digest: digest, generation: s.states[name].generation + 1}
}

func encodeManifest(manifest *workspacev1.Manifest) (payload []byte, digest string, err error) {
	if manifest == nil {
		return nil, "", errors.New("workspace: nil manifest")
	}
	payload, err = (proto.MarshalOptions{Deterministic: true}).Marshal(manifest)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(payload)
	return payload, hex.EncodeToString(hash[:]), nil
}

func manifestChunkRefs(manifest *workspacev1.Manifest) []casindex.ChunkRef {
	refs := make([]casindex.ChunkRef, 0)
	seen := map[string]struct{}{}
	for _, entry := range manifest.GetEntries() {
		for _, chunk := range entry.GetChunks() {
			if _, exists := seen[chunk.GetDigest()]; exists {
				continue
			}
			seen[chunk.GetDigest()] = struct{}{}
			refs = append(refs, casindex.ChunkRef{Digest: chunk.GetDigest(), Size: chunk.GetSize()})
		}
	}
	slices.SortFunc(refs, func(a, b casindex.ChunkRef) int { return strings.Compare(a.Digest, b.Digest) })
	return refs
}

func missingChunkBytes(manifest *workspacev1.Manifest, missing map[string]struct{}) uint64 {
	var total uint64
	for _, ref := range manifestChunkRefs(manifest) {
		if _, isMissing := missing[ref.Digest]; isMissing {
			total = saturatingWorkspaceBytes(total, ref.Size)
		}
	}
	return total
}

func saturatingWorkspaceBytes(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

// RunCleanup drives the durable five-minute worker for an indexed Node.
func (s *Server) RunCleanup(ctx context.Context) error {
	if s.cleanup == nil {
		return nil
	}
	return s.cleanup.Run(ctx)
}

func (s *Server) StorageStatus() (casindex.StorageStatus, error) {
	if s.cleanup == nil {
		return casindex.StorageStatus{}, errors.New("workspace: storage index unavailable")
	}
	return s.cleanup.Status()
}

func (s *Server) Prune(ctx context.Context, dryRun bool) (casindex.Plan, error) {
	if s.cleanup == nil {
		return casindex.Plan{}, errors.New("workspace: storage index unavailable")
	}
	return s.cleanup.Prune(ctx, casindex.PruneSoft, 0, dryRun)
}
