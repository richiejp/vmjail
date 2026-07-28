package casindex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/richiejp/darkaifactory/internal/cas"
)

const (
	chunkPrefix    = "chunk/"
	manifestPrefix = "manifest/"
	currentPrefix  = "current/"
	indexDirName   = "metadata.badger"
)

var (
	ErrStaleManifest   = errors.New("casindex: stale manifest generation or digest")
	ErrManifestMissing = errors.New("casindex: manifest not found")
	ErrChunkReferenced = errors.New("casindex: chunk became referenced before deletion")
)

type Index struct {
	db    *badger.DB
	store *cas.Store
	now   func() time.Time

	mu      sync.Mutex
	touchMu sync.Mutex
	touches map[string]time.Time
}

// Open creates the Badger metadata index beside casRoot's chunk fanout and
// imports unindexed legacy chunks as protected records.
func Open(casRoot string, now func() time.Time) (*Index, error) {
	if !filepath.IsAbs(casRoot) {
		return nil, fmt.Errorf("casindex: CAS root %q must be absolute", casRoot)
	}
	if now == nil {
		now = time.Now
	}
	store, err := cas.Open(casRoot)
	if err != nil {
		return nil, err
	}
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(casRoot, indexDirName)).WithLogger(nil))
	if err != nil {
		return nil, fmt.Errorf("casindex: open Badger: %w", err)
	}
	index := &Index{db: db, store: store, now: now, touches: map[string]time.Time{}}
	if err := index.migrateLegacy(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	if err := i.FlushAccesses(); err != nil {
		return err
	}
	return i.db.Close()
}

func (i *Index) migrateLegacy() error {
	root := filepath.Join(i.store.Root(), "sha256")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		digest := entry.Name()
		if !validDigest(digest) {
			return nil
		}
		if _, err := i.Chunk(digest); err == nil {
			return nil
		} else if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		reader, err := i.store.Get(digest)
		if err != nil {
			return err
		}
		size, copyErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		chunk := Chunk{
			Digest: digest, Size: uint64(size), LastUse: info.ModTime(), Origin: OriginLegacy, //nolint:gosec // io.Copy byte counts are non-negative.
			Recoverability: RecoverabilityProtected, Disposition: DispositionActive,
		}
		return i.saveChunk(chunk)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("casindex: migrate legacy chunks: %w", err)
	}
	return nil
}

func (i *Index) Chunk(digest string) (Chunk, error) {
	var chunk Chunk
	err := i.db.View(func(txn *badger.Txn) error { return getJSON(txn, chunkKey(digest), &chunk) })
	return chunk, err
}

func (i *Index) Manifest(syncKey, digest string, generation uint64) (Manifest, error) {
	var manifest Manifest
	err := i.db.View(func(txn *badger.Txn) error {
		return getJSON(txn, manifestKey(syncKey, generation, digest), &manifest)
	})
	return manifest, err
}

