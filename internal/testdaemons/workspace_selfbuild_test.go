//go:build linux && integration

package testdaemons_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// daftSelfBuildSentinel is printed by the wrapper shell when
// `make check` exits 0 inside the sandbox. Distinct from the
// readiness sentinel (D6C4-READY) so a successful check is not
// confused with the container merely having booted.
const daftSelfBuildSentinel = "DAFT-SELFBUILD-OK"

// daftSelfBuildSlowBoot is a strict deadline, not a generous
// ceiling — per CONTRIBUTING.md "Slowness is a bug". Measured
// budget on a healthy machine + healthy egress:
//
//   - ~2–3 min for nix to substitute the dev-shell closure (~1.5
//     GB: Go toolchain + golangci-lint + gofumpt + gocyclo +
//     goconst + scc + buf + tmux + dash …) from cache.nixos.org
//     through sandboxnet's egress NAT;
//   - ~3 min for `make check` itself with cold Go build / module
//     / lint caches (host baseline: 32 s warm, 2 m 45 s fully
//     cold).
//
// Total healthy cold ≈ 5–6 min; with a warm `/nix/store` (D6c.3
// slice 3, host-mounted via virtiofs) it drops to ~1 min. 6 min
// is the tight cap: anything longer is the known 50 KB/s
// sandboxnet-egress throughput floor (PLAN.md D6c.3 slice 2
// "Throughput floor under investigation") surfacing as a
// failure, which is the point. Caching policy: if a true cold
// substitution legitimately exceeds 6 min on your machine,
// rerun — the closure now sits on /nix/store and the next run
// is warm. We do not extend the deadline to accommodate the
// first uncached run.
const daftSelfBuildSlowBoot = 6 * time.Minute

// TestClusterWorkspaceDaftSelfBuild is D6c.4 slice 1: prove the
// dogfooding loop — boot a workspace, drop the daft source in,
// run `nix develop -c make check` inside, and assert the standard
// pre-commit gate (fmt + lint + tests + coverage) passes.
//
// Source delivery is the same pattern as the bun dev-shell test
// (D6c.3 slice 2): tar the tracked tree of HEAD into a tempdir
// and virtiofs-mount it at /workspace. `git archive HEAD` skips
// build artefacts and .git automatically, so the mount is
// hermetic by construction. nix re-fetches the flake's
// submodule pins from their upstream URLs via the sandboxnet
// egress proxy; the .git directory is not required.
//
// Gated behind DAFT_RUN_SELFBUILD=1 — the first run substitutes
// the full dev-shell closure from cache.nixos.org, which is the
// network-heaviest workload the integration suite can run and
// overruns the default per-test budget. Same opt-in shape as
// TestWorkspaceThroughputExperiment.
func TestClusterWorkspaceDaftSelfBuild(t *testing.T) {
	if os.Getenv("DAFT_RUN_SELFBUILD") != "1" {
		t.Skip("set DAFT_RUN_SELFBUILD=1 to run the daft-in-daft self-build test (~5–6 min healthy cold, fails at 6 min)")
	}

	srcDir := t.TempDir()
	exportDaftSourceTo(t, srcDir)

	imgPath := testdaemons.FetchNixOfficialImage(t)
	base, err := tarball.ImageFromPath(imgPath, nil)
	if err != nil {
		t.Fatalf("load %s: %v", imgPath, err)
	}
	img := withLongRunningEntrypoint(t, base, "D6C4-READY")
	ref := pushImageToRegistry(t, img, "test/d6c4-daft-selfbuild:v1")

	c := testdaemons.NewCluster(t, testdaemons.WithEgressProxy())
	s := testtmux.New(t)

	logDir := t.TempDir()
	buildLog := filepath.Join(logDir, "selfbuild.log")
	wsName := "d6c4-selfbuild"

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C4", []testdaemons.WorkspaceStep{
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
			prefix, srcDir, wsName, orch,
		)},
		// `nix develop` evaluating the daft flake + substituting the
		// dev-shell closure + running `go test ./...` in parallel is
		// well past the 256 MiB default. 8 GiB / 4 CPUs mirrors the
		// envelope a developer actually uses on the host.
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 8G --cpus 4 --name %s %s",
			prefix, wsName, orch,
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

	c.WaitForSandboxdOutput(t, "D6C4-READY", 60*time.Second)

	// Wrapper rationale, mirroring the bun test:
	//   * HOME/USER/PATH inline because crun-exec doesn't inherit
	//     the container entrypoint's process.env.
	//   * NIX_SSL_CERT_FILE points at the cacert bundle the
	//     nixos/nix image ships, so `nix` can hit cache.nixos.org
	//     over HTTPS through the sandboxnet egress proxy.
	//   * sh -lc rebuilds PATH via /etc/profile.d/nix.sh so the
	//     nix binary resolves.
	//   * --extra-experimental-features opts into nix-command +
	//     flakes without writing /etc/nix/nix.conf.
	//   * --option build-users-group "" disables the nixbld
	//     uid-sandbox; virtiofsd lacks CAP_CHOWN over bundle files
	//     in init userns (see D7) so chowning /nix/store to nixbld
	//     EPERMs. Safe here: `make check` runs Go tooling, not
	//     derivation builds.
	//   * `make check` exits 0 on success; the trailing
	//     `&& echo DAFT-SELFBUILD-OK` lets waitForFileContains
	//     detect success without relying on the absence of error
	//     output (which `go test -v` produces in volume even on a
	//     green run).
	execShell := "cd /workspace && exec env " +
		"HOME=/tmp USER=root " +
		"NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt " +
		"/bin/sh -lc 'nix " +
		"--extra-experimental-features \"nix-command flakes\" " +
		"--option build-users-group \"\" " +
		"develop --command sh -c \"make check && echo " +
		daftSelfBuildSentinel + "\"'"

	buildProc := startDaftSubprocess(
		t, buildLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", execShell,
	)
	attachWorkspaceProfiler(t, c, wsName)
	waitForFileContains(t, buildProc, buildLog,
		daftSelfBuildSentinel, daftSelfBuildSlowBoot,
		func() string { return s.Capture() })
}

