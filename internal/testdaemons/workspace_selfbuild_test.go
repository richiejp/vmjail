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

// daftSelfBuildInnerTimeout caps `nix develop … make check` from
// inside the sandbox. If nix hangs (the symptom of the
// not-yet-diagnosed narinfo-on-404 bug — proxy passthrough is
// verified by TestPassthrough404PreservesStatusAndBody but the
// in-VM nix client mis-handles the response anyway), GNU
// `timeout` kills the inner pipeline with SIGKILL; the daft
// `workspace exec` subprocess then exits and
// waitForFileContains fails-fast on `proc.exited()` rather than
// waiting out the outer cap. Budget: ~30 s nix-develop env
// activation + ~3 min make check (host cold baseline 2 m 45 s)
// = ~3.5 min worst case. 4 min leaves a small buffer.
const daftSelfBuildInnerTimeout = 240 // seconds, formatted into the wrapper shell

// daftSelfBuildSlowBoot is the outer cap for waitForFileContains.
// Strict deadline, not a generous ceiling — per CONTRIBUTING.md
// "Slowness is a bug". Just past the inner GNU-timeout so any
// hang in nix surfaces as a fast subprocess-exit failure (~5 s
// after the inner kill), not an outer-timeout fatal that swallows
// the cause. If the test legitimately exceeds 4.5 min the cache
// is cold; rerun once `/nix/store` is partially populated (D6c.3
// slice 3, host-mounted virtiofs, is the proper fix). We do not
// extend the deadline to accommodate the first uncached run.
const daftSelfBuildSlowBoot = 270 * time.Second

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
		t.Skip("set DAFT_RUN_SELFBUILD=1 to run the daft-in-daft self-build test (inner 4 min cap on nix, outer 4.5 min on the sentinel wait)")
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
	// `timeout --signal=KILL Ns` bounds the inner pipeline. If nix
	// hangs (currently observed against cache.nixos.org 404s; see
	// TestPassthrough404PreservesStatusAndBody and PLAN.md D6c.4),
	// SIGKILL fires, the wrapper shell exits, `daft workspace
	// exec` propagates the non-zero exit, and waitForFileContains
	// fails-fast on `proc.exited()` instead of waiting out the
	// outer cap. SIGKILL not SIGTERM because nix's substitution
	// loop has historically ignored SIGTERM until the active fetch
	// completes — same reason `make integration`'s VMs are killed
	// not terminated.
	// `nix develop path:.` forces flake evaluation in plain-path
	// mode rather than git mode. With git mode, nix's libgit2
	// would try to walk `.git/objects` to identify tracked files;
	// our `.git/` copy ships HEAD/index/refs but not the object
	// store (~700 MB of blob CAS that `make check` does not
	// touch), so libgit2 errors with code = 6 (GIT_ENOTFOUND).
	// Plain-path mode treats the workdir as the source verbatim
	// and reads every file present — exactly what we want, since
	// `git archive HEAD` already gave us the hermetic HEAD
	// content. Trade-off: any uncommitted scratch in this dir
	// would now be visible to the flake, but the export is a
	// fresh tempdir so there isn't any.
	execShell := fmt.Sprintf(
		"cd /workspace && exec env "+
			"HOME=/tmp USER=root "+
			"NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt "+
			"/bin/sh -lc 'timeout --signal=KILL %ds nix "+
			"--extra-experimental-features \"nix-command flakes\" "+
			"--option build-users-group \"\" "+
			"develop path:. --command sh -c \"make check && echo %s\"'",
		daftSelfBuildInnerTimeout, daftSelfBuildSentinel,
	)

	// Two debugging aids attach before the build exec so they cover
	// the whole run from the first sample. The in-container /proc
	// profiler answers "is the VM OOM/CPU-bound/blocked"; the Tier-2
	// eBPF gadget stream (sandbox observe) answers "what is the build
	// actually doing" — process tree, fetch bytes, poll loops, ENOENT.
	// Both are invisible on a pass and right there in the log on a
	// stall. Per [[feedback-observability]] the gadget view is the
	// preferred surface; the /proc sampler stays as a complementary
	// resource view while it's still useful.
	attachWorkspaceProfiler(t, c, wsName)
	attachWorkspaceObserve(t, c, wsName)
	buildProc := startDaftSubprocess(
		t, buildLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", execShell,
	)
	waitForFileContains(t, buildProc, buildLog,
		daftSelfBuildSentinel, daftSelfBuildSlowBoot,
		func() string { return s.Capture() })
}

