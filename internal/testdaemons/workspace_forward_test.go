//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceForwardAgainstNixServer is the D6b.4
// dev-server + port-forward proof. Drives the full v0 reference
// workflow end to end:
//
//  1. Push the nix-shell-image (which bakes
//     /bin/daft-hello-server) to an in-process OCI registry.
//  2. Stand up the subprocess control plane + a tmux pane.
//  3. Drive workspace create / image / mount / port add /
//     target / launch — same arithmetic-sentinel pattern as
//     D6b.2 and D6b.3, except we now also declare a container
//     port so the orchestrator's port-route table picks it up.
//  4. Wait for the container's readiness sentinel on sandboxd
//     stdout (proves the VM booted + crun is running).
//  5. Background a `daft workspace exec --tty=false` that
//     launches daft-hello-server on container port 8080.
//     The exec stays in the tmux pane as a backgrounded process;
//     its stdout is captured to a file so the test can wait on
//     the server's own readiness banner.
//  6. Background a `daft workspace forward LOCAL:8080` on the
//     same pane. Wait for the forwarder's listener-up banner.
//  7. HTTP GET against the local forwarder and assert the
//     known body — the byte path is:
//     local listener → ForwardPort RPC → orchestrator
//     → sandboxd → vsock → agent → container → server.
//
// Roughly the longest e2e cycle in the project. Targets a
// reference Dev Loop that mirrors what a real user would do.
func TestClusterWorkspaceForwardAgainstNixServer(t *testing.T) {
	imgPath := requireEnvImage(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6b4-nix:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	logDir := t.TempDir()
	serverLog := filepath.Join(logDir, "server.log")
	forwardLog := filepath.Join(logDir, "forward.log")
	wsName := "d6b4-fwd"
	const containerPort = uint32(8080)
	localPort := freeLocalPort(t)

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	type step struct {
		label string
		cmd   string
	}
	steps := []step{
		{"create", fmt.Sprintf(
			"%s workspace create --name %s %s",
			prefix, wsName, orch,
		)},
		{"image", fmt.Sprintf(
			"%s workspace image %s --name %s %s",
			prefix, ref, wsName, orch,
		)},
		{"mount", fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, mountSrc, wsName, orch,
		)},
		{"port", fmt.Sprintf(
			"%s workspace port add %d --name %s %s",
			prefix, containerPort, wsName, orch,
		)},
		{"target", fmt.Sprintf(
			"%s workspace target %s --name %s %s",
			prefix, c.SandboxdFP, wsName, orch,
		)},
		{"launch", fmt.Sprintf(
			"%s workspace launch --name %s %s",
			prefix, wsName, orch,
		)},
	}
	for i, st := range steps {
		base := (i + 1) * 1000
		full := fmt.Sprintf(`%s; printf 'D6B4-%%d\n' $((%d+$?))`, st.cmd, base)
		s.Enter(full)
		s.WaitFor(fmt.Sprintf("D6B4-%d", base), 30*time.Second)
	}

	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	// daft workspace exec and daft workspace forward are both
	// long-running blocking commands. Driving them through the
	// tmux pane with `&` was attempted but daft inherited the
	// shell's job-control stdin = /dev/null and silently hung
	// before producing any output. Run them as managed
	// subprocesses instead — far simpler, gives the test direct
	// stdout/stderr capture, and clean Cleanup via SIGTERM.
	startDaftSubprocess(
		t, serverLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/daft-hello-server",
		fmt.Sprintf("--addr=:%d", containerPort),
	)
	waitForFileContains(t, serverLog,
		"daft-hello-server listening on", 30*time.Second,
		func() string { return s.Capture() })

	startDaftSubprocess(
		t, forwardLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "forward",
		fmt.Sprintf("%d:%d", localPort, containerPort), wsName,
		"--orchestrator", c.OrchestratorURL,
	)
	waitForFileContains(t, forwardLog,
		"daft workspace forward:", 30*time.Second,
		func() string { return s.Capture() })

	// HTTP GET through the forwarder. Use a per-attempt timeout
	// + a brief retry loop: the forwarder's stdout banner fires
	// the moment its listener opens, but Connect-RPC may need
	// another roundtrip before the first accept is fully wired.
	url := fmt.Sprintf("http://127.0.0.1:%d/", localPort)
	body := httpGetWithRetry(t, url, 10*time.Second)
	if !strings.Contains(body, "hello from daft-hello-server") {
		t.Fatalf("response body = %q, want hello-from-daft-hello-server",
			body)
	}
}