// exportDaftSourceTo writes the tracked-at-HEAD daft tree under
// dst. `git archive HEAD` (rather than `cp -r`) guarantees the
// payload is hermetic: no bin/, no result*, no untracked editor
// scratch, no .git. nix doesn't need .git either — the flake's
// submodule pins live in third_party/submodule-revs.json (a
// tracked file), and `nix develop` happily evaluates a flake
// from a plain directory.
//
// Runs `git archive HEAD | tar -x -C dst`. The pipe is wired in
// Go because exec.Cmd.StdoutPipe + os/exec orchestration is
// clearer (and lets us surface either side's error) than shelling
// out to /bin/sh.
func exportDaftSourceTo(t *testing.T, dst string) {
	t.Helper()
	root := daftRepoRoot(t)

	archive := exec.Command("git", "-C", root, "archive", "--format=tar", "HEAD")
	untar := exec.Command("tar", "-x", "-C", dst)

	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatalf("git archive stdout pipe: %v", err)
	}
	untar.Stdin = pipe

	var archiveStderr, untarStderr strings.Builder
	archive.Stderr = &archiveStderr
	untar.Stderr = &untarStderr

	if err := untar.Start(); err != nil {
		t.Fatalf("start tar: %v", err)
	}
	if err := archive.Run(); err != nil {
		_ = untar.Process.Kill()
		_, _ = untar.Process.Wait()
		t.Fatalf("git archive: %v (stderr: %s)", err, archiveStderr.String())
	}
	if err := untar.Wait(); err != nil {
		t.Fatalf("tar extract: %v (stderr: %s)", err, untarStderr.String())
	}
}

// daftRepoRoot returns the absolute path of the daft module root,
// resolved from `go env GOMOD`. Mirrors the unexported helper in
// cluster.go (kept duplicated rather than exported because this
// is the only test that needs it from outside the package and
// the body is six lines).
func daftRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	modPath := strings.TrimSpace(string(out))
	if modPath == "" || modPath == "/dev/null" {
		t.Fatal(errors.New("not inside a Go module"))
	}
	return filepath.Dir(modPath)
}
