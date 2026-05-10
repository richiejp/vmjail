package sandboxdcli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWorkspaceServeViaCobraRejectsMissingPath drives the full
// cobra tree through `daft-sandboxd workspace serve` with no flags
// — exercises newWorkspaceServeCmd's RunE closure that direct
// runWorkspaceServe calls miss.
func TestWorkspaceServeViaCobraRejectsMissingPath(t *testing.T) {
	dir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	err := Execute([]string{workspaceCmdName, serveSubName, "--config-dir", dir},
		&outBuf, &errBuf)
	if err == nil {
		t.Fatal("expected error for missing --path")
	}
}

// TestWorkspaceServeRejectsMissingPath proves the up-front flag
// validation refuses an empty --path.
func TestWorkspaceServeRejectsMissingPath(t *testing.T) {
	dir := t.TempDir()
	err := runWorkspaceServe(context.Background(), nopWriter{},
		"", "ws", "127.0.0.1:0", dir)
	if err == nil {
		t.Fatal("expected error for missing --path")
	}
	if !strings.Contains(err.Error(), "--path is required") {
		t.Errorf("error = %v, want --path message", err)
	}
}

func TestWorkspaceServeRejectsMissingWorkspaceName(t *testing.T) {
	dir := t.TempDir()
	err := runWorkspaceServe(context.Background(), nopWriter{},
		t.TempDir(), "", "127.0.0.1:0", dir)
	if err == nil {
		t.Fatal("expected error for missing --workspace")
	}
}

// TestWorkspaceServeStartsAndShutdown drives the full happy-path
// minus the actual pull (covered by the cli e2e test). Verifies the
// server boots, prints the fingerprint, and shuts down on context
// cancel.
func TestWorkspaceServeStartsAndShutdown(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte("a"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &syncBuf{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := runWorkspaceServe(ctx, out, src, "ws", "127.0.0.1:0", dir)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("runWorkspaceServe: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "fingerprint") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "fingerprint") {
		t.Fatalf("server didn't print fingerprint: out=%q", out.String())
	}
	cancel()
	wg.Wait()
}

// syncBuf is a thread-safe bytes.Buffer for capturing concurrent
// writes from a runWorkspaceServe goroutine.
type syncBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