// CommitManifest atomically publishes an exact manifest generation only after
// every referenced chunk exists. Repeating the same digest/generation is
// idempotent; stale or conflicting commits are rejected.
func (i *Index) CommitManifest(request CommitRequest) error {
	if request.SyncKey == "" || request.Generation == 0 || !validDigest(request.Digest) {
		return errors.New("casindex: invalid manifest identity")
	}
	if request.Origin == OriginSandbox {
		request.Recoverability = RecoverabilityProtected
	}
	if request.Recoverability == "" {
		return errors.New("casindex: manifest recoverability is required")
	}
	var err error
	request.Chunks, err = uniqueChunkRefs(request.Chunks)
	if err != nil {
		return err
	}
	for _, ref := range request.Chunks {
		has, err := i.store.Has(ref.Digest)
		if err != nil {
			return err
		}
		if !has {
			return fmt.Errorf("casindex: manifest chunk %s is missing", ref.Digest)
		}
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	return i.db.Update(func(txn *badger.Txn) error {
		var previous Manifest
		hasPrevious, err := getOptionalJSON(txn, currentKey(request.SyncKey), &previous)
		if err != nil {
			return err
		}
		if hasPrevious {
			if previous.Generation == request.Generation && previous.Digest == request.Digest {
				return nil
			}
			if request.Generation <= previous.Generation {
				return ErrStaleManifest
			}
			previous.Current = false
			previous.Disposition = DispositionSuperseded
			if err := setJSON(txn, manifestKey(previous.SyncKey, previous.Generation, previous.Digest), previous); err != nil {
				return err
			}
			if err := i.removeManifestReferences(txn, previous, false); err != nil {
				return err
			}
		}
		manifest := Manifest{
			SyncKey: request.SyncKey, Digest: request.Digest, Generation: request.Generation,
			Chunks: request.Chunks, Origin: request.Origin, Recoverability: request.Recoverability,
			Disposition: DispositionActive, Current: true, CreatedAt: i.now(),
		}
		if err := i.addManifestReferences(txn, manifest); err != nil {
			return err
		}
		if err := setJSON(txn, manifestKey(manifest.SyncKey, manifest.Generation, manifest.Digest), manifest); err != nil {
			return err
		}
		return setJSON(txn, currentKey(manifest.SyncKey), manifest)
	})
}

// Acknowledge adopts only the exact current manifest. A stale client cannot
// make newer sandbox writes recoverable.
func (i *Index) Acknowledge(syncKey, digest string, generation uint64) error {
	return i.transitionExact(syncKey, digest, generation, func(txn *badger.Txn, manifest *Manifest) error {
		manifest.Origin = OriginClientAdopted
		manifest.Recoverability = RecoverabilityRecoverable
		for _, ref := range manifest.Chunks {
			chunk, err := chunkInTxn(txn, ref.Digest)
			if err != nil {
				return err
			}
			self := Reference{SyncKey: manifest.SyncKey, Digest: manifest.Digest, Generation: manifest.Generation}
			protected, err := hasProtectedReference(txn, chunk.References, &self)
			if err != nil {
				return err
			}
			if protected {
				chunk.Recoverability = RecoverabilityProtected
			} else {
				chunk.Origin = OriginClientAdopted
				chunk.Recoverability = RecoverabilityRecoverable
			}
			if err := setJSON(txn, chunkKey(chunk.Digest), chunk); err != nil {
				return err
			}
		}
		return nil
	})
}

// Reject makes the exact unadopted current manifest eligible and restores the
// newest prior recoverable manifest, when one exists.
func (i *Index) Reject(syncKey, digest string, generation uint64) error {
	return i.transitionExact(syncKey, digest, generation, func(txn *badger.Txn, manifest *Manifest) error {
		manifest.Current = false
		manifest.Disposition = DispositionRejected
		manifest.Recoverability = RecoverabilityRecoverable
		if err := i.removeManifestReferences(txn, *manifest, true); err != nil {
			return err
		}
		restored, ok, err := newestRecoverable(txn, syncKey, generation)
		if err != nil {
			return err
		}
		if !ok {
			return txn.Delete(currentKey(syncKey))
		}
		restored.Current = true
		restored.Disposition = DispositionActive
		if err := i.addManifestReferences(txn, restored); err != nil {
			return err
		}
		if err := setJSON(txn, manifestKey(restored.SyncKey, restored.Generation, restored.Digest), restored); err != nil {
			return err
		}
		return setJSON(txn, currentKey(syncKey), restored)
	})
}

func (i *Index) transitionExact(syncKey, digest string, generation uint64, mutate func(*badger.Txn, *Manifest) error) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.db.Update(func(txn *badger.Txn) error {
		var current Manifest
		if err := getJSON(txn, currentKey(syncKey), &current); err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return ErrManifestMissing
			}
			return err
		}
		if current.Digest != digest || current.Generation != generation {
			return ErrStaleManifest
		}
		if err := mutate(txn, &current); err != nil {
			return err
		}
		if err := setJSON(txn, manifestKey(current.SyncKey, current.Generation, current.Digest), current); err != nil {
			return err
		}
		if current.Current {
			return setJSON(txn, currentKey(current.SyncKey), current)
		}
		return nil
	})
}

func (i *Index) addManifestReferences(txn *badger.Txn, manifest Manifest) error {
	for _, ref := range manifest.Chunks {
		chunk, err := chunkInTxn(txn, ref.Digest)
		if errors.Is(err, badger.ErrKeyNotFound) {
			chunk = Chunk{Digest: ref.Digest, Size: ref.Size, LastUse: i.now(), Disposition: DispositionActive}
		} else if err != nil {
			return err
		}
		hadProtectedReference := len(chunk.References) > 0 && chunk.Recoverability == RecoverabilityProtected
		chunk.Size = ref.Size
		chunk.LastUse = i.now()
		chunk.Origin = manifest.Origin
		if hadProtectedReference || manifest.Recoverability == RecoverabilityProtected {
			chunk.Recoverability = RecoverabilityProtected
		} else {
			chunk.Recoverability = RecoverabilityRecoverable
		}
		chunk.Disposition = DispositionActive
		chunk.QueuedAt = time.Time{}
		chunk.QueueScore = 0
		chunk.References = appendUniqueReference(chunk.References, Reference{
			SyncKey: manifest.SyncKey, Digest: manifest.Digest, Generation: manifest.Generation,
		})
		if err := setJSON(txn, chunkKey(chunk.Digest), chunk); err != nil {
			return err
		}
	}
	return nil
}

