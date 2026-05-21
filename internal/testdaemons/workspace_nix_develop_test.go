//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// d6c1IndexMarker is a known string baked into the index.html the
// test writes alongside the flake fixture. The forwarded HTTP GET
// asserts the response contains it — proving the byte path from
// the local listener all the way down to the python http.server
// running inside `nix run` inside the container is connected.
const d6c1IndexMarker = "d6c1-hermetic-marker"

// TestClusterWorkspaceForwardAgainstNixRun loads the nix-tooling
// OCI image, mounts a flake fixture whose `serve` app points at a
// pre-baked python3, and drives:
//
//	daft workspace exec ws -- /bin/nix run /workspace#serve -- \
//	    -m http.server <port>
//
// Proves the nix CLI works hermetically inside the container (no
// substituters, no network, no extra fetch) and that a /workspace
// flake drives which program runs. The forwarded HTTP GET asserts
// the response body surfaces back through the full byte path:
// local listener → ForwardPort RPC → orchestrator → sandboxd →
// vsock → agent → container → nix-evaluated python3 → http.server.
func TestClusterWorkspaceForwardAgainstNixRun(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_TOOLING_IMAGE",
		"nix build .#nix-tooling-image -o bin/nix-tooling-image")
	fixtureDir := requireEnvPath(t, "DAFT_NIX_FLAKE_FIXTURE",
		"nix build .#d6c1-flake-fixture -o bin/d6c1-flake-fixture")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6c1-tooling:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	copyFlakeFixture(t, fixtureDir, mountSrc)
	writeD6c1Index(t, mountSrc)

	logDir := t.TempDir()
	serverLog := filepath.Join(logDir, "server.log")
	forwardLog := filepath.Join(logDir, "forward.log")
	wsName := "d6c1-nixrun"
	const containerPort = uint32(8000)
	localPort := freeLocalPort(t)

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C1", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s",
			prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, mountSrc, wsName, orch,
		)},
		{Label: "port", Cmd: fmt.Sprintf(
			"%s workspace port add %d --name %s %s",
			prefix, containerPort, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s",
			prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s",
			prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-nix-tooling ready", 60*time.Second)

	// The exec'd command:
	//   cd /workspace && nix run /workspace#serve -- -m http.server N
	//
	// `nix run` evaluates /workspace/flake.nix, locates
	// apps.x86_64-linux.serve.program (a literal python3 store
	// path baked at flake-build time), and execs it with the
	// supplied args. cd /workspace first so python's
	// http.server serves the workspace dir (where index.html
	// lives) rather than the container's root.
	//
	// --offline rejects any substituter fetch; the flake has no
	// inputs and the program is already in /nix/store, so a
	// healthy run must not touch the network. If it does try,
	// we want a fast clear failure, not a slow timeout.
	startDaftSubprocess(
		t, serverLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c",
		// HOME/USER/PATH are set inline because crun exec doesn't
		// propagate the OCI runtime spec's process.env to ad-hoc
		// exec sessions — only the entrypoint inherits it. Without
		// HOME, nix CLI fatals with "cannot determine user's home
		// directory". (Wiring image-config Env through `daft
		// workspace exec` is a separate concern; that's tracked in
		// the PLAN as a follow-up.)
		fmt.Sprintf(
			"cd /workspace && exec env HOME=/tmp USER=root "+
				"NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt "+
				"/bin/nix --offline run "+
				"/workspace#serve -- -m http.server %d",
			containerPort,
		),
	)
	waitForFileContains(t, serverLog,
		"Serving HTTP on", 90*time.Second,
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

	url := fmt.Sprintf("http://127.0.0.1:%d/index.html", localPort)
	body := httpGetWithRetry(t, url, 10*time.Second)
	if !strings.Contains(body, d6c1IndexMarker) {
		t.Fatalf("response body = %q, want substring %q",
			body, d6c1IndexMarker)
	}
}

// copyFlakeFixture copies the read-only flake.nix from the
// fixture's nix-store directory into dst with 0644 perms. The
// store source is r/o; the dst is mounted into the container at
// /workspace, where nix run wants to write flake.lock — so the
// copy needs to be writable.
func copyFlakeFixture(t *testing.T, fixtureDir, dst string) {
	t.Helper()
	src := filepath.Join(fixtureDir, "flake.nix")
	b, err := os.ReadFile(src) //nolint:gosec // src is DAFT_NIX_FLAKE_FIXTURE; test-only.
	if err != nil {
		t.Fatalf("read flake fixture %s: %v", src, err)
	}
	out := filepath.Join(dst, "flake.nix")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
}

// writeD6c1Index writes a tiny index.html the forwarded HTTP GET
// can fetch and assert on. python -m http.server serves the
// process's CWD; the exec'd shell cd's into /workspace first, so
// /workspace/index.html lands at GET /index.html.
func writeD6c1Index(t *testing.T, dst string) {
	t.Helper()
	out := filepath.Join(dst, "index.html")
	body := "<html><body>" + d6c1IndexMarker + "</body></html>\n"
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
}
