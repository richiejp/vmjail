package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunListensAndServesBody starts the server on an ephemeral
// port (--addr=:0), reads the bound-port banner off the captured
// stdout, then HTTP-GETs the listener and confirms the body
// matches the --body flag.
func TestRunListensAndServesBody(t *testing.T) {
	body := "test-body-payload\n"
	out := newBufferedStdoutWaiter()
	exitCh := make(chan int, 1)
	go func() {
		exitCh <- run([]string{"--addr=127.0.0.1:0", "--body=" + body}, out)
	}()

	addr := waitForListenAddr(t, out, 2*time.Second)
	url := fmt.Sprintf("http://%s/", addr)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url) //nolint:gosec,noctx // loopback test URL.
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
}

// TestRunRejectsBadFlags exercises the flag-parse error branch:
// an unknown flag returns code 2 and the function exits before
// trying to listen.
func TestRunRejectsBadFlags(t *testing.T) {
	var buf bytes.Buffer
	code := run([]string{"--no-such-flag"}, &buf)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

// TestRunReturnsOneOnListenError forces net.Listen to fail by
// targeting an obviously-invalid address. Covers the "listen
// error → log + return 1" branch.
func TestRunReturnsOneOnListenError(t *testing.T) {
	var buf bytes.Buffer
	code := run([]string{"--addr=bogus-host-name:1:2:3"}, &buf)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "vmjail-hello-server: listen") {
		t.Errorf("missing listen-error log: %q", buf.String())
	}
}

// TestRunReturnsOneOnServeError pins the serve-error branch: we
// substitute a serve func that errors immediately, simulating an
// http.Server.Shutdown / unexpected accept failure. Covers the
// only path the happy test can't reach without ungracefully
// closing the listener.
func TestRunReturnsOneOnServeError(t *testing.T) {
	oldServe := serve
	defer func() { serve = oldServe }()
	want := errors.New("synthetic serve fail")
	serve = func(net.Listener, http.Handler) error { return want }

	var buf bytes.Buffer
	code := run([]string{"--addr=127.0.0.1:0"}, &buf)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "synthetic serve fail") {
		t.Errorf("missing serve-error log: %q", buf.String())
	}
}

// TestMainCallsRunWithOsArgs pins the trivial main() wrapper.
func TestMainCallsRunWithOsArgs(t *testing.T) {
	oldExit, oldOut, oldArgs, oldServe := osExit, stdout, os.Args, serve
	defer func() { osExit, stdout, os.Args, serve = oldExit, oldOut, oldArgs, oldServe }()
	os.Args = []string{"vmjail-hello-server", "--no-such-flag"}
	var buf bytes.Buffer
	stdout = &buf
	code := -1
	osExit = func(c int) { code = c }
	main()
	if code != 2 {
		t.Errorf("code = %d, want 2 (bad-flag exit)", code)
	}
}

// bufferedStdoutWaiter accumulates everything the server writes
// to stdout so the test can poll for the bound-port banner
// without racing the goroutine.
type bufferedStdoutWaiter struct {
	mu  byteMutex
	buf bytes.Buffer
}

func newBufferedStdoutWaiter() *bufferedStdoutWaiter {
	return &bufferedStdoutWaiter{}
}

func (b *bufferedStdoutWaiter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *bufferedStdoutWaiter) Snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// byteMutex is sync.Mutex spelt out so the test file avoids a
// `sync` import (Go test rigs at top level often prefer one
// helper over a stdlib import for clarity).
type byteMutex struct{ ch chan struct{} }

func (m *byteMutex) Lock() {
	if m.ch == nil {
		m.ch = make(chan struct{}, 1)
	}
	m.ch <- struct{}{}
}

func (m *byteMutex) Unlock() { <-m.ch }

func waitForListenAddr(t *testing.T, out *bufferedStdoutWaiter, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//vmjail:lint:disable=forloopbound reason="bounded by deadline; exits on match or t.Fatalf"
	for {
		snap := out.Snapshot()
		if idx := strings.Index(snap, readyPrefix); idx >= 0 {
			rest := snap[idx+len(readyPrefix):]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				return strings.TrimSpace(rest[:nl])
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never printed %q within %s; stdout=%q",
				readyPrefix, timeout, snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