// startDaftSubprocess spawns a long-running `daft ...` command
// under its own process group with combined stdout+stderr
// redirected to logPath. Registers a Cleanup that SIGTERMs the
// process group so the test doesn't leak daft + its in-VM
// children when it ends. Mirrors startDaemon's pattern from
// cluster.go but specialised for client-side daft invocations.
func startDaftSubprocess(t *testing.T, logPath string, daftBin string, args ...string) *exec.Cmd {
	t.Helper()
	out, err := os.Create(logPath) //nolint:gosec // logPath is under t.TempDir(); test-only.
	if err != nil {
		t.Fatalf("create log %s: %v", logPath, err)
	}
	//nolint:gosec // G204: args are caller-controlled test-only paths.
	cmd := exec.Command(daftBin, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	// /dev/null stdin so daft's stdin pump EOFs immediately —
	// same as a non-interactive shell-backgrounded invocation,
	// but explicit so the behaviour is predictable.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	cmd.Stdin = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daft %v: %v", args, err)
	}
	t.Cleanup(func() {
		_ = devNull.Close()
		_ = out.Close()
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
	})
	return cmd
}

// freeLocalPort picks a free loopback port for `daft workspace
// forward`. Brief race with other binders; acceptable for tests.
// Returns the port as uint32 to match the parsed-port type
// daft workspace forward emits, but the value lives well within
// the uint16 TCP range so the conversion is loss-free.
func freeLocalPort(t *testing.T) uint32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return uint32(addr.Port) //nolint:gosec // TCP ports fit in uint16; conversion is loss-free.
}

// waitForFileContains polls path until substr appears or
// timeout fires. The poll cadence is tight (100 ms) so the
// readiness handoff between subprocess and Go test is snappy.
// On timeout the helper fatals with the file contents AND the
// tmux pane snapshot — the latter pins down whether the typed
// command even reached daft (vs. a shell-quoting bug that left
// the redirection broken so the file stays empty).
func waitForFileContains(t *testing.T, path, substr string, timeout time.Duration, paneDump func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline + t.Fatalf below"
	for {
		b, _ := os.ReadFile(path) //nolint:gosec // path is under t.TempDir(); test-only.
		if strings.Contains(string(b), substr) {
			return
		}
		if time.Now().After(deadline) {
			fi, statErr := os.Stat(path)
			fiInfo := "missing"
			if statErr == nil {
				fiInfo = fmt.Sprintf("size=%d mtime=%s", fi.Size(), fi.ModTime())
			} else {
				fiInfo = fmt.Sprintf("stat: %v", statErr)
			}
			t.Fatalf("file %s never contained %q within %s (%s)\n"+
				"--- file contents ---\n%s\n"+
				"--- pane ---\n%s",
				path, substr, timeout, fiInfo, b, paneDump())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// httpGetWithRetry does GET against url until the request lands
// a 200 OK or timeout fires. Tolerates the brief gap between
// the forwarder printing its banner and its first Accept being
// fully wired.
func httpGetWithRetry(t *testing.T, url string, timeout time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	//daft:lint:disable=forloopbound reason="bounded by deadline + t.Fatalf below"
	for {
		resp, err := client.Get(url) //nolint:gosec,noctx // url is a loopback test URL; no untrusted input.
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				return string(body)
			}
			lastErr = fmt.Errorf("status=%d readErr=%v", resp.StatusCode, readErr)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("http GET %s failed after %s: %v", url, timeout, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