func (i *Index) removeManifestReferences(txn *badger.Txn, manifest Manifest, explicitlyRecoverable bool) error {
	for _, ref := range manifest.Chunks {
		chunk, err := chunkInTxn(txn, ref.Digest)
		if err != nil {
			return err
		}
		chunk.References = slices.DeleteFunc(chunk.References, func(candidate Reference) bool {
			return candidate.SyncKey == manifest.SyncKey && candidate.Digest == manifest.Digest && candidate.Generation == manifest.Generation
		})
		if len(chunk.References) == 0 {
			chunk.Disposition = manifest.Disposition
			if explicitlyRecoverable || manifest.Recoverability == RecoverabilityRecoverable {
				chunk.Recoverability = RecoverabilityRecoverable
			}
		} else {
			protected, err := hasProtectedReference(txn, chunk.References, nil)
			if err != nil {
				return err
			}
			chunk.Disposition = DispositionActive
			if protected {
				chunk.Recoverability = RecoverabilityProtected
			} else {
				chunk.Recoverability = RecoverabilityRecoverable
			}
		}
		if err := setJSON(txn, chunkKey(chunk.Digest), chunk); err != nil {
			return err
		}
	}
	return nil
}

func hasProtectedReference(txn *badger.Txn, references []Reference, skip *Reference) (bool, error) {
	for _, reference := range references {
		if skip != nil && reference == *skip {
			continue
		}
		var manifest Manifest
		if err := getJSON(txn, manifestKey(reference.SyncKey, reference.Generation, reference.Digest), &manifest); err != nil {
			return false, err
		}
		if manifest.Recoverability == RecoverabilityProtected {
			return true, nil
		}
	}
	return false, nil
}

