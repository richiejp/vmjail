package workspacecatalog

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
)

const testWorkspaceName = "name"

func TestPersistentStoreRoundTripResetsRuntimeStatus(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "workspaces")
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	store, err := Open(path, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := store.Create(CreateInput{UUID: "workspace-a", Name: "alpha"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now = now.Add(time.Minute)
	want, err := store.Update(created.Metadata.UUID, MutateSpec, func(ws *Workspace) error {
		ws.Spec.ImageRef = "example.invalid/agent@sha256:abc"
		ws.Spec.TargetSandboxFingerprint = "sha256:node"
		ws.Spec.Mounts = []Mount{{Target: "/workspace", Source: "project"}}
		return nil
	})
	if err != nil {
		t.Fatalf("Update spec: %v", err)
	}
	_, err = store.Update(created.Metadata.UUID, MutateStatus, func(ws *Workspace) error {
		ws.Status.CurrentSandbox = &SandboxRef{UID: "runtime-only"}
		ws.Status.ObservedGeneration = ws.Metadata.Generation
		ws.Status.Lock.ExpiresAt = now.Add(time.Hour)
		return nil
	})
	if err != nil {
		t.Fatalf("Update status: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	reopened, err := Open(path, func() time.Time { return now.Add(time.Hour) })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("reopened Close: %v", err)
		}
	})
	got, err := reopened.Get(created.Metadata.UUID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata != want.Metadata {
		t.Fatalf("metadata = %#v, want %#v", got.Metadata, want.Metadata)
	}
	if got.Spec.ImageRef != want.Spec.ImageRef || len(got.Spec.Mounts) != 1 {
		t.Fatalf("spec = %#v, want %#v", got.Spec, want.Spec)
	}
	if got.Status.CurrentSandbox != nil || got.Status.ObservedGeneration != 0 || got.Status.Lock.IsHeld() {
		t.Fatalf("runtime status survived restart: %#v", got.Status)
	}
	if !got.Status.IsTrue(ConditionReady) {
		t.Fatalf("Ready was not reconstructed: %#v", got.Status.Conditions)
	}
}

func TestPersistentStoreRenameDeleteAndLocking(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "workspaces")
	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := store.Create(CreateInput{UUID: "workspace-a", Name: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Update(created.Metadata.UUID, MutateMetadata, func(ws *Workspace) error {
		ws.Metadata.Name = "after"
		return nil
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := Open(path, nil); err == nil {
		t.Fatal("second Open unexpectedly acquired Badger lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := reopened.LookupByAlias(created.Metadata.OwnerFingerprint, "after"); err != nil {
		t.Fatalf("LookupByAlias(after): %v", err)
	}
	if err := reopened.Delete(created.Metadata.UUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := reopened.Delete(created.Metadata.UUID); err != nil {
		t.Fatalf("idempotent Delete: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened: %v", err)
	}
	empty, err := Open(path, nil)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	defer func() {
		if err := empty.Close(); err != nil {
			t.Errorf("final Close: %v", err)
		}
	}()
	if got := empty.List(nil); len(got) != 0 {
		t.Fatalf("List after delete = %#v", got)
	}
}

func TestOpenRejectsInvalidPersistence(t *testing.T) {
	t.Parallel()
	if _, err := Open("", nil); err == nil {
		t.Fatal("Open(empty) succeeded")
	}
	if _, err := NewPersistent(nil, nil); err == nil {
		t.Fatal("NewPersistent(nil) succeeded")
	}

	tests := []struct {
		name  string
		key   []byte
		value []byte
		want  string
	}{
		{name: "json", key: []byte(workspacePrefix + "broken"), value: []byte("{"), want: "decode"},
		{name: "record", key: []byte(workspacePrefix + "broken"), value: []byte(`{"version":1}`), want: "empty uuid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspaces")
			db, err := badger.Open(badger.DefaultOptions(path).WithLogger(nil))
			if err != nil {
				t.Fatalf("badger.Open: %v", err)
			}
			if err := db.Update(func(txn *badger.Txn) error {
				if err := txn.Set([]byte("meta/schema_version"), []byte("1")); err != nil {
					return err
				}
				return txn.Set(tc.key, tc.value)
			}); err != nil {
				t.Fatalf("seed schema: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("seed Close: %v", err)
			}
			if _, err := Open(path, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidatePersistedWorkspace(t *testing.T) {
	t.Parallel()
	now := time.Now()
	valid := persistedWorkspace{
		Version:  workspaceRecordVersion,
		Metadata: Metadata{UUID: "id", Name: testWorkspaceName, Generation: 1, CreatedAt: now, UpdatedAt: now},
	}
	tests := []struct {
		name string
		edit func(*persistedWorkspace)
	}{
		{name: "version", edit: func(r *persistedWorkspace) { r.Version++ }},
		{name: "uuid", edit: func(r *persistedWorkspace) { r.Metadata.UUID = "" }},
		{name: "name", edit: func(r *persistedWorkspace) { r.Metadata.Name = "" }},
		{name: "generation", edit: func(r *persistedWorkspace) { r.Metadata.Generation = 0 }},
		{name: "created", edit: func(r *persistedWorkspace) { r.Metadata.CreatedAt = time.Time{} }},
		{name: "updated", edit: func(r *persistedWorkspace) { r.Metadata.UpdatedAt = time.Time{} }},
	}
	if err := validatePersistedWorkspace(valid); err != nil {
		t.Fatalf("valid record: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			record := valid
			tc.edit(&record)
			if err := validatePersistedWorkspace(record); err == nil {
				t.Fatal("invalid record accepted")
			}
		})
	}
}

func TestBadgerBackendReportsClosedDatabase(t *testing.T) {
	t.Parallel()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(t.TempDir(), "workspaces")).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	backend := &badgerBackend{db: db}
	ws := Workspace{Metadata: Metadata{UUID: "id"}}
	if err := backend.Save(ws); err == nil || !strings.Contains(err.Error(), "write workspace") {
		t.Fatalf("Save error = %v, want write workspace", err)
	}
	if err := backend.Delete(ws.Metadata.UUID); err == nil || !strings.Contains(err.Error(), "delete workspace") {
		t.Fatalf("Delete error = %v, want delete workspace", err)
	}
}

type failingBackend struct{ err error }

func (f *failingBackend) Save(Workspace) error { return f.err }
func (f *failingBackend) Delete(string) error  { return f.err }
func (f *failingBackend) Close() error         { return f.err }

func TestPersistenceFailureLeavesMemoryUnchanged(t *testing.T) {
	t.Parallel()
	backend := &failingBackend{err: errors.New("disk failed")}
	store := newStore(nil, backend)
	if _, err := store.Create(CreateInput{UUID: "id", Name: testWorkspaceName}); err == nil {
		t.Fatal("Create succeeded through failing backend")
	}
	if got := store.List(nil); len(got) != 0 {
		t.Fatalf("failed Create changed memory: %#v", got)
	}

	store.backend = nil
	created, err := store.Create(CreateInput{UUID: "id", Name: testWorkspaceName})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	store.backend = backend
	if _, err := store.Update(created.Metadata.UUID, MutateMetadata, func(ws *Workspace) error {
		ws.Metadata.Name = "renamed"
		return nil
	}); err == nil {
		t.Fatal("Update succeeded through failing backend")
	}
	if _, err := store.LookupByAlias(created.Metadata.OwnerFingerprint, testWorkspaceName); err != nil {
		t.Fatalf("old alias disappeared: %v", err)
	}
	if err := store.Delete(created.Metadata.UUID); err == nil {
		t.Fatal("Delete succeeded through failing backend")
	}
	if _, err := store.Get(created.Metadata.UUID); err != nil {
		t.Fatalf("failed Delete changed memory: %v", err)
	}
	if err := store.Close(); !errors.Is(err, backend.err) {
		t.Fatalf("Close error = %v, want %v", err, backend.err)
	}
	if _, err := store.Get(created.Metadata.UUID); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v, want ErrClosed", err)
	}
	if err := store.Delete(created.Metadata.UUID); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after Close = %v, want ErrClosed", err)
	}
	if _, err := store.Create(CreateInput{UUID: "other", Name: "other"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Create after Close = %v, want ErrClosed", err)
	}
	if _, err := store.LookupByAlias(created.Metadata.OwnerFingerprint, testWorkspaceName); !errors.Is(err, ErrClosed) {
		t.Fatalf("LookupByAlias after Close = %v, want ErrClosed", err)
	}
	if _, err := store.Update(created.Metadata.UUID, MutateStatus, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Update after Close = %v, want ErrClosed", err)
	}
	if got := store.List(nil); got != nil {
		t.Fatalf("List after Close = %#v, want nil", got)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestCloseInMemoryStore(t *testing.T) {
	t.Parallel()
	store := NewStore(nil)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
