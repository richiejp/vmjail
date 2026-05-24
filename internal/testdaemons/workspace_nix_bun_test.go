//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// d6c3BunMarker is a known string the bun-served body carries.
// The forwarded HTTP GET asserts the response contains it,
// proving the byte path local-listener → ForwardPort RPC →
// orchestrator → sandboxd → vsock → agent → container → `bun
// run server.ts`. Same role as d6c1IndexMarker.
const d6c3BunMarker = "d6c3-bun-marker"

// d6c3BunSlowBoot is the deadline for `nix develop -c bun run …`
// to print its readiness line. The first run inside a fresh VM
// has to (a) evaluate the flake (resolving nixpkgs from
// github), (b) substitute the bun closure from cache.nixos.org
// through sandboxnet's egress NAT, (c) launch bun. A warm path
// is ~2 min; the generous cap covers a cold large closure.
// Kept strictly under `make integration`'s 20-minute go-test
// timeout so waitForFileContains gets the chance to fatal with
// the file + pane dump (and the attached profiler's samples)
// before the framework's panic kills the test.
const d6c3BunSlowBoot = 15 * time.Minute

// d6c3FlakeNix is the minimal flake the test mounts at
// /workspace. Pins nixpkgs to the nixos-25.05 channel so the
// dev-shell evaluation is roughly stable across runs; bun
// itself is pulled by store-path from that nixpkgs revision.
// nix locks the input on the first `nix develop`, which
// writes /workspace/flake.lock — the mount must be writable.
const d6c3FlakeNix = `{
  description = "D6c.3 bun dev-shell";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs?ref=nixos-25.05";

  outputs = { self, nixpkgs }: let
    pkgs = nixpkgs.legacyPackages.x86_64-linux;
  in {
    devShells.x86_64-linux.default = pkgs.mkShell {
      packages = [ pkgs.bun ];
    };
  };
}
`

// d6c3ServerTS runs inside the bun dev-shell. Bun.serve binds
// 0.0.0.0 explicitly because Bun defaults to localhost-only
// and the port-forward path goes through agent → container's
// init netns; a localhost listener inside the container would
// still be reachable on 127.0.0.1, but spelling 0.0.0.0 keeps
// the intent clear and survives any future binding-policy
// change in Bun. The readiness banner mirrors daft-hello-
// server's "listening on" sentinel so waitForFileContains
// can use the same shape of substring match.
const d6c3ServerTS = `const port = parseInt(process.env.PORT ?? "8080");
const body = "hello from bun (` + d6c3BunMarker + `)\n";
const server = Bun.serve({
  hostname: "0.0.0.0",
  port: port,
  fetch(_req) {
    return new Response(body);
  },
});
console.log("bun listening on http://0.0.0.0:" + server.port);
`

