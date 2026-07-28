package workspace

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/trace"
)

type PushClient interface {
	PutManifest(context.Context, *connect.Request[workspacev1.PutManifestRequest]) (*connect.Response[workspacev1.PutManifestResponse], error)
	PutChunk(context.Context, *connect.Request[workspacev1.PutChunkRequest]) (*connect.Response[workspacev1.PutChunkResponse], error)
}

// syncKeySep separates the workspace name from the in-VM target in a
// SyncKey. NUL can't appear in either component (workspace names are
// validated identifiers; targets are sanitised paths), so it's an
// unambiguous joiner.
const syncKeySep = "\x00"

// SyncKey composes the opaque WorkspaceService key for one mount of a
// workspace. The server treats the result as a single string; the
// (workspace, target) split exists only so two mounts of the same
// workspace address distinct manifests.
func SyncKey(workspace, target string) string {
	return workspace + syncKeySep + target
}

// Push uploads m and the chunks it references to the server under
// name. It calls PutManifest to publish the manifest and learn which
// chunks the server lacks, then PutChunk each missing chunk straight
// from the local store's compressed bytes — no recompression. Chunks
// the server already holds (a re-push of a mostly-unchanged tree) are
// skipped, so steady-state pushes move only the delta.
func Push(
	ctx context.Context,
	client PushClient,
	store *cas.Store,
	name string,
	m *workspacev1.Manifest,
) error {
	if client == nil {
		return errors.New("workspace: nil client")
	}
	if store == nil {
		return errors.New("workspace: nil cas store")
	}
	if m == nil {
		return errors.New("workspace: nil manifest")
	}
	ctx, span := trace.Start(ctx, "workspace.Push",
		trace.WithAttributes(trace.KV{Key: "workspace", Value: name}))
	defer span.End()

	resp, err := client.PutManifest(ctx, connect.NewRequest(&workspacev1.PutManifestRequest{
		Workspace: name,
		Manifest:  m,
	}))
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("workspace: put manifest: %w", err))
	}
	for _, digest := range resp.Msg.GetMissingDigests() {
		if err := pushChunk(ctx, client, store, digest); err != nil {
			return trace.RecordError(ctx, err)
		}
	}
	return nil
}

// pushChunk reads one chunk's compressed bytes from the local store
// and uploads them. A missing local chunk is a programming error —
// the manifest just built from this store references it — so it
// surfaces rather than being skipped.
func pushChunk(
	ctx context.Context,
	client PushClient,
	store *cas.Store,
	digest string,
) error {
	compressed, err := store.GetCompressed(digest)
	if err != nil {
		return fmt.Errorf("workspace: read local chunk %s: %w", digest, err)
	}
	_, err = client.PutChunk(ctx, connect.NewRequest(&workspacev1.PutChunkRequest{
		Digest:     digest,
		Compressed: compressed,
	}))
	if err != nil {
		return fmt.Errorf("workspace: put chunk %s: %w", digest, err)
	}
	return nil
}

// Pull fetches the manifest published under name and materialises it
// at dest, pulling any missing chunks into store. It's the symmetric
// counterpart to Push: GetManifest followed by ApplyManifest. dest
// must already exist.
func Pull(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	name, dest string,
	opts ApplyOptions,
) error {
	if client == nil {
		return errors.New("workspace: nil client")
	}
	ctx, span := trace.Start(ctx, "workspace.Pull",
		trace.WithAttributes(trace.KV{Key: "workspace", Value: name}))
	defer span.End()

	resp, err := client.GetManifest(ctx, connect.NewRequest(&workspacev1.GetManifestRequest{
		Workspace: name,
	}))
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("workspace: get manifest: %w", err))
	}
	if err := ApplyManifest(ctx, client, store, resp.Msg.GetManifest(), dest, opts); err != nil {
		return err
	}
	if resp.Msg.GetManifestDigest() == "" || resp.Msg.GetGeneration() == 0 {
		return nil // Compatibility with pre-index WorkspaceService peers.
	}
	_, err = client.AcknowledgeManifest(ctx, connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
		Workspace: name, ManifestDigest: resp.Msg.GetManifestDigest(), Generation: resp.Msg.GetGeneration(),
	}))
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("workspace: acknowledge manifest: %w", err))
	}
	return nil
}
