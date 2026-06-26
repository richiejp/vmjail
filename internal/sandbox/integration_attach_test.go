//go:build linux && integration

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/sandboxattach"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

// TestAttachExecE2E is the D2 attach end-to-end proof: boot a
// real Cloud Hypervisor VM with a long-running container as PID
// 1, dial the in-VM daft-agent's attach listener over hybrid-
// vsock, execute `/hello` inside the running container via the
// attach session, and assert the container's banner round-trips
// back over the PTY tunnel. Every layer is real — KVM, CH,
// virtiofsd, crun, PTY, vsock; no stubs.
func TestAttachExecE2E(t *testing.T) {
	cfg := resolveArtifacts(t)
	srcBundle := resolveBundle(t)

	bundleDir := prepareKeepaliveBundle(t, srcBundle)

	cfg.Shares = []*ShareConfig{{
		HostDir:    filepath.Dir(bundleDir),
		Tag:        "bundles",
		Mountpoint: "/bundles",
	}}
	cfg.Bundle = filepath.Join("/bundles", filepath.Base(bundleDir))
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(t.TempDir(), "v.sock"),
		GuestCID:   3,
		Attach:     true,
	}

	console := &syncBuf{}
	ctx, cancel := context.WithTimeout(traceCtx(t), 60*time.Second)
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(ctx, console, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
				t.Logf("Serve returned: %v\n--- console ---\n%s", err, console.String())
			}
		case <-time.After(10 * time.Second):
			t.Logf("Serve did not return within 10s of cancel\n--- console ---\n%s", console.String())
		}
	})

	// The container is up once its banner has been mirrored to the
	// console: PID 1 prints ContainerBanner and then enters its
	// keepalive block, leaving the container running so a `crun
	// exec` can land in it.
	waitForLine(t, console, agent.ContainerBanner, 45*time.Second)

	// Open the attach channel: dial guest vsock at AttachPort,
	// send the Hello with Command=["/hello"]. The agent will
	// `crun exec daft-container /hello`.
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	conn, err := DialGuestVsock(dialCtx, cfg.Vsock.HostSocket, agent.DefaultAttachVsockPort)
	if err != nil {
		t.Fatalf("DialGuestVsock: %v\n--- console ---\n%s", err, console.String())
	}
	session, err := sandboxattach.OpenSession(ctx, conn, sandboxgateway.AttachRequest{
		SessionID:  "d2d-test",
		SandboxUID: "d2d-sandbox",
		Command:    []string{"/hello"},
		TTY:        true,
		Cols:       80,
		Rows:       24,
	})
	if err != nil {
		t.Fatalf("OpenSession: %v\n--- console ---\n%s", err, console.String())
	}
	defer func() { _ = session.Close() }()

	// Cap the round-trip read so a hung PTY surfaces as a test
	// failure with the partial buffer rather than a deadline-
	// exceeded ctx swallowing the diagnostic.
	if derr := session.SetReadDeadline(time.Now().Add(20 * time.Second)); derr != nil {
		t.Fatalf("SetReadDeadline: %v", derr)
	}
	out, err := io.ReadAll(session)
	if err != nil {
		t.Fatalf("ReadAll(session): %v\noutput so far: %q\n--- console ---\n%s",
			err, out, console.String())
	}
	if !strings.Contains(string(out), agent.ContainerBanner) {
		t.Fatalf("attach session output missing banner %q\ngot: %q\n--- console ---\n%s",
			agent.ContainerBanner, out, console.String())
	}
}

// prepareKeepaliveBundle materialises a bundle under t.TempDir()
// whose config.json runs `/hello --keepalive` so the container
// stays alive after boot. The rootfs binary is copied from the
// upstream hello-bundle so the same `/hello` is also reachable
// via `crun exec`.
func prepareKeepaliveBundle(t *testing.T, srcBundle string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "keepalive-bundle")
	if err := os.MkdirAll(filepath.Join(dest, "rootfs"), 0o755); err != nil { //nolint:gosec // mode is the OCI rootfs convention.
		t.Fatalf("MkdirAll rootfs: %v", err)
	}
	srcBin := filepath.Join(srcBundle, "rootfs", "hello")
	dstBin := filepath.Join(dest, "rootfs", "hello")
	if err := copyExec(srcBin, dstBin); err != nil {
		t.Fatalf("copy hello binary: %v", err)
	}

	cfgBytes, err := os.ReadFile(filepath.Join(srcBundle, "config.json")) //nolint:gosec // srcBundle is the integration artefact path.
	if err != nil {
		t.Fatalf("read source config.json: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(cfgBytes, &spec); err != nil {
		t.Fatalf("parse source config.json: %v", err)
	}
	proc, ok := spec["process"].(map[string]any)
	if !ok {
		t.Fatalf("source config.json has no process object: %v", spec)
	}
	proc["args"] = []string{"/hello", "--keepalive"}
	patched, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal patched config.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "config.json"), patched, 0o644); err != nil { //nolint:gosec // bundle config has no secrets.
		t.Fatalf("write patched config.json: %v", err)
	}
	return dest
}

// copyExec copies an executable file from src to dst preserving
// 0o755 perms (the rootfs binary must be exec-bit for crun to
// run it).
func copyExec(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the bundle's rootfs binary.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // exec bit required for crun.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// syncBuf is a goroutine-safe bytes.Buffer. Serve writes to it in
// a background goroutine; the test reads from it via String() to
// scan for banners. Without the mutex, Go's race detector flags
// concurrent read/write.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLine polls b.String() until it contains target, or
// fails the test once timeout elapses. Used to gate the attach
// dial on the container actually being up.
func waitForLine(t *testing.T, b *syncBuf, target string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline via the if-return below"
	for {
		if strings.Contains(b.String(), target) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q on console within %s\n--- console ---\n%s",
				target, timeout, b.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
