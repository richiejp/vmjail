package casindex

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/richiejp/darkaifactory/internal/cas"
)

const testSyncKey = "workspace\x00/workspace"

type indexRig struct {
	root  string
	store *cas.Store
	index *Index
	now   time.Time
}

func newIndexRig(t *testing.T, contents ...string) (*indexRig, []ChunkRef) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]ChunkRef, 0, len(contents))
	for _, content := range contents {
		digest, size, err := store.Put(bytes.NewBufferString(content))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ChunkRef{Digest: digest, Size: size})
	}
	now := time.Unix(2_000_000, 0)
	index, err := Open(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return &indexRig{root: root, store: store, index: index, now: now}, refs
}

func manifestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func commitRequest(generation uint64, digest string, refs []ChunkRef, origin Origin, recoverability Recoverability) CommitRequest {
	return CommitRequest{
		SyncKey: testSyncKey, Digest: digest, Generation: generation, Chunks: refs,
		Origin: origin, Recoverability: recoverability,
	}
}

func TestOpenMigratesLegacyChunksProtectedAndPersists(t *testing.T) {
	rig, refs := newIndexRig(t, "legacy payload")
	chunk, err := rig.index.Chunk(refs[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Size != refs[0].Size || chunk.Origin != OriginLegacy || chunk.Recoverability != RecoverabilityProtected {
		t.Fatalf("migrated chunk = %#v", chunk)
	}
	if err := rig.index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(rig.root, func() time.Time { return rig.now.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.Chunk(refs[0].Digest)
	if err != nil || got.Digest != chunk.Digest || got.Size != chunk.Size {
		t.Fatalf("reopened chunk = %#v, %v", got, err)
	}
}

func TestWorkspaceStatusCountsUniqueProtectedChunksAndStableManifests(t *testing.T) {
	rig, refs := newIndexRig(t, "shared", "only protected", "recoverable")
	requests := []CommitRequest{
		{SyncKey: "workspace\x00/b", Digest: manifestDigest("b"), Generation: 2, Chunks: refs[:2], Origin: OriginSandbox, Recoverability: RecoverabilityProtected},
		{SyncKey: "workspace\x00/a", Digest: manifestDigest("a"), Generation: 1, Chunks: refs[:1], Origin: OriginSandbox, Recoverability: RecoverabilityProtected},
		{SyncKey: "workspace\x00/c", Digest: manifestDigest("c"), Generation: 3, Chunks: refs[2:], Origin: OriginClientUpload, Recoverability: RecoverabilityRecoverable},
		{SyncKey: "other\x00/a", Digest: manifestDigest("other"), Generation: 1, Chunks: refs[:1], Origin: OriginSandbox, Recoverability: RecoverabilityProtected},
	}
	for _, request := range requests {
		if err := rig.index.CommitManifest(request); err != nil {
			t.Fatal(err)
		}
	}
	status, err := rig.index.WorkspaceStatus("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Manifests) != 3 || status.Manifests[0].SyncKey != "workspace\x00/a" || status.Manifests[2].SyncKey != "workspace\x00/c" {
		t.Fatalf("manifests = %#v", status.Manifests)
	}
	if want := refs[0].Size + refs[1].Size; status.ProtectedBytes != want {
		t.Fatalf("protected bytes = %d, want %d", status.ProtectedBytes, want)
	}
	if _, err := rig.index.WorkspaceStatus(""); err == nil {
		t.Fatal("empty workspace status succeeded")
	}
}

func TestCommitManifestBatchesLargeChunkFanout(t *testing.T) {
	rig, _ := newIndexRig(t)
	const chunkCount = 4096
	refs := make([]ChunkRef, 0, chunkCount)
	for index := range chunkCount {
		digest, size, err := rig.store.Put(bytes.NewBufferString(fmt.Sprintf("chunk-%d", index)))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ChunkRef{Digest: digest, Size: size})
	}
	payload := bytes.Repeat([]byte("manifest-entry\n"), 1<<15)
	digest := manifestDigest("large-fanout")
	if err := rig.index.CommitManifest(CommitRequest{
		SyncKey: testSyncKey, Digest: digest, Generation: 1, Chunks: refs,
		Origin: OriginSandbox, Recoverability: RecoverabilityProtected, Payload: payload,
	}); err != nil {
		t.Fatalf("CommitManifest large fanout: %v", err)
	}
	manifest, err := rig.index.Manifest(testSyncKey, digest, 1)
	if err != nil || len(manifest.Chunks) != chunkCount || !bytes.Equal(manifest.Payload, payload) {
		t.Fatalf("large manifest = %d chunks, %d payload bytes, %v", len(manifest.Chunks), len(manifest.Payload), err)
	}
	if err := rig.index.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(currentKey(testSyncKey))
		if err != nil {
			return err
		}
		return item.Value(func(value []byte) error {
			if len(value) > 256 {
				return fmt.Errorf("current pointer grew to %d bytes", len(value))
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.Acknowledge(testSyncKey, digest, 1); err != nil {
		t.Fatalf("Acknowledge large fanout: %v", err)
	}
	rig.index.now = func() time.Time { return rig.now.Add(time.Hour) }
	plan := Plan{Candidates: make([]Candidate, 0, len(refs))}
	for _, ref := range refs {
		rig.index.Touch(ref.Digest)
		plan.Candidates = append(plan.Candidates, Candidate{Digest: ref.Digest, Score: 0.5})
	}
	if err := rig.index.FlushAccesses(); err != nil {
		t.Fatalf("FlushAccesses large batch: %v", err)
	}
	if err := rig.index.QueuePlan(plan); err != nil {
		t.Fatalf("QueuePlan large batch: %v", err)
	}
	if err := rig.index.DeleteManifest(testSyncKey, digest, 1); err != nil {
		t.Fatalf("DeleteManifest large fanout: %v", err)
	}
	chunk, err := rig.index.Chunk(refs[len(refs)-1].Digest)
	if err != nil || len(chunk.References) != 0 || chunk.Recoverability != RecoverabilityRecoverable {
		t.Fatalf("last chunk after large deletion = %#v, %v", chunk, err)
	}
}

func TestOpenRepairsUnpublishedStagedReferences(t *testing.T) {
	rig, refs := newIndexRig(t, "staged", "recoverable", "protected payload")
	staged := Manifest{
		SyncKey: testSyncKey, Digest: manifestDigest("staged"), Generation: 1,
		Chunks: refs, Origin: OriginSandbox, Recoverability: RecoverabilityProtected,
		Disposition: DispositionActive, CreatedAt: rig.now,
	}
	if err := rig.index.db.Update(func(txn *badger.Txn) error { return setManifest(txn, staged) }); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.addManifestReferencesBatched(staged); err != nil {
		t.Fatal(err)
	}
	missing := Reference{SyncKey: "missing", Digest: manifestDigest("missing"), Generation: 1}
	if err := rig.index.db.Update(func(txn *badger.Txn) error {
		chunk, err := chunkInTxn(txn, refs[0].Digest)
		if err != nil {
			return err
		}
		chunk.References = append(chunk.References, missing)
		return setJSON(txn, chunkKey(chunk.Digest), chunk)
	}); err != nil {
		t.Fatal(err)
	}
	recoverable := commitRequest(1, manifestDigest("recoverable-current"), refs[1:2], OriginClientUpload, RecoverabilityRecoverable)
	recoverable.SyncKey = "recoverable"
	if err := rig.index.CommitManifest(recoverable); err != nil {
		t.Fatal(err)
	}
	protected := commitRequest(1, manifestDigest("protected-current"), refs[2:], OriginSandbox, RecoverabilityProtected)
	protected.SyncKey = "protected-sync"
	if err := rig.index.CommitManifest(protected); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.db.Update(func(txn *badger.Txn) error {
		chunk, err := chunkInTxn(txn, refs[1].Digest)
		if err != nil {
			return err
		}
		chunk.Recoverability = RecoverabilityProtected
		return setJSON(txn, chunkKey(chunk.Digest), chunk)
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(rig.root, func() time.Time { return rig.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	chunk, err := reopened.Chunk(refs[0].Digest)
	if err != nil || len(chunk.References) != 0 {
		t.Fatalf("repaired staged reference = %#v, %v", chunk, err)
	}
	recovered, err := reopened.Chunk(refs[1].Digest)
	if err != nil || len(recovered.References) != 1 || recovered.Recoverability != RecoverabilityRecoverable {
		t.Fatalf("repaired recoverable reference = %#v, %v", recovered, err)
	}
	stillProtected, err := reopened.Chunk(refs[2].Digest)
	if err != nil || len(stillProtected.References) != 1 || stillProtected.Recoverability != RecoverabilityProtected {
		t.Fatalf("repaired protected reference = %#v, %v", stillProtected, err)
	}
}

func TestManifestExactAdoptionRejectAndRestore(t *testing.T) {
	rig, refs := newIndexRig(t, "client", "sandbox", "new sandbox")
	clientDigest := manifestDigest("client")
	if err := rig.index.CommitManifest(commitRequest(1, clientDigest, refs[:1], OriginClientUpload, RecoverabilityRecoverable)); err != nil {
		t.Fatal(err)
	}
	sandboxDigest := manifestDigest("sandbox")
	if err := rig.index.CommitManifest(commitRequest(2, sandboxDigest, refs[1:2], OriginSandbox, RecoverabilityRecoverable)); err != nil {
		t.Fatal(err)
	}
	protected, err := rig.index.Chunk(refs[1].Digest)
	if err != nil || protected.Recoverability != RecoverabilityProtected || len(protected.References) != 1 {
		t.Fatalf("sandbox chunk = %#v, %v", protected, err)
	}
	if err := rig.index.Acknowledge(testSyncKey, clientDigest, 1); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("stale acknowledgement = %v", err)
	}
	if err := rig.index.Acknowledge(testSyncKey, sandboxDigest, 2); err != nil {
		t.Fatal(err)
	}
	adopted, _ := rig.index.Chunk(refs[1].Digest)
	if adopted.Recoverability != RecoverabilityRecoverable || adopted.Origin != OriginClientAdopted {
		t.Fatalf("adopted chunk = %#v", adopted)
	}

	newDigest := manifestDigest("new sandbox")
	if err := rig.index.CommitManifest(commitRequest(3, newDigest, refs[2:], OriginSandbox, RecoverabilityProtected)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.Reject(testSyncKey, sandboxDigest, 2); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("stale rejection = %v", err)
	}
	if err := rig.index.Reject(testSyncKey, newDigest, 3); err != nil {
		t.Fatal(err)
	}
	rejected, _ := rig.index.Manifest(testSyncKey, newDigest, 3)
	if rejected.Current || rejected.Disposition != DispositionRejected {
		t.Fatalf("rejected manifest = %#v", rejected)
	}
	restored, _ := rig.index.Manifest(testSyncKey, sandboxDigest, 2)
	if !restored.Current || restored.Disposition != DispositionActive {
		t.Fatalf("restored manifest = %#v", restored)
	}
}

func TestSharedChunkRemainsProtectedUntilEveryManifestIsAdopted(t *testing.T) {
	rig, refs := newIndexRig(t, "shared")
	first := commitRequest(1, manifestDigest("first"), refs, OriginSandbox, RecoverabilityProtected)
	second := commitRequest(1, manifestDigest("second"), refs, OriginSandbox, RecoverabilityProtected)
	second.SyncKey = "other-workspace\x00/workspace"
	if err := rig.index.CommitManifest(first); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(second); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.Acknowledge(first.SyncKey, first.Digest, first.Generation); err != nil {
		t.Fatal(err)
	}
	chunk, _ := rig.index.Chunk(refs[0].Digest)
	if chunk.Recoverability != RecoverabilityProtected || len(chunk.References) != 2 {
		t.Fatalf("partially adopted shared chunk = %#v", chunk)
	}
	if err := rig.index.Acknowledge(second.SyncKey, second.Digest, second.Generation); err != nil {
		t.Fatal(err)
	}
	chunk, _ = rig.index.Chunk(refs[0].Digest)
	if chunk.Recoverability != RecoverabilityRecoverable {
		t.Fatalf("fully adopted shared chunk = %#v", chunk)
	}
}

func TestRejectFirstManifestLeavesNoCurrentReference(t *testing.T) {
	rig, refs := newIndexRig(t, "sandbox")
	digest := manifestDigest("first sandbox")
	if err := rig.index.CommitManifest(commitRequest(1, digest, refs, OriginSandbox, RecoverabilityProtected)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.Reject(testSyncKey, digest, 1); err != nil {
		t.Fatal(err)
	}
	chunk, _ := rig.index.Chunk(refs[0].Digest)
	if chunk.Recoverability != RecoverabilityRecoverable || chunk.Disposition != DispositionRejected || len(chunk.References) != 0 {
		t.Fatalf("rejected first chunk = %#v", chunk)
	}
	if err := rig.index.Acknowledge(testSyncKey, digest, 1); !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("rejected current still visible: %v", err)
	}
}

func TestCommitManifestTransactionsAndGenerationChecks(t *testing.T) {
	rig, refs := newIndexRig(t, "present")
	digest := manifestDigest("one")
	request := commitRequest(1, digest, refs, OriginClientUpload, RecoverabilityRecoverable)
	if err := rig.index.CommitManifest(request); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(request); err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("conflict"), refs, OriginClientUpload, RecoverabilityRecoverable)); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("conflicting generation = %v", err)
	}
	missing := ChunkRef{Digest: manifestDigest("missing"), Size: 7}
	if err := rig.index.CommitManifest(commitRequest(2, manifestDigest("two"), []ChunkRef{missing}, OriginSandbox, RecoverabilityProtected)); err == nil {
		t.Fatal("missing chunk manifest committed")
	}
	if _, err := rig.index.Manifest(testSyncKey, manifestDigest("two"), 2); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("failed transaction became visible: %v", err)
	}
	bad := commitRequest(2, manifestDigest("bad"), []ChunkRef{{Digest: "bad", Size: 1}}, OriginSandbox, RecoverabilityProtected)
	if err := rig.index.CommitManifest(bad); err == nil {
		t.Fatal("invalid chunk digest accepted")
	}
	bad = commitRequest(2, manifestDigest("bad sizes"), []ChunkRef{refs[0], {Digest: refs[0].Digest, Size: refs[0].Size + 1}}, OriginSandbox, RecoverabilityProtected)
	if err := rig.index.CommitManifest(bad); err == nil {
		t.Fatal("conflicting duplicate size accepted")
	}
}

func TestCommitManifestPropagatesCASProbeError(t *testing.T) {
	rig, _ := newIndexRig(t)
	digest := "aa" + strings.Repeat("0", 62)
	fanout := filepath.Join(rig.root, "sha256")
	if err := os.MkdirAll(fanout, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("aa", filepath.Join(fanout, "aa")); err != nil {
		t.Fatal(err)
	}
	request := commitRequest(1, manifestDigest("manifest"), []ChunkRef{{Digest: digest, Size: 1}}, OriginSandbox, RecoverabilityProtected)
	if err := rig.index.CommitManifest(request); err == nil {
		t.Fatal("CAS probe error ignored")
	}
}

func TestCommitManifestRejectsCorruptTransactionalState(t *testing.T) {
	t.Run("current record", func(t *testing.T) {
		rig, refs := newIndexRig(t, "chunk")
		if err := rig.index.db.Update(func(txn *badger.Txn) error {
			return txn.Set(currentKey(testSyncKey), []byte("{"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("manifest"), refs, OriginSandbox, RecoverabilityProtected)); err == nil {
			t.Fatal("corrupt current record accepted")
		}
	})

	t.Run("previous chunk record", func(t *testing.T) {
		rig, refs := newIndexRig(t, "old", "new")
		if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("old"), refs[:1], OriginSandbox, RecoverabilityProtected)); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.db.Update(func(txn *badger.Txn) error { return txn.Delete(chunkKey(refs[0].Digest)) }); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.CommitManifest(commitRequest(2, manifestDigest("new"), refs[1:], OriginSandbox, RecoverabilityProtected)); err == nil {
			t.Fatal("missing previous chunk record accepted")
		}
	})

	t.Run("new chunk record", func(t *testing.T) {
		rig, refs := newIndexRig(t, "chunk")
		if err := rig.index.db.Update(func(txn *badger.Txn) error {
			return txn.Set(chunkKey(refs[0].Digest), []byte("{"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("manifest"), refs, OriginSandbox, RecoverabilityProtected)); err == nil {
			t.Fatal("corrupt new chunk record accepted")
		}
	})
}

func TestCommitRebuildsMissingMetadataAndDeduplicatesRefs(t *testing.T) {
	rig, refs := newIndexRig(t, "chunk")
	if err := rig.index.db.Update(func(txn *badger.Txn) error { return txn.Delete(chunkKey(refs[0].Digest)) }); err != nil {
		t.Fatal(err)
	}
	request := commitRequest(1, manifestDigest("manifest"), []ChunkRef{refs[0], refs[0]}, OriginClientUpload, RecoverabilityRecoverable)
	request.Payload = []byte("manifest payload")
	if err := rig.index.CommitManifest(request); err != nil {
		t.Fatal(err)
	}
	chunk, err := rig.index.Chunk(refs[0].Digest)
	if err != nil || len(chunk.References) != 1 || chunk.Size != refs[0].Size {
		t.Fatalf("rebuilt chunk metadata = %#v, %v", chunk, err)
	}
	manifest, err := rig.index.Manifest(testSyncKey, request.Digest, request.Generation)
	if err != nil || len(manifest.Chunks) != 1 || !bytes.Equal(manifest.Payload, request.Payload) {
		t.Fatalf("deduplicated manifest = %#v, %v", manifest, err)
	}
	currents, err := rig.index.CurrentManifests()
	if err != nil || len(currents) != 1 || currents[0].SyncKey != testSyncKey || !bytes.Equal(currents[0].Payload, request.Payload) {
		t.Fatalf("current manifests = %#v, %v", currents, err)
	}
}

func TestSupersededSandboxChunkStaysProtected(t *testing.T) {
	rig, refs := newIndexRig(t, "old", "new")
	if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("old"), refs[:1], OriginSandbox, RecoverabilityProtected)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(commitRequest(2, manifestDigest("new"), refs[1:], OriginSandbox, RecoverabilityProtected)); err != nil {
		t.Fatal(err)
	}
	old, _ := rig.index.Chunk(refs[0].Digest)
	if old.Recoverability != RecoverabilityProtected || old.Disposition != DispositionSuperseded || len(old.References) != 0 {
		t.Fatalf("superseded sandbox chunk = %#v", old)
	}
}

func TestDeleteWorkspaceReleasesProtection(t *testing.T) {
	rig, refs := newIndexRig(t, "sandbox")
	digest := manifestDigest("sandbox")
	if err := rig.index.CommitManifest(commitRequest(1, digest, refs, OriginSandbox, RecoverabilityProtected)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteWorkspace(WorkspaceID("workspace")); err != nil {
		t.Fatal(err)
	}
	chunk, _ := rig.index.Chunk(refs[0].Digest)
	if chunk.Recoverability != RecoverabilityRecoverable || len(chunk.References) != 0 || chunk.Disposition != DispositionDeleted {
		t.Fatalf("deleted workspace chunk = %#v", chunk)
	}
	manifest, _ := rig.index.Manifest(testSyncKey, digest, 1)
	if manifest.Current || manifest.Disposition != DispositionDeleted {
		t.Fatalf("deleted manifest = %#v", manifest)
	}
	if err := rig.index.DeleteWorkspace(WorkspaceID("")); err == nil {
		t.Fatal("empty workspace deletion accepted")
	}
	if err := rig.index.DeleteWorkspace(WorkspaceID("absent")); err != nil {
		t.Fatalf("absent workspace deletion: %v", err)
	}
}

func TestDeleteManifestRequiresExactCurrentGeneration(t *testing.T) {
	rig, refs := newIndexRig(t, "sandbox")
	digest := manifestDigest("sandbox")
	if err := rig.index.CommitManifest(commitRequest(2, digest, refs, OriginSandbox, RecoverabilityProtected)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteManifest(testSyncKey, digest, 1); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("stale delete = %v, want ErrStaleManifest", err)
	}
	if err := rig.index.DeleteManifest(testSyncKey, digest, 2); err != nil {
		t.Fatal(err)
	}
	manifest, err := rig.index.Manifest(testSyncKey, digest, 2)
	if err != nil || manifest.Current || manifest.Disposition != DispositionDeleted || manifest.Recoverability != RecoverabilityRecoverable {
		t.Fatalf("deleted manifest = %#v, %v", manifest, err)
	}
	if err := rig.index.DeleteManifest(testSyncKey, digest, 2); !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("idempotent delete = %v, want ErrManifestMissing", err)
	}
}

func TestDeletingOneSharedWorkspacePreservesOtherProtection(t *testing.T) {
	rig, refs := newIndexRig(t, "shared")
	first := commitRequest(1, manifestDigest("first"), refs, OriginSandbox, RecoverabilityProtected)
	second := commitRequest(1, manifestDigest("second"), refs, OriginSandbox, RecoverabilityProtected)
	second.SyncKey = "other\x00/workspace"
	if err := rig.index.CommitManifest(first); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(second); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteWorkspace(WorkspaceID("workspace")); err != nil {
		t.Fatal(err)
	}
	chunk, _ := rig.index.Chunk(refs[0].Digest)
	if chunk.Recoverability != RecoverabilityProtected || chunk.Disposition != DispositionActive || len(chunk.References) != 1 {
		t.Fatalf("remaining shared protection = %#v", chunk)
	}
}

func TestDurableQueueRevalidationDeletionAndTouchBatch(t *testing.T) {
	rig, refs := newIndexRig(t, "queued", "referenced")
	queued := refs[0]
	if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("queued"), []ChunkRef{queued}, OriginClientUpload, RecoverabilityRecoverable)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(commitRequest(2, manifestDigest("replacement"), refs[1:], OriginClientUpload, RecoverabilityRecoverable)); err != nil {
		t.Fatal(err)
	}
	plan := Plan{Candidates: []Candidate{{Digest: queued.Digest, Size: queued.Size, Score: .8}}}
	if err := rig.index.QueuePlan(plan); err != nil {
		t.Fatal(err)
	}
	items, err := rig.index.Queued(1)
	if err != nil || len(items) != 1 || items[0].Digest != queued.Digest {
		t.Fatalf("queue = %#v, %v", items, err)
	}

	rig.index.Touch(queued.Digest)
	rig.index.now = func() time.Time { return rig.now.Add(2 * time.Hour) }
	rig.index.Touch(queued.Digest)
	if err := rig.index.FlushAccesses(); err != nil {
		t.Fatal(err)
	}
	touched, _ := rig.index.Chunk(queued.Digest)
	if !touched.LastUse.Equal(rig.now.Add(2 * time.Hour)) {
		t.Fatalf("last use = %v", touched.LastUse)
	}

	if err := rig.index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(rig.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	items, err = reopened.Queued(10)
	if err != nil || len(items) != 1 {
		t.Fatalf("recovered queue = %#v, %v", items, err)
	}
	if err := reopened.DeleteQueued(context.Background(), queued.Digest); err != nil {
		t.Fatal(err)
	}
	if has, err := rig.store.Has(queued.Digest); err != nil || has {
		t.Fatalf("deleted CAS chunk has=%v err=%v", has, err)
	}
	if _, err := reopened.Chunk(queued.Digest); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("deleted metadata error = %v", err)
	}
}

func TestQueuedRecoverableReferenceIsDeletedAndNeedsRepush(t *testing.T) {
	rig, refs := newIndexRig(t, "candidate")
	chunk := refs[0]
	legacy, _ := rig.index.Chunk(chunk.Digest)
	legacy.Recoverability = RecoverabilityRecoverable
	legacy.Disposition = DispositionRejected
	if err := rig.index.saveChunk(legacy); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.QueuePlan(Plan{Candidates: []Candidate{{Digest: chunk.Digest, Score: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(commitRequest(1, manifestDigest("reference"), refs, OriginClientUpload, RecoverabilityRecoverable)); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteQueued(t.Context(), chunk.Digest); err != nil {
		t.Fatalf("recoverable deletion error = %v", err)
	}
	if has, err := rig.store.Has(chunk.Digest); err != nil || has {
		t.Fatalf("recoverable CAS chunk has=%v err=%v", has, err)
	}
	manifest, err := rig.index.Manifest(testSyncKey, manifestDigest("reference"), 1)
	if err != nil || !manifest.NeedsRepush {
		t.Fatalf("affected manifest = %#v, %v", manifest, err)
	}
}

func TestQueueOrderingSkipsIneligibleAndRejectsUnqueuedDelete(t *testing.T) {
	rig, refs := newIndexRig(t, "low", "high", "protected")
	for index, ref := range refs {
		chunk, _ := rig.index.Chunk(ref.Digest)
		chunk.Disposition = DispositionRejected
		if index < 2 {
			chunk.Recoverability = RecoverabilityRecoverable
		}
		if err := rig.index.saveChunk(chunk); err != nil {
			t.Fatal(err)
		}
	}
	plan := Plan{Candidates: []Candidate{
		{Digest: refs[0].Digest, Score: .2},
		{Digest: refs[1].Digest, Score: .9},
		{Digest: refs[2].Digest, Score: 1},
	}}
	if err := rig.index.QueuePlan(plan); err != nil {
		t.Fatal(err)
	}
	queued, err := rig.index.Queued(10)
	if err != nil || len(queued) != 2 || queued[0].Digest != refs[1].Digest || queued[1].Digest != refs[0].Digest {
		t.Fatalf("ordered queue = %#v, %v", queued, err)
	}
	if err := rig.index.DeleteQueued(t.Context(), refs[2].Digest); !errors.Is(err, ErrChunkReferenced) {
		t.Fatalf("unqueued deletion = %v", err)
	}
	for _, ref := range refs[:2] {
		chunk, _ := rig.index.Chunk(ref.Digest)
		chunk.QueueScore = 1
		if err := rig.index.saveChunk(chunk); err != nil {
			t.Fatal(err)
		}
	}
	queued, err = rig.index.Queued(1)
	wantFirst := min(refs[0].Digest, refs[1].Digest)
	if err != nil || len(queued) != 1 || queued[0].Digest != wantFirst {
		t.Fatalf("tied limited queue = %#v, %v; want %s", queued, err, wantFirst)
	}
}

func TestTouchMissingChunkAndEmptyFlush(t *testing.T) {
	rig, _ := newIndexRig(t)
	if err := rig.index.FlushAccesses(); err != nil {
		t.Fatal(err)
	}
	rig.index.Touch(manifestDigest("missing"))
	if err := rig.index.FlushAccesses(); err != nil {
		t.Fatal(err)
	}
}

func TestTouchDoesNotMoveLastUseBackward(t *testing.T) {
	rig, refs := newIndexRig(t, "chunk")
	request := commitRequest(1, manifestDigest("manifest"), refs, OriginClientUpload, RecoverabilityRecoverable)
	if err := rig.index.CommitManifest(request); err != nil {
		t.Fatal(err)
	}
	rig.index.now = func() time.Time { return rig.now.Add(-time.Hour) }
	rig.index.Touch(refs[0].Digest)
	if err := rig.index.FlushAccesses(); err != nil {
		t.Fatal(err)
	}
	chunk, _ := rig.index.Chunk(refs[0].Digest)
	if !chunk.LastUse.Equal(rig.now) {
		t.Fatalf("last use moved backward to %v", chunk.LastUse)
	}
}

func TestOpenAndManifestValidationErrors(t *testing.T) {
	if _, err := Open("relative", nil); err == nil {
		t.Fatal("relative CAS root accepted")
	}
	rig, _ := newIndexRig(t)
	for _, request := range []CommitRequest{
		{},
		{SyncKey: testSyncKey, Digest: manifestDigest("x"), Generation: 1},
	} {
		if err := rig.index.CommitManifest(request); err == nil {
			t.Fatalf("invalid request accepted: %#v", request)
		}
	}
	if err := rig.index.Acknowledge(testSyncKey, manifestDigest("none"), 1); !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("missing acknowledgement = %v", err)
	}
	if queued, err := rig.index.Queued(0); err != nil || queued != nil {
		t.Fatalf("zero queue = %#v, %v", queued, err)
	}
	if err := (*Index)(nil).Close(); err != nil {
		t.Fatalf("nil close = %v", err)
	}
	if _, err := rig.index.Chunk(manifestDigest("absent")); !errors.Is(err, badger.ErrKeyNotFound) && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent chunk = %v", err)
	}
}

func TestOpenStorageAndMigrationErrors(t *testing.T) {
	rootFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(rootFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(rootFile, nil); err == nil {
		t.Fatal("file CAS root accepted")
	}

	badgerRoot := filepath.Join(t.TempDir(), "cas")
	if err := os.MkdirAll(badgerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badgerRoot, indexDirName), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(badgerRoot, nil); err == nil {
		t.Fatal("blocked Badger directory accepted")
	}

	corruptRoot := filepath.Join(t.TempDir(), "corrupt")
	digest := manifestDigest("corrupt")
	path := filepath.Join(corruptRoot, "sha256", digest[:2], digest[2:4], digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not zstd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(corruptRoot, nil); err == nil {
		t.Fatal("corrupt legacy chunk accepted")
	}
}

func TestLowLevelValidationAndPersistenceErrors(t *testing.T) {
	if validDigest("short") || validDigest(strings.Repeat("g", 64)) || !validDigest(strings.Repeat("a", 64)) {
		t.Fatal("digest validation mismatch")
	}
	reference := Reference{SyncKey: "key", Digest: manifestDigest("manifest"), Generation: 1}
	if got := appendUniqueReference([]Reference{reference}, reference); len(got) != 1 {
		t.Fatalf("duplicate reference appended: %#v", got)
	}
	rig, _ := newIndexRig(t)
	if err := rig.index.DeleteManifest("", manifestDigest("invalid-delete"), 1); err == nil {
		t.Fatal("invalid manifest deletion accepted")
	}
	if err := rig.index.DeleteManifest(testSyncKey, manifestDigest("missing-delete"), 1); !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("missing manifest deletion = %v", err)
	}
	if err := rig.index.db.Update(func(txn *badger.Txn) error {
		return setJSON(txn, []byte("bad-json"), make(chan struct{}))
	}); err == nil {
		t.Fatal("unmarshalable metadata accepted")
	}
	if err := rig.index.db.View(func(txn *badger.Txn) error {
		_, err := hasProtectedReference(txn, []Reference{reference}, nil)
		return err
	}); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("missing referenced manifest error = %v", err)
	}
	if err := rig.index.db.Close(); err != nil {
		t.Fatal(err)
	}
	closedDigest := manifestDigest("closed")
	closedOps := []struct {
		name string
		run  func() error
	}{
		{"commit", func() error {
			return rig.index.CommitManifest(commitRequest(1, closedDigest, nil, OriginClientUpload, RecoverabilityRecoverable))
		}},
		{"acknowledge", func() error { return rig.index.Acknowledge(testSyncKey, closedDigest, 1) }},
		{"reject", func() error { return rig.index.Reject(testSyncKey, closedDigest, 1) }},
		{"delete workspace", func() error { return rig.index.DeleteWorkspace(WorkspaceID("workspace")) }},
		{"queue plan", func() error { return rig.index.QueuePlan(Plan{}) }},
	}
	for _, operation := range closedOps {
		if err := operation.run(); err == nil {
			t.Errorf("%s succeeded on closed index", operation.name)
		}
	}
	if _, err := rig.index.Chunk(closedDigest); err == nil {
		t.Error("chunk read succeeded on closed index")
	}
	if _, err := rig.index.Manifest(testSyncKey, closedDigest, 1); err == nil {
		t.Error("manifest read succeeded on closed index")
	}
	if _, err := rig.index.Chunks(); err == nil {
		t.Error("chunk listing succeeded on closed index")
	}
	if _, err := rig.index.CurrentManifests(); err == nil {
		t.Error("manifest listing succeeded on closed index")
	}
	if _, err := rig.index.Queued(1); err == nil {
		t.Error("queue read succeeded on closed index")
	}
	if err := rig.index.DeleteQueued(t.Context(), closedDigest); err == nil {
		t.Error("queued deletion succeeded on closed index")
	}
	rig.index.Touch(manifestDigest("after-close"))
	if err := rig.index.Close(); err == nil {
		t.Fatal("closed index flush succeeded")
	}
}

func TestOpenFailsClosedOnCorruptRepairMetadata(t *testing.T) {
	t.Run("chunk", func(t *testing.T) {
		rig, refs := newIndexRig(t, "corrupt chunk")
		if err := rig.index.db.Update(func(txn *badger.Txn) error {
			return txn.Set(chunkKey(refs[0].Digest), []byte("{"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(rig.root, time.Now); err == nil {
			t.Fatal("corrupt chunk metadata accepted on startup")
		}
	})

	t.Run("referenced manifest", func(t *testing.T) {
		rig, refs := newIndexRig(t, "corrupt manifest")
		digest := manifestDigest("corrupt manifest")
		if err := rig.index.CommitManifest(commitRequest(1, digest, refs, OriginSandbox, RecoverabilityProtected)); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.db.Update(func(txn *badger.Txn) error {
			return txn.Set(manifestKey(testSyncKey, 1, digest), []byte("{"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(rig.root, time.Now); err == nil {
			t.Fatal("corrupt referenced manifest accepted on startup")
		}
	})
}

func TestDeleteQueuedMarksCurrentRecoverableManifestForRepush(t *testing.T) {
	rig, refs := newIndexRig(t, "recoverable current")
	digest := manifestDigest("recoverable current")
	request := commitRequest(1, digest, refs, OriginClientUpload, RecoverabilityRecoverable)
	if err := rig.index.CommitManifest(request); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.QueuePlan(Plan{Candidates: []Candidate{{Digest: refs[0].Digest, Score: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteQueued(t.Context(), refs[0].Digest); err != nil {
		t.Fatal(err)
	}
	manifest, err := rig.index.Manifest(testSyncKey, digest, 1)
	if err != nil || !manifest.NeedsRepush {
		t.Fatalf("deleted manifest = %#v, %v", manifest, err)
	}
}

func TestManifestScansRejectCorruptRecords(t *testing.T) {
	rig, refs := newIndexRig(t, "corrupt scan")
	digest := manifestDigest("corrupt scan")
	request := commitRequest(1, digest, refs, OriginClientUpload, RecoverabilityRecoverable)
	if err := rig.index.CommitManifest(request); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.QueuePlan(Plan{Candidates: []Candidate{{Digest: refs[0].Digest, Score: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.db.Update(func(txn *badger.Txn) error {
		return txn.Set(manifestKey(testSyncKey, 1, digest), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteQueued(t.Context(), refs[0].Digest); err == nil {
		t.Fatal("queued deletion accepted a corrupt referenced manifest")
	}
	if err := rig.index.db.View(func(txn *badger.Txn) error {
		_, _, err := newestRecoverable(txn, testSyncKey, 2)
		return err
	}); err == nil {
		t.Fatal("recoverable scan accepted a corrupt manifest")
	}
}

func TestCompactManifestDecodeRejectsCorruptMetadata(t *testing.T) {
	rig, _ := newIndexRig(t)
	digest := manifestDigest("compact-corrupt")
	key := manifestKey(testSyncKey, 1, digest)
	write := func(value any) {
		t.Helper()
		if err := rig.index.db.Update(func(txn *badger.Txn) error { return setJSON(txn, key, value) }); err != nil {
			t.Fatal(err)
		}
	}
	write(storedManifest{Version: 2})
	if _, err := rig.index.Manifest(testSyncKey, digest, 1); err == nil {
		t.Fatal("unsupported compact manifest version accepted")
	}
	write(storedManifest{Version: 1, Body: []byte("not gzip")})
	if _, err := rig.index.Manifest(testSyncKey, digest, 1); err == nil {
		t.Fatal("invalid compact manifest compression accepted")
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	write(storedManifest{Version: 1, Body: compressed.Bytes()})
	if _, err := rig.index.Manifest(testSyncKey, digest, 1); err == nil {
		t.Fatal("invalid compact manifest body accepted")
	}
	if err := rig.index.db.Update(func(txn *badger.Txn) error {
		return setJSON(txn, currentKey(testSyncKey), Reference{SyncKey: testSyncKey})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.index.CurrentManifests(); err == nil {
		t.Fatal("invalid current pointer accepted")
	}
	legacy := Manifest{SyncKey: testSyncKey, Digest: digest, Generation: 1, Payload: []byte("legacy")}
	write(legacy)
	decoded, err := rig.index.Manifest(testSyncKey, digest, 1)
	if err != nil || !bytes.Equal(decoded.Payload, legacy.Payload) {
		t.Fatalf("legacy manifest decode = %#v, %v", decoded, err)
	}
}

func TestPublishStagedManifestRejectsPointerRaces(t *testing.T) {
	rig, refs := newIndexRig(t, "current")
	currentRequest := commitRequest(1, manifestDigest("current"), refs, OriginClientUpload, RecoverabilityRecoverable)
	if err := rig.index.CommitManifest(currentRequest); err != nil {
		t.Fatal(err)
	}
	staged := Manifest{SyncKey: testSyncKey, Digest: manifestDigest("staged-race"), Generation: 2, Current: true}
	if err := rig.index.publishStagedManifest(testSyncKey, staged, Manifest{}, false); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("missing expected pointer race = %v", err)
	}
	wrongPrevious := Manifest{SyncKey: testSyncKey, Digest: manifestDigest("wrong"), Generation: 1}
	if err := rig.index.publishStagedManifest(testSyncKey, staged, wrongPrevious, true); !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("wrong expected pointer race = %v", err)
	}
	if err := rig.index.db.Update(func(txn *badger.Txn) error {
		return setJSON(txn, currentKey(testSyncKey), Reference{SyncKey: testSyncKey})
	}); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.publishStagedManifest(testSyncKey, staged, Manifest{}, false); err == nil {
		t.Fatal("corrupt current pointer accepted during publication")
	}
}

func TestSharedChunkRetainsStrongestRemainingRecoverability(t *testing.T) {
	rig, refs := newIndexRig(t, "shared")
	protectedA := commitRequest(1, manifestDigest("protected-a"), refs, OriginSandbox, RecoverabilityProtected)
	protectedA.SyncKey = "protected-a"
	protectedB := commitRequest(1, manifestDigest("protected-b"), refs, OriginSandbox, RecoverabilityProtected)
	protectedB.SyncKey = "protected-b"
	if err := rig.index.CommitManifest(protectedA); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(protectedB); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.Acknowledge(protectedB.SyncKey, protectedB.Digest, protectedB.Generation); err != nil {
		t.Fatal(err)
	}
	chunk, err := rig.index.Chunk(refs[0].Digest)
	if err != nil || chunk.Recoverability != RecoverabilityProtected {
		t.Fatalf("shared protected chunk = %#v, %v", chunk, err)
	}
	if err := rig.index.DeleteManifest(protectedA.SyncKey, protectedA.Digest, protectedA.Generation); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.DeleteManifest(protectedB.SyncKey, protectedB.Digest, protectedB.Generation); err != nil {
		t.Fatal(err)
	}

	recoverableA := commitRequest(1, manifestDigest("recoverable-a"), refs, OriginClientUpload, RecoverabilityRecoverable)
	recoverableA.SyncKey = "recoverable-a"
	recoverableB := commitRequest(1, manifestDigest("recoverable-b"), refs, OriginClientUpload, RecoverabilityRecoverable)
	recoverableB.SyncKey = "recoverable-b"
	if err := rig.index.CommitManifest(recoverableA); err != nil {
		t.Fatal(err)
	}
	if err := rig.index.CommitManifest(recoverableB); err != nil {
		t.Fatal(err)
	}
	replacement := commitRequest(2, manifestDigest("replacement"), nil, OriginClientUpload, RecoverabilityRecoverable)
	replacement.SyncKey = recoverableB.SyncKey
	if err := rig.index.CommitManifest(replacement); err != nil {
		t.Fatal(err)
	}
}

//nolint:funlen // The independent corruption subtests share one key-deletion fixture and are clearer together.
func TestCorruptMetadataAndDeletionFailures(t *testing.T) {
	deleteKey := func(t *testing.T, index *Index, key []byte) {
		t.Helper()
		if err := index.db.Update(func(txn *badger.Txn) error { return txn.Delete(key) }); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("adoption chunk missing", func(t *testing.T) {
		rig, refs := newIndexRig(t, "chunk")
		digest := manifestDigest("manifest")
		if err := rig.index.CommitManifest(commitRequest(1, digest, refs, OriginSandbox, RecoverabilityProtected)); err != nil {
			t.Fatal(err)
		}
		deleteKey(t, rig.index, chunkKey(refs[0].Digest))
		if err := rig.index.Acknowledge(testSyncKey, digest, 1); err == nil {
			t.Fatal("missing chunk metadata adopted")
		}
	})

	t.Run("rejection chunk missing", func(t *testing.T) {
		rig, refs := newIndexRig(t, "chunk")
		digest := manifestDigest("manifest")
		if err := rig.index.CommitManifest(commitRequest(1, digest, refs, OriginSandbox, RecoverabilityProtected)); err != nil {
			t.Fatal(err)
		}
		deleteKey(t, rig.index, chunkKey(refs[0].Digest))
		if err := rig.index.Reject(testSyncKey, digest, 1); err == nil {
			t.Fatal("missing chunk metadata rejected")
		}
	})

	t.Run("corrupt records", func(t *testing.T) {
		rig, refs := newIndexRig(t, "chunk")
		if err := rig.index.db.Update(func(txn *badger.Txn) error {
			if err := txn.Set(currentKey(testSyncKey), []byte("{")); err != nil {
				return err
			}
			return txn.Set(chunkKey(refs[0].Digest), []byte("{"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.Acknowledge(testSyncKey, manifestDigest("x"), 1); err == nil {
			t.Fatal("corrupt current metadata accepted")
		}
		if _, err := rig.index.Chunks(); err == nil {
			t.Fatal("corrupt chunk listing accepted")
		}
		if _, err := rig.index.Queued(1); err == nil {
			t.Fatal("corrupt queue listing accepted")
		}
	})

	t.Run("queue candidate missing", func(t *testing.T) {
		rig, _ := newIndexRig(t)
		missing := manifestDigest("missing")
		if err := rig.index.QueuePlan(Plan{Candidates: []Candidate{{Digest: missing}}}); !errors.Is(err, badger.ErrKeyNotFound) {
			t.Fatalf("missing queue candidate = %v", err)
		}
		if err := rig.index.DeleteQueued(t.Context(), missing); !errors.Is(err, badger.ErrKeyNotFound) {
			t.Fatalf("missing queued deletion = %v", err)
		}
	})

	t.Run("CAS unlink", func(t *testing.T) {
		rig, refs := newIndexRig(t, "chunk")
		chunk, _ := rig.index.Chunk(refs[0].Digest)
		chunk.Recoverability = RecoverabilityRecoverable
		chunk.Disposition = DispositionRejected
		if err := rig.index.saveChunk(chunk); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.QueuePlan(Plan{Candidates: []Candidate{{Digest: chunk.Digest, Score: 1}}}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(rig.root, "sha256", chunk.Digest[:2], chunk.Digest[2:4], chunk.Digest)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := rig.index.DeleteQueued(t.Context(), chunk.Digest); err == nil {
			t.Fatal("CAS unlink failure ignored")
		}
	})
}
