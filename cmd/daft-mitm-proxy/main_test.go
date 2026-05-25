package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a mutex-guarded io.Writer + reader for log
// assertions. The proxy logs from its serve goroutine while the test
// reads from the test goroutine; a raw bytes.Buffer would be a data
// race, and reading before the serve goroutine has logged would be a
// timing race. waitForContains polls until the substring lands or the
// deadline passes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) waitForContains(t *testing.T, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline"
	for {
		if strings.Contains(s.String(), substr) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ephemeralAddrFlag asks run() to bind an OS-assigned port on
// loopback. Shared by the run()-level tests.
const ephemeralAddrFlag = "--addr=127.0.0.1:0"

// TestRunWritesCAToDisk pins the --ca-cert-out behaviour the
// integration wiring needs: invoking the binary with the flag writes a
// PEM-encoded CA cert to the path.
func TestRunWritesCAToDisk(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "proxy-ca.pem")
	// Start in a goroutine since http.Serve blocks; we only care that
	// the file appears before Serve gets to it, which is the order in
	// run().
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{ephemeralAddrFlag, "--ca-cert-out=" + caPath}, &out)
	}()

	// Poll: file should land within a few hundred ms of binary start.
	// If the binary errored out, done fires with non-zero; re-check
	// below.
	deadline := time.Now().Add(5 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by 5s deadline"
	for {
		if _, err := os.Stat(caPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ca-cert-out %s not written within 5s; binary out=%q", caPath, out.String())
		}
		select {
		case code := <-done:
			t.Fatalf("binary exited code=%d before CA was written; out=%q",
				code, out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}

	caPEM, err := os.ReadFile(caPath) //nolint:gosec // caPath is t.TempDir + constant.
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	if !bytes.HasPrefix(caPEM, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Errorf("CA file does not start with PEM CERTIFICATE block: %s", caPEM[:min(len(caPEM), 64)])
	}
}

// TestRunRejectsBadFlags pins the flag-parse error path so future arg
// additions don't accidentally turn errors into hangs.
func TestRunRejectsBadFlags(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--no-such-flag"}, &out)
	if code == 0 {
		t.Errorf("code = 0 on bad flag; want non-zero")
	}
}

// TestRunSurfacesListenError pins the listen-failure path.
func TestRunSurfacesListenError(t *testing.T) {
	// Bind a port, then ask run() to bind the same port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	addr := ln.Addr().String()
	var out bytes.Buffer
	code := run([]string{"--addr=" + addr}, &out)
	if code == 0 {
		t.Errorf("code = 0 on bound port; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "listen") {
		t.Errorf("missing listen err in out: %q", out.String())
	}
}

// TestMainCallsRunWithOsArgs pins the trivial main() wrapper. Without
// it main() stays uncovered and drags the per-file gate.
func TestMainCallsRunWithOsArgs(t *testing.T) {
	oldExit, oldOut, oldArgs := osExit, stdout, os.Args
	defer func() { osExit, stdout, os.Args = oldExit, oldOut, oldArgs }()
	os.Args = []string{"daft-mitm-proxy", "--no-such-flag"}
	var buf bytes.Buffer
	stdout = &buf
	code := -1
	osExit = func(c int) { code = c }
	main()
	if code != 2 {
		t.Errorf("code = %d, want 2 (bad-flag exit)", code)
	}
}

// TestRunRejectsBadCAOutPath surfaces the "CA cert write failed" error
// path: pointing --ca-cert-out at a path whose parent doesn't exist
// must fail-fast with the listen step skipped.
func TestRunRejectsBadCAOutPath(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "no-such-dir", "ca.pem")
	var out bytes.Buffer
	code := run([]string{ephemeralAddrFlag, "--ca-cert-out=" + caPath}, &out)
	if code == 0 {
		t.Errorf("code = 0; want non-zero")
	}
	if !strings.Contains(out.String(), "write") {
		t.Errorf("missing 'write' err in out: %q", out.String())
	}
}

// TestRunSurfacesCAError covers run()'s fail-fast when the CA can't be
// built — exercised by pointing --ca-dir at a directory holding a
// corrupt ca.pem (a present-but-malformed CA is a hard error, not a
// silent regenerate). run() must report "ca:" and return non-zero
// before touching the listener.
func TestRunSurfacesCAError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("seed corrupt cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("seed corrupt key: %v", err)
	}
	var out bytes.Buffer
	code := run([]string{ephemeralAddrFlag, "--ca-dir=" + dir}, &out)
	if code == 0 {
		t.Errorf("code = 0; want non-zero")
	}
	if !strings.Contains(out.String(), "ca:") {
		t.Errorf("missing 'ca:' err in out: %q", out.String())
	}
}

// TestRunCacheOpenError covers run()'s fail-fast when --cache-dir can't
// be opened (here, its parent is a regular file).
func TestRunCacheOpenError(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	var out bytes.Buffer
	code := run([]string{ephemeralAddrFlag, "--cache-dir=" + filepath.Join(parent, "c")}, &out)
	if code == 0 {
		t.Errorf("code = 0; want non-zero")
	}
	if !strings.Contains(out.String(), "cache:") {
		t.Errorf("missing 'cache:' err in out: %q", out.String())
	}
}

// TestRunStartsWithCacheDir covers run()'s cache-open success path:
// with a valid --cache-dir the proxy opens the cache and reaches the
// listening state. Runs in a goroutine since Serve blocks; the
// listener is reclaimed at process exit (same pattern as
// TestRunWritesCAToDisk).
func TestRunStartsWithCacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	out := &syncBuffer{}
	go func() {
		_ = run([]string{
			ephemeralAddrFlag,
			"--cache-dir=" + cacheDir,
			"--cache-max-bytes=4096",
		}, out)
	}()
	if !out.waitForContains(t, readyPrefix, 5*time.Second) {
		t.Fatalf("proxy did not reach listening state with cache; out=%q", out.String())
	}
}
