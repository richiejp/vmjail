package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

type failingArchiveWriter struct{ err error }

func (w failingArchiveWriter) Write([]byte) (int, error) { return 0, w.err }

const archiveFileName = "file"

func TestStreamMountArchivesManifestDirectlyFromCAS(t *testing.T) {
	server, store := mustNewServer(t)
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, archiveFileName), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(archiveFileName, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildManifest(source, store, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Add("workspace", manifest); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	applied, err := server.StreamMount(t.Context(), "workspace", &archive)
	if err != nil || !applied {
		t.Fatalf("StreamMount = %t, %v", applied, err)
	}
	entries := map[string]*tar.Header{}
	reader := tar.NewReader(&archive)
	//daft:lint:disable=forloopbound reason="bounded by the tar end-of-archive marker from StreamMount"
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		entries[header.Name] = header
		if header.Name == archiveFileName {
			payload, readErr := io.ReadAll(reader)
			if readErr != nil || string(payload) != "payload" {
				t.Fatalf("file payload = %q, %v", payload, readErr)
			}
		}
	}
	if entries["empty"].Typeflag != tar.TypeDir || entries[archiveFileName].Mode != 0o640 || entries["link"].Linkname != archiveFileName {
		t.Fatalf("archive entries = %#v", entries)
	}
}

func TestStreamMountEmptyValidationAndFailures(t *testing.T) {
	server, _ := mustNewServer(t)
	var archive bytes.Buffer
	applied, err := server.StreamMount(t.Context(), "missing", &archive)
	if err != nil || applied {
		t.Fatalf("missing = %t, %v", applied, err)
	}
	if _, err := tar.NewReader(&archive).Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("empty archive = %v", err)
	}
	if _, err := server.StreamMount(t.Context(), "", &archive); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := server.StreamMount(t.Context(), "missing", nil); err == nil {
		t.Fatal("nil destination accepted")
	}
	server.mu.Lock()
	server.manifests["bad"] = &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Path: "../escape"}}}
	server.manifests["kind"] = &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Path: "entry", Kind: workspacev1.EntryKind(99)}}}
	server.manifests["huge"] = &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Path: "huge", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Size: ^uint64(0)}}}
	server.manifests["missing-chunk"] = &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{
		Path: archiveFileName, Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Size: 1,
		Chunks: []*workspacev1.ChunkRef{{Digest: "sha256:missing", Size: 1}},
	}}}
	server.manifests["dir"] = &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Path: "dir", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR}}}
	server.mu.Unlock()
	if _, err := server.StreamMount(t.Context(), "bad", io.Discard); err == nil {
		t.Fatal("unsafe entry accepted")
	}
	if _, err := server.StreamMount(t.Context(), "kind", io.Discard); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := server.StreamMount(t.Context(), "huge", io.Discard); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := server.StreamMount(t.Context(), "missing-chunk", io.Discard); err == nil {
		t.Fatal("missing chunk accepted")
	}
	wantWrite := errors.New("write failed")
	if _, err := server.StreamMount(t.Context(), "dir", failingArchiveWriter{err: wantWrite}); !errors.Is(err, wantWrite) {
		t.Fatalf("header write failure = %v", err)
	}
	if _, err := server.StreamMount(t.Context(), "missing", failingArchiveWriter{err: wantWrite}); !errors.Is(err, wantWrite) {
		t.Fatalf("close write failure = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	server.mu.Lock()
	server.manifests["cancelled"] = &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Path: "entry", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR}}}
	server.mu.Unlock()
	if _, err := server.StreamMount(cancelled, "cancelled", io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v", err)
	}
}
