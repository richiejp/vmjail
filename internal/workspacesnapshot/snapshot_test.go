package workspacesnapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testDirectory = "mounts/0"
	testSamePath  = "same"
)

type snapshotListener struct {
	conn net.Conn
	err  error
	done chan struct{}
}

func (l *snapshotListener) Accept() (net.Conn, error) {
	if l.conn != nil {
		conn := l.conn
		l.conn = nil
		return conn, nil
	}
	if l.err != nil {
		return nil, l.err
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *snapshotListener) Close() error   { close(l.done); return nil }
func (l *snapshotListener) Addr() net.Addr { return testAddr("snapshot") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

func TestPullRoundTrip(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "hello.txt"), []byte("from xfs"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		_ = serveOne(server, map[string]string{testDirectory: source})
	}()
	destination := filepath.Join(t.TempDir(), "snapshot")
	err := Pull(t.Context(), func(context.Context) (net.Conn, error) { return client, nil }, testDirectory, destination)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "hello.txt")) // #nosec G304 -- test-controlled temporary destination.
	if err != nil || string(got) != "from xfs" {
		t.Fatalf("file = %q, %v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(destination, "link")); err != nil || got != "hello.txt" {
		t.Fatalf("link = %q, %v", got, err)
	}
}

func TestServeStopsWithContextAndReturnsAcceptError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	listener := &snapshotListener{done: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, nil) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve cancellation = %v", err)
	}
	want := errors.New("accept failed")
	err := Serve(t.Context(), &snapshotListener{err: want, done: make(chan struct{})}, nil)
	if !errors.Is(err, want) {
		t.Fatalf("Serve accept error = %v", err)
	}
}

func TestServeAcceptsAndStreamsConnection(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	listener := &snapshotListener{conn: server, done: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, map[string]string{testDirectory: source}) }()
	destination := filepath.Join(t.TempDir(), "out")
	if err := Pull(t.Context(), func(context.Context) (net.Conn, error) { return client, nil }, testDirectory, destination); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v", err)
	}
}

func TestPullReportsDialError(t *testing.T) {
	want := errors.New("dial failed")
	err := Pull(t.Context(), func(context.Context) (net.Conn, error) { return nil, want }, testDirectory, t.TempDir())
	if !errors.Is(err, want) {
		t.Fatalf("Pull = %v", err)
	}
}

func TestServeOneRejectsUnknownDirectory(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveOne(server, nil) }()
	if _, err := io.WriteString(client, `{"directory":"mounts/0"}`); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "unknown workspace directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestServeOneReportsRequestRootAndWriterErrors(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- serveOne(server, nil) }()
		if _, err := io.WriteString(client, "not-json\n"); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		if err := <-done; err == nil {
			t.Fatal("invalid request accepted")
		}
	})
	t.Run("root", func(t *testing.T) {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- serveOne(server, map[string]string{testDirectory: filepath.Join(t.TempDir(), "missing")})
		}()
		if _, err := io.WriteString(client, `{"directory":"mounts/0"}`); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		if err := <-done; err == nil {
			t.Fatal("missing root accepted")
		}
	})
	t.Run("writer", func(t *testing.T) {
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, "file"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- serveOne(server, map[string]string{testDirectory: source}) }()
		if _, err := io.WriteString(client, `{"directory":"mounts/0"}`); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		if err := <-done; err == nil {
			t.Fatal("closed writer accepted")
		}
	})
}

func TestServeOneReportsUnreadableTree(t *testing.T) {
	source := t.TempDir()
	locked := filepath.Join(source, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // #nosec G302 -- restore traversal for TempDir cleanup.
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveOne(server, map[string]string{testDirectory: source}) }()
	if _, err := io.WriteString(client, `{"directory":"mounts/0"}`); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, client)
		close(readDone)
	}()
	if err := <-done; err == nil {
		t.Fatal("unreadable tree accepted")
	}
	_ = server.Close()
	_ = client.Close()
	<-readDone
}

func TestPullReportsRequestWriteError(t *testing.T) {
	client, server := net.Pipe()
	_ = server.Close()
	err := Pull(t.Context(), func(context.Context) (net.Conn, error) { return client, nil }, testDirectory, t.TempDir())
	if err == nil {
		t.Fatal("closed request connection accepted")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	err := extract(tar.NewReader(&archive), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe snapshot path") {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractRejectsUnsupportedAndTruncatedEntries(t *testing.T) {
	var unsupported bytes.Buffer
	tw := tar.NewWriter(&unsupported)
	if err := tw.WriteHeader(&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extract(tar.NewReader(&unsupported), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported error = %v", err)
	}

	var truncated bytes.Buffer
	tw = tar.NewWriter(&truncated)
	if err := tw.WriteHeader(&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o600, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	archive := truncated.Bytes()
	if err := extract(tar.NewReader(bytes.NewReader(archive[:len(archive)-2])), t.TempDir()); err == nil {
		t.Fatal("truncated archive accepted")
	}
}

func TestExtractReportsArchiveAndFilesystemConflicts(t *testing.T) {
	if err := extract(tar.NewReader(strings.NewReader("short")), t.TempDir()); err == nil {
		t.Fatal("malformed archive accepted")
	}

	archive := func(headers ...tar.Header) *tar.Reader {
		var payload bytes.Buffer
		tw := tar.NewWriter(&payload)
		for i := range headers {
			if err := tw.WriteHeader(&headers[i]); err != nil {
				t.Fatal(err)
			}
			if headers[i].Size > 0 {
				if _, err := tw.Write(bytes.Repeat([]byte("x"), int(headers[i].Size))); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return tar.NewReader(bytes.NewReader(payload.Bytes()))
	}
	tests := []struct {
		name    string
		headers []tar.Header
	}{
		{name: "parent is file", headers: []tar.Header{{Name: "parent", Typeflag: tar.TypeReg}, {Name: "parent/file", Typeflag: tar.TypeReg}}},
		{name: "duplicate file", headers: []tar.Header{{Name: testSamePath, Typeflag: tar.TypeReg}, {Name: testSamePath, Typeflag: tar.TypeReg}}},
		{name: "directory over file", headers: []tar.Header{{Name: testSamePath, Typeflag: tar.TypeReg}, {Name: testSamePath, Typeflag: tar.TypeDir}}},
		{name: "symlink over file", headers: []tar.Header{{Name: testSamePath, Typeflag: tar.TypeReg}, {Name: testSamePath, Typeflag: tar.TypeSymlink, Linkname: "target"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := extract(archive(test.headers...), t.TempDir()); err == nil {
				t.Fatal("conflicting archive accepted")
			}
		})
	}
}

func TestExtractReportsDestinationPreparationErrors(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extract(tar.NewReader(bytes.NewReader(nil)), filepath.Join(parentFile, "child")); err == nil {
		t.Fatal("destination beneath file accepted")
	}

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // #nosec G302 -- restore traversal so TempDir cleanup can remove it.
	if err := extract(tar.NewReader(bytes.NewReader(nil)), filepath.Join(locked, "child")); err == nil {
		t.Fatal("destination beneath unwritable directory accepted")
	}
}
