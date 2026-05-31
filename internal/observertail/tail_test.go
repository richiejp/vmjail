package observertail

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuf is a mutex-guarded io.Writer a test can poll from one
// goroutine while Stream writes to it from another, without racing.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: test appends to its own t.TempDir() file.
	if err != nil {
		t.Fatalf("open append %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

func TestStreamDumpThenEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer.jsonl")
	writeFile(t, path, "line1\nline2\n")
	var buf bytes.Buffer
	// follow=false returns at EOF without waiting.
	if err := Stream(context.Background(), path, false, &buf); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("dump = %q, want the whole file", buf.String())
	}
}

func TestStreamOpenError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	if err := Stream(context.Background(), missing, false, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error opening a missing file")
	}
}

func TestStreamFollowAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer.jsonl")
	writeFile(t, path, "first\n")
	var buf safeBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stream(ctx, path, true, &buf, time.Millisecond)
	}()

	waitFor(t, func() bool { return strings.Contains(buf.String(), "first") }, "initial dump")
	appendFile(t, path, "second\n")
	waitFor(t, func() bool { return strings.Contains(buf.String(), "second") }, "appended line")

	// The follow loop must still be running until we cancel.
	select {
	case <-done:
		t.Fatal("Stream returned before ctx cancel in follow mode")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after ctx cancel")
	}
}

func TestStreamFollowAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observer.jsonl")
	writeFile(t, path, "pre-rotate\n")
	var buf safeBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stream(ctx, path, true, &buf, time.Millisecond)
	}()
	waitFor(t, func() bool { return strings.Contains(buf.String(), "pre-rotate") }, "pre-rotate content")

	// Mimic observability.Observer.rotateLocked: rename current → .1,
	// then recreate the path as a fresh (smaller) file.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate rename: %v", err)
	}
	writeFile(t, path, "post-rotate\n")
	waitFor(t, func() bool { return strings.Contains(buf.String(), "post-rotate") }, "post-rotate content")

	cancel()
	<-done
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write fail") }

// TestStreamReadError covers the non-EOF read-error branch: opening a
// directory succeeds and stats fine, but reading it errors.
func TestStreamReadError(t *testing.T) {
	dir := t.TempDir()
	if err := Stream(context.Background(), dir, false, &bytes.Buffer{}); err == nil {
		t.Fatal("expected a read error streaming a directory")
	}
}

func TestStreamCtxAlreadyCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer.jsonl")
	writeFile(t, path, "data\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// follow=true, but the cancelled ctx short-circuits the loop top.
	if err := Stream(ctx, path, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("Stream on a cancelled ctx = %v, want nil", err)
	}
}

func TestReopenIfRotatedSeekError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer.jsonl")
	writeFile(t, path, "data\n")
	f, err := os.Open(path) //nolint:gosec // G304: test opens its own t.TempDir() file.
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	_ = f.Close() // Seek on a closed file errors.
	if _, _, _, rerr := reopenIfRotated(path, f, info); rerr == nil {
		t.Error("expected a Seek error from a closed file")
	}
}

func TestStreamWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer.jsonl")
	writeFile(t, path, "data\n")
	if err := Stream(context.Background(), path, false, errWriter{}); err == nil {
		t.Fatal("expected the write error to surface")
	}
}

// TestStreamFollowToleratesRotationGap exercises the reopen path when
// the log is briefly absent mid-rotation (renamed away, not yet
// recreated): the follow loop must tolerate the stat miss and pick the
// new file up once it appears.
func TestStreamFollowToleratesRotationGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observer.jsonl")
	writeFile(t, path, "before\n")
	var buf safeBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stream(ctx, path, true, &buf, time.Millisecond)
	}()
	waitFor(t, func() bool { return strings.Contains(buf.String(), "before") }, "before content")

	// Rename away with NO immediate recreate → the path is absent for
	// a few poll ticks (reopenIfRotated's os.Stat errors → continue).
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	writeFile(t, path, "after\n")
	waitFor(t, func() bool { return strings.Contains(buf.String(), "after") }, "after content")
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
