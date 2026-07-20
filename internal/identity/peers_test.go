package identity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenPeerStoreRejectsEmptyPath(t *testing.T) {
	if _, err := OpenPeerStore(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestOpenPeerStoreCreatesEmptyOnMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	s, err := OpenPeerStore(path)
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
	if s.Path() != path {
		t.Errorf("Path = %q, want %q", s.Path(), path)
	}
}

func TestOpenPeerStoreRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := OpenPeerStore(path); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestOpenPeerStoreRejectsInvalidEntries(t *testing.T) {
	id, _ := Generate(RoleClient)
	tests := []struct {
		name string
		data string
	}{
		{name: "zero fingerprint", data: `[{"role":"client"}]`},
		{name: "invalid role", data: `[{"fingerprint":"` + id.Fingerprint().String() + `","role":"root"}]`},
		{name: "duplicate", data: `[{"fingerprint":"` + id.Fingerprint().String() + `"},{"fingerprint":"` + id.Fingerprint().String() + `"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "peers.json")
			if err := os.WriteFile(path, []byte(tt.data), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := OpenPeerStore(path); err == nil {
				t.Fatal("expected invalid entry error")
			}
		})
	}
}

func TestPeerStoreAddLookupRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, err := OpenPeerStore(path)
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), Label: "laptop", Role: RoleClient}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := s.Lookup(id.Fingerprint())
	if !ok {
		t.Fatal("Lookup returned false after Add")
	}
	if got.Label != "laptop" {
		t.Errorf("Label = %q, want laptop", got.Label)
	}
	if got.ApprovedAt.IsZero() {
		t.Error("ApprovedAt should have been auto-filled")
	}

	// Reopen and verify persistence.
	s2, err := OpenPeerStore(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	got2, ok := s2.Lookup(id.Fingerprint())
	if !ok {
		t.Fatal("entry vanished after reopen")
	}
	if got2.Fingerprint != id.Fingerprint() {
		t.Errorf("fingerprint mismatch after reopen")
	}
}

func TestPeerStoreAddRejectsZeroFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)
	if err := s.Add(Peer{}); err == nil {
		t.Fatal("expected error for zero fingerprint")
	}
}

func TestPeerStoreAddPreservesProvidedTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)
	id, _ := Generate(RoleClient)
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), ApprovedAt: when}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, _ := s.Lookup(id.Fingerprint())
	if !got.ApprovedAt.Equal(when) {
		t.Errorf("ApprovedAt = %v, want %v", got.ApprovedAt, when)
	}
}

func TestPeerStoreAddPreservesOriginalApprovalTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)
	id, _ := Generate(RoleClient)
	original := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), ApprovedAt: original}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), Label: "renamed", ApprovedAt: original.Add(time.Hour)}); err != nil {
		t.Fatalf("second Add: %v", err)
	}
	got, _ := s.Lookup(id.Fingerprint())
	if !got.ApprovedAt.Equal(original) {
		t.Errorf("ApprovedAt = %v, want original %v", got.ApprovedAt, original)
	}
}

func TestPeerStoreAddRejectsInvalidRole(t *testing.T) {
	s, _ := OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	id, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), Role: Role("root")}); err == nil {
		t.Fatal("expected invalid role error")
	}
}

func TestPeerStoreRemoveDeletesEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)
	id, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint()}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Remove(id.Fingerprint()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Lookup(id.Fingerprint()); ok {
		t.Error("entry still present after Remove")
	}
}

func TestPeerStoreRemoveIsIdempotentForMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)
	var fp Fingerprint
	for i := range fp {
		fp[i] = byte(i)
	}
	if err := s.Remove(fp); err != nil {
		t.Errorf("Remove of unknown fp = %v, want nil", err)
	}
}

func TestPeerStoreListIsSortedByApprovalThenFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id1, _ := Generate(RoleClient)
	id2, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id1.Fingerprint(), ApprovedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(Peer{Fingerprint: id2.Fingerprint(), ApprovedAt: t0}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Fingerprint != id2.Fingerprint() {
		t.Errorf("oldest entry first failed")
	}
}

func TestPeerStoreListTiebreaksOnFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)

	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idA, _ := Generate(RoleClient)
	idB, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: idA.Fingerprint(), ApprovedAt: when}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(Peer{Fingerprint: idB.Fingerprint(), ApprovedAt: when}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Tie on time → fingerprint string ascending.
	if got[0].Fingerprint.String() >= got[1].Fingerprint.String() {
		t.Errorf("not sorted by fingerprint on tie: %v", got)
	}
}

func TestPeerStoreAddReplacesDuplicateFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	s, _ := OpenPeerStore(path)
	id, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), Label: "old"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(Peer{Fingerprint: id.Fingerprint(), Label: "new"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, _ := s.Lookup(id.Fingerprint())
	if got.Label != "new" {
		t.Errorf("Label = %q, want %q", got.Label, "new")
	}
	if l := s.List(); len(l) != 1 {
		t.Errorf("List len = %d, want 1", len(l))
	}
}

// TestPeerStoreAddFailsWhenStorePathIsADir covers saveLocked's Rename
// error path: open the store at a normal path, then plant a non-empty
// directory at the path before calling Add. WriteFile to .tmp still
// succeeds (different path), Rename(tmp, path) fails because path is
// a non-empty directory.
func TestPeerStoreAddFailsWhenStorePathIsADir(t *testing.T) {
	root := t.TempDir()
	storePath := filepath.Join(root, "peers.json")
	s, err := OpenPeerStore(storePath)
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	// #nosec G301 — test-only, dir under t.TempDir().
	if err := os.MkdirAll(filepath.Join(storePath, "child"), 0o750); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint()}); err == nil {
		t.Fatal("expected error when store path is a dir")
	}
	if _, ok := s.Lookup(id.Fingerprint()); ok {
		t.Fatal("failed Add changed the live approval set")
	}
}

func TestPeerStoreFailedRemovePreservesLiveApproval(t *testing.T) {
	root := t.TempDir()
	storePath := filepath.Join(root, "peers.json")
	s, err := OpenPeerStore(storePath)
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	id, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint()}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("Remove store file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(storePath, "child"), 0o750); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if err := s.Remove(id.Fingerprint()); err == nil {
		t.Fatal("Remove succeeded with blocked persistence path")
	}
	if _, ok := s.Lookup(id.Fingerprint()); !ok {
		t.Fatal("failed Remove changed the live approval set")
	}
}

func TestPeerStoreSaveCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "subdir", "peers.json")
	s, err := OpenPeerStore(path)
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	id, _ := Generate(RoleClient)
	if err := s.Add(Peer{Fingerprint: id.Fingerprint()}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file written: %v", err)
	}
}

// TestNewMemoryPeerStore covers the in-memory variant used by
// brokered workspace attaches.
func TestNewMemoryPeerStore(t *testing.T) {
	var a, b Fingerprint
	a[0], b[0] = 1, 2
	s := NewMemoryPeerStore(a, b, Fingerprint{} /* skipped: zero */)
	if _, ok := s.Lookup(a); !ok {
		t.Error("missing fingerprint a")
	}
	if _, ok := s.Lookup(b); !ok {
		t.Error("missing fingerprint b")
	}
	if got := len(s.List()); got != 2 {
		t.Errorf("List len = %d, want 2 (zero skipped)", got)
	}
	if s.Path() != "" {
		t.Errorf("Path = %q, want empty for in-memory store", s.Path())
	}
	// Add / Remove on an in-memory store must not error despite
	// the empty path (saveLocked early-returns).
	var c Fingerprint
	c[0] = 3
	if err := s.Add(Peer{Fingerprint: c}); err != nil {
		t.Errorf("Add on mem store: %v", err)
	}
	if err := s.Remove(c); err != nil {
		t.Errorf("Remove on mem store: %v", err)
	}
	if _, ok := s.Lookup(c); ok {
		t.Error("Remove didn't take effect")
	}
}

// TestNewMemoryPeerStoreEmpty covers the zero-arg constructor.
func TestNewMemoryPeerStoreEmpty(t *testing.T) {
	s := NewMemoryPeerStore()
	if got := len(s.List()); got != 0 {
		t.Errorf("List len = %d, want 0", got)
	}
}

// TestOpenPeerStoreFailsWhenParentIsAFile exercises the read-error
// branch of OpenPeerStore (anything other than fs.ErrNotExist must be
// surfaced — a corrupt or unreadable file should never be silently
// treated as "empty store").
func TestOpenPeerStoreFailsWhenParentIsAFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := OpenPeerStore(filepath.Join(blocker, "peers.json")); err == nil {
		t.Fatal("expected error when reading a path beneath a regular file")
	}
}
