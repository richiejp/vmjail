package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// Server implements WorkspaceServiceHandler over a CAS plus an
// in-memory registry of named manifests. The laptop running
// `daft workspace serve` constructs one of these, calls Add for
// each workspace it wants to expose, and hands it to mtls.Server.
type Server struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
	store     *cas.Store
	mu        sync.RWMutex
	manifests map[string]*workspacev1.Manifest
}

// NewServer returns an empty Server backed by store. Call Add to
// register workspaces before the server begins accepting traffic.
func NewServer(store *cas.Store) (*Server, error) {
	if store == nil {
		return nil, errors.New("workspace: nil cas store")
	}
	return &Server{
		store:     store,
		manifests: map[string]*workspacev1.Manifest{},
	}, nil
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
	// proto.Clone of a non-nil *Manifest always returns a
	// *Manifest; the assertion is an invariant check, not a
	// recoverable error.
	clone, _ := proto.Clone(m).(*workspacev1.Manifest)
	s.manifests[name] = clone
	return nil
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

// GetManifest implements the proto endpoint. Unknown workspaces
// return CodeNotFound; never the empty manifest, because an empty
// reply is indistinguishable from "the workspace genuinely has no
// files" and we want callers to be able to tell the two apart.
//
// We hand the registered manifest pointer straight back: Add
// already cloned on the way in, and Connect serialises the
// response onto the wire before the handler returns, so no caller
// can mutate it.
func (s *Server) GetManifest(_ context.Context, req *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	name := req.Msg.GetWorkspace()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("workspace: empty workspace name"))
	}
	s.mu.RLock()
	m, ok := s.manifests[name]
	s.mu.RUnlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("workspace: unknown workspace %q", name))
	}
	return connect.NewResponse(&workspacev1.GetManifestResponse{Manifest: m}), nil
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
func (s *Server) PutManifest(_ context.Context, req *connect.Request[workspacev1.PutManifestRequest]) (*connect.Response[workspacev1.PutManifestResponse], error) {
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
	if err := s.Add(name, m); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&workspacev1.PutManifestResponse{
		MissingDigests: missing,
	}), nil
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
		return connect.NewResponse(&workspacev1.PutChunkResponse{}), nil
	case errors.Is(err, cas.ErrInvalidDigest), errors.Is(err, cas.ErrCorrupt):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
}