// DeleteWorkspace releases protection for every current mount manifest owned
// by workspaceID. Historical manifests remain as audit/provenance records.
func (i *Index) DeleteWorkspace(workspaceID string) error {
	if workspaceID == "" {
		return errors.New("casindex: empty workspace ID")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.db.Update(func(txn *badger.Txn) error {
		prefix := []byte(currentPrefix)
		iterator := txn.NewIterator(badger.DefaultIteratorOptions)
		defer iterator.Close()
		var currents []Manifest
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			var manifest Manifest
			if err := iterator.Item().Value(func(value []byte) error { return json.Unmarshal(value, &manifest) }); err != nil {
				return err
			}
			if strings.HasPrefix(manifest.SyncKey, workspaceID+"\x00") {
				currents = append(currents, manifest)
			}
		}
		for manifestIndex := range currents {
			manifest := currents[manifestIndex]
			manifest.Current = false
			manifest.Disposition = DispositionDeleted
			manifest.Recoverability = RecoverabilityRecoverable
			if err := i.removeManifestReferences(txn, manifest, true); err != nil {
				return err
			}
			if err := setJSON(txn, manifestKey(manifest.SyncKey, manifest.Generation, manifest.Digest), manifest); err != nil {
				return err
			}
			if err := txn.Delete(currentKey(manifest.SyncKey)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (i *Index) Touch(digest string) {
	i.touchMu.Lock()
	i.touches[digest] = i.now()
	i.touchMu.Unlock()
}

func (i *Index) FlushAccesses() error {
	i.touchMu.Lock()
	touches := i.touches
	i.touches = map[string]time.Time{}
	i.touchMu.Unlock()
	if len(touches) == 0 {
		return nil
	}
	return i.db.Update(func(txn *badger.Txn) error {
		for digest, accessed := range touches {
			chunk, err := chunkInTxn(txn, digest)
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if accessed.After(chunk.LastUse) {
				chunk.LastUse = accessed
			}
			if err := setJSON(txn, chunkKey(digest), chunk); err != nil {
				return err
			}
		}
		return nil
	})
}

func (i *Index) QueuePlan(plan Plan) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	queuedAt := i.now()
	return i.db.Update(func(txn *badger.Txn) error {
		for _, candidate := range plan.Candidates {
			chunk, err := chunkInTxn(txn, candidate.Digest)
			if err != nil {
				return err
			}
			if !eligible(&chunk) {
				continue
			}
			chunk.QueuedAt = queuedAt
			chunk.QueueScore = candidate.Score
			if err := setJSON(txn, chunkKey(chunk.Digest), chunk); err != nil {
				return err
			}
		}
		return nil
	})
}

func (i *Index) Chunks() ([]Chunk, error) {
	var chunks []Chunk
	err := i.db.View(func(txn *badger.Txn) error {
		iterator := txn.NewIterator(badger.DefaultIteratorOptions)
		defer iterator.Close()
		prefix := []byte(chunkPrefix)
		for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
			var chunk Chunk
			if err := iterator.Item().Value(func(value []byte) error { return json.Unmarshal(value, &chunk) }); err != nil {
				return err
			}
			chunks = append(chunks, chunk)
		}
		return nil
	})
	return chunks, err
}

func (i *Index) Queued(limit int) ([]Chunk, error) {
	if limit <= 0 {
		return nil, nil
	}
	chunks, err := i.Chunks()
	if err != nil {
		return nil, err
	}
	chunks = slices.DeleteFunc(chunks, func(chunk Chunk) bool { return chunk.QueuedAt.IsZero() })
	slices.SortFunc(chunks, func(a, b Chunk) int {
		if a.QueueScore != b.QueueScore {
			if a.QueueScore > b.QueueScore {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	return chunks[:min(limit, len(chunks))], nil
}

// DeleteQueued revalidates protection and references immediately before
// unlinking. A crash after unlink but before the metadata delete is recovered
// by the next call, which treats an already-missing CAS file as deleted.
func (i *Index) DeleteQueued(_ context.Context, digest string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	var chunk Chunk
	if err := i.db.View(func(txn *badger.Txn) error { return getJSON(txn, chunkKey(digest), &chunk) }); err != nil {
		return err
	}
	if chunk.QueuedAt.IsZero() || !eligible(&chunk) {
		return ErrChunkReferenced
	}
	if err := i.store.Delete(digest); err != nil {
		return err
	}
	return i.db.Update(func(txn *badger.Txn) error { return txn.Delete(chunkKey(digest)) })
}

func (i *Index) saveChunk(chunk Chunk) error {
	return i.db.Update(func(txn *badger.Txn) error { return setJSON(txn, chunkKey(chunk.Digest), chunk) })
}

func newestRecoverable(txn *badger.Txn, syncKey string, before uint64) (Manifest, bool, error) {
	prefix := []byte(manifestPrefix + encodeKey(syncKey) + "/")
	iterator := txn.NewIterator(badger.DefaultIteratorOptions)
	defer iterator.Close()
	var newest Manifest
	found := false
	for iterator.Seek(prefix); iterator.ValidForPrefix(prefix); iterator.Next() {
		var candidate Manifest
		if err := iterator.Item().Value(func(value []byte) error { return json.Unmarshal(value, &candidate) }); err != nil {
			return Manifest{}, false, err
		}
		if candidate.Generation < before && candidate.Recoverability == RecoverabilityRecoverable &&
			candidate.Disposition != DispositionRejected && candidate.Disposition != DispositionDeleted &&
			(!found || candidate.Generation > newest.Generation) {
			newest, found = candidate, true
		}
	}
	return newest, found, nil
}

func appendUniqueReference(references []Reference, reference Reference) []Reference {
	for _, existing := range references {
		if existing == reference {
			return references
		}
	}
	return append(references, reference)
}

func uniqueChunkRefs(refs []ChunkRef) ([]ChunkRef, error) {
	seen := map[string]uint64{}
	result := make([]ChunkRef, 0, len(refs))
	for _, ref := range refs {
		if !validDigest(ref.Digest) {
			return nil, fmt.Errorf("casindex: invalid chunk digest %q", ref.Digest)
		}
		if size, exists := seen[ref.Digest]; exists {
			if size != ref.Size {
				return nil, fmt.Errorf("casindex: conflicting sizes for chunk %s", ref.Digest)
			}
			continue
		}
		seen[ref.Digest] = ref.Size
		result = append(result, ref)
	}
	return result, nil
}

func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, char := range digest {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func chunkKey(digest string) []byte { return []byte(chunkPrefix + digest) }

func manifestKey(syncKey string, generation uint64, digest string) []byte {
	return []byte(manifestPrefix + encodeKey(syncKey) + "/" + fmt.Sprintf("%020d", generation) + "/" + digest)
}

func currentKey(syncKey string) []byte { return []byte(currentPrefix + encodeKey(syncKey)) }

func encodeKey(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }

func chunkInTxn(txn *badger.Txn, digest string) (Chunk, error) {
	var chunk Chunk
	err := getJSON(txn, chunkKey(digest), &chunk)
	return chunk, err
}

func setJSON(txn *badger.Txn, key []byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return txn.Set(key, encoded)
}

func getJSON(txn *badger.Txn, key []byte, destination any) error {
	item, err := txn.Get(key)
	if err != nil {
		return err
	}
	return item.Value(func(value []byte) error { return json.Unmarshal(value, destination) })
}

func getOptionalJSON(txn *badger.Txn, key []byte, destination any) (bool, error) {
	err := getJSON(txn, key, destination)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	return err == nil, err
}