// TestClusterWorkspaceBunDevServer is D6c.3 slice 2: the
// reference dev loop a real user would experience. Mounts a
// flake fixture at /workspace, exec's `nix develop -c bun run
// server.ts` inside the container, then port-forwards 8080 and
// asserts the bun-served body comes back through the full
// byte path.
//
// This exercises the D6c.2 sandboxnet egress under real load:
// `nix develop` resolves nixpkgs and substitutes the bun
// closure from cache.nixos.org via the in-VM tap0 →
// sandboxnet's gvisor-tap-vsock netstack → host network.
// On a cold container, that's the network-heaviest workload
// the integration suite runs.
//
// The workspace requests 4 GiB / 4 vCPUs: the 256 MiB default
// OOM-kills nix mid nixpkgs evaluation. An in-VM profiler is
// attached around the readiness wait so a future regression
// (hang vs crash vs slow) is diagnosable straight from the test
// log instead of needing a bespoke probe.
func TestClusterWorkspaceBunDevServer(t *testing.T) {
	imgPath := testdaemons.FetchNixOfficialImage(t)
	base, err := tarball.ImageFromPath(imgPath, nil)
	if err != nil {
		t.Fatalf("load %s: %v", imgPath, err)
	}
	img := withLongRunningEntrypoint(t, base, "D6C3S2-READY")
	ref := pushImageToRegistry(t, img, "test/d6c3-nix-bun:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	writeBunFixture(t, mountSrc)

	logDir := t.TempDir()
	serverLog := filepath.Join(logDir, "server.log")
	forwardLog := filepath.Join(logDir, "forward.log")
	wsName := "d6c3-bun"
	const containerPort = uint32(8080)
	localPort := freeLocalPort(t)

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C3S2", []testdaemons.WorkspaceStep{
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
		// nix develop evaluating nixpkgs OOM-kills at the 256 MiB
		// default; give the guest enough RAM (and more vCPUs than
		// the 1-core default) to finish evaluation + substitution.
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 4G --cpus 4 --name %s %s",
			prefix, wsName, orch,
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

	c.WaitForSandboxdOutput(t, "D6C3S2-READY", 60*time.Second)

	// HOME/USER/PATH set inline for the same reason D6c.1's test
	// does: `daft workspace exec` doesn't inherit the container
	// entrypoint's process.env (crun-exec gap). NIX_SSL_CERT_FILE
	// points at the cacert bundle the nixos/nix image ships.
	// PATH is rebuilt by `sh -lc`'s sourcing of /etc/profile.d/
	// nix.sh; HOME is what nix needs to read/write its profile
	// links and lock files.
	// `nix-command` and `flakes` are gated experimental features
	// on the upstream nixos/nix image; opt in via the CLI flag
	// instead of writing /etc/nix/nix.conf so the test stays
	// stateless across container restarts.
	//
	// `--option build-users-group ""` disables nix's nixbld
	// uid-sandbox at evaluation time. nix-daemon would otherwise
	// `chown /nix/store` to gid 30000 (nixbld) at first contact,
	// and that chown hits EPERM because virtiofsd — although it
	// has CAP_CHOWN within sandboxd's userns — does not have
	// CAP_CHOWN over the bundle files which live on the host fs
	// (init userns). Properly fixing this needs idmapped mounts
	// on the bundle share (or a sandboxd-private tmpfs / fuse
	// translator) — tracked under "Phase D7 — rootless storage
	// translation" in this file's slice plan. The workaround is
	// safe for this test: we're running a dev-shell, not building
	// derivations, so we don't need build sandboxing.
	execShell := "cd /workspace && exec env " +
		"HOME=/tmp USER=root " +
		"NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt " +
		"/bin/sh -lc 'nix " +
		"--extra-experimental-features \"nix-command flakes\" " +
		"--option build-users-group \"\" " +
		"-vv " +
		"develop --command bun run /workspace/server.ts'"
	serverProc := startDaftSubprocess(
		t, serverLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", execShell,
	)
	attachWorkspaceProfiler(t, c, wsName)
	waitForFileContains(t, serverProc, serverLog,
		"bun listening on", d6c3BunSlowBoot,
		func() string { return s.Capture() })

	forwardProc := startDaftSubprocess(
		t, forwardLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "forward",
		fmt.Sprintf("%d:%d", localPort, containerPort), wsName,
		"--orchestrator", c.OrchestratorURL,
	)
	waitForFileContains(t, forwardProc, forwardLog,
		"daft workspace forward:", 30*time.Second,
		func() string { return s.Capture() })

	url := fmt.Sprintf("http://127.0.0.1:%d/", localPort)
	body := httpGetWithRetry(t, url, 10*time.Second)
	if !strings.Contains(body, d6c3BunMarker) {
		t.Fatalf("response body = %q, want substring %q",
			body, d6c3BunMarker)
	}
}

// writeBunFixture lays down flake.nix + server.ts under dst.
// Both files are written with 0o644 so the container's uid
// (mapped via userns) can read them; flake.lock — which nix
// writes on first develop — needs the directory writable too.
func writeBunFixture(t *testing.T, dst string) {
	t.Helper()
	files := map[string]string{
		"flake.nix": d6c3FlakeNix,
		"server.ts": d6c3ServerTS,
	}
	for name, content := range files {
		out := filepath.Join(dst, name)
		if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", out, err)
		}
	}
}