// exportDaftSourceTo writes the tracked-at-HEAD daft tree under
// dst, plus the minimal `.git/` metadata `make check` needs.
// `git archive HEAD` gives a hermetic file payload — no bin/, no
// result*, no untracked editor scratch. We layer on `.git/` next
// so `git rev-parse --show-toplevel`, `git submodule status`, and
// the `check-submodule-revs.sh` gate that wraps them all keep
// working inside the sandbox. The submodule worktrees themselves
// stay empty (gitlinks aren't extracted by `git archive`); the
// drift gate reads SHAs from `.git/index` + `.git/modules/<name>/
// HEAD`, never the worktrees, and treats "not initialized"
// submodules as "use the index-pinned SHA".
//
// Objects + logs + hooks + lfs are excluded so the metadata copy
// is ~11 MB rather than ~838 MB (almost all of the latter is
// `.git/modules/third_party/{linux,clang,…}/objects`, none of
// which is referenced by `make check`).
func exportDaftSourceTo(t *testing.T, dst string) {
	t.Helper()
	root := daftRepoRoot(t)

	// 1. tracked HEAD content via git archive | tar -x.
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

	// 2. Minimal `.git/` metadata so `make check-submodule-revs`
	// works inside the sandbox. We tar from the host repo's
	// `.git/` directly into dst/.git/, excluding the heavy blob
	// stores. tar's --transform/-s flags vary across versions,
	// so we drive a single `tar -C $root/.git -cf - <selected> |
	// tar -C $dst/.git -xf -` pipeline, with the selection
	// implemented by --exclude.
	gitDst := filepath.Join(dst, ".git")
	if err := os.MkdirAll(gitDst, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	gitSrc := filepath.Join(root, ".git")
	// Exclude paths chosen so libgit2 (nix flake evaluator) still
	// recognises the result as a valid repo. `objects/info` stays
	// in (libgit2 walks it during repo discovery); only the blob
	// stores under `objects/[0-9a-f][0-9a-f]/` and `objects/pack/`
	// are dropped. Without that nuance nix bails with "libgit2
	// error code = 6" (GIT_ENOTFOUND).
	cp := exec.Command("tar",
		"-C", gitSrc,
		"--exclude=objects/[0-9a-f][0-9a-f]",
		"--exclude=objects/pack",
		"--exclude=lfs",
		"--exclude=logs",
		"--exclude=hooks",
		"-cf", "-",
		".")
	xcp := exec.Command("tar", "-C", gitDst, "-xf", "-")
	pipe2, err := cp.StdoutPipe()
	if err != nil {
		t.Fatalf("git-meta pipe: %v", err)
	}
	xcp.Stdin = pipe2

	var cpErr, xcpErr strings.Builder
	cp.Stderr = &cpErr
	xcp.Stderr = &xcpErr

	if err := xcp.Start(); err != nil {
		t.Fatalf("start git-meta extract: %v", err)
	}
	if err := cp.Run(); err != nil {
		_ = xcp.Process.Kill()
		_, _ = xcp.Process.Wait()
		t.Fatalf("git-meta tar: %v (stderr: %s)", err, cpErr.String())
	}
	if err := xcp.Wait(); err != nil {
		t.Fatalf("git-meta extract: %v (stderr: %s)", err, xcpErr.String())
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
