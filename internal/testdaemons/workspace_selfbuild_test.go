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

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// daftSelfBuildSentinel is printed by the wrapper shell when
// `make check` exits 0 inside the sandbox. Distinct from the
// readiness sentinel (daft-check ready) so a successful check is
// not confused with the container merely having booted.
const daftSelfBuildSentinel = "DAFT-SELFBUILD-OK"

// daftSelfBuildInnerTimeout caps `make check` from inside the
// sandbox. The image is nix-less and the deps are vendored, so
// there is no substitution and no go-module fetch — the cost is
// purely the cold-GOCACHE compile of every package + the test
// run. GNU `timeout` kills the inner pipeline with SIGKILL on
// overrun; `daft workspace exec` then exits and
// waitForFileContains fails-fast on `proc.exited()` rather than
// waiting out the outer cap. Provisional 10 min: a cold-GOCACHE
// golangci-lint + go test on the 4-vCPU envelope below is the cost
// (the build is offline, so there's no substitution to wait on).
// This tightens once the capability work lands a green run and the
// warm-GOCACHE follow-up (PLAN.md D6c.4 slice 1) caps the compile.
const daftSelfBuildInnerTimeout = 600 // seconds, formatted into the wrapper shell

// daftSelfBuildSlowBoot is the outer cap for waitForFileContains.
// Strict deadline, not a generous ceiling — per CONTRIBUTING.md
// "Slowness is a bug". Just past the inner GNU-timeout so any
// overrun surfaces as a fast subprocess-exit failure (~5 s after
// the inner kill), not an outer-timeout fatal that swallows the
// cause. A cold run that legitimately exceeds this is the signal
// to land the baked warm-GOCACHE, not to extend the deadline.
const daftSelfBuildSlowBoot = 660 * time.Second

// TestClusterWorkspaceDaftSelfBuild is D6c.4 slice 1: prove the
// dogfooding loop — boot a workspace, drop the daft source in,
// run `make check` inside, and assert the standard pre-commit
// gate (fmt + lint + tests + coverage) passes.
//
// It runs against the NIX-LESS daft-check-image (flake
// `daft-check-image`, env DAFT_CHECK_IMAGE): the dev-shell check
// toolchain baked as image content with no nix binary/daemon/db,
// plus the Go dependency set as a vendor tree at /opt/daft/vendor.
// The wrapper symlinks that to /workspace/vendor and runs
// `make check` with GOFLAGS=-mod=vendor GOPROXY=off, so the build
// is hermetic and network-free — no cache.nixos.org substitution
// (the cap-buster the io-snoop gadget caught against the old
// `nix develop` image) and no go-module fetch.
//
// Source delivery is the same pattern as the bun dev-shell test
// (D6c.3 slice 2): tar the tracked tree of HEAD into a tempdir
// and virtiofs-mount it at /workspace. `git archive HEAD` skips
// build artefacts and the heavy .git blob stores; exportDaftSourceTo
// layers the minimal .git metadata the check-submodule-revs gate
// needs.
//
// Gated behind DAFT_RUN_SELFBUILD=1: a cold-GOCACHE `make check`
// compile overruns the default per-test budget even offline.
// Same opt-in shape as TestWorkspaceThroughputExperiment; the
// baked warm-GOCACHE follow-up is what lets this join the default
// suite.
func TestClusterWorkspaceDaftSelfBuild(t *testing.T) {
	if os.Getenv("DAFT_RUN_SELFBUILD") != "1" {
		// Gated AND currently red-when-enabled: the offline build
		// (fmt + lint + compile) passes in-sandbox, but `make check`
		// runs daft's own unit suite and 19 tests across 5 packages
		// (daftfuse, userns, agent, observability, sandboxdcli) need
		// host/VM capabilities the hardened container withholds
		// (/dev/fuse, newuidmap/newgidmap, agent init devices,
		// /proc io). Granting those via per-workspace config sections
		// is the next slice — see PLAN.md D6c.4 slice 1.
		t.Skip("set DAFT_RUN_SELFBUILD=1 to run the daft-in-daft self-build test (offline make check inside the sandbox; blocked on container capabilities — see PLAN.md D6c.4)")
	}

	srcDir := t.TempDir()
	exportDaftSourceTo(t, srcDir)
	// Drop the vendored Go deps into the source mount so the offline
	// `make check` resolves every package with -mod=vendor. Delivered
	// here (host cp → virtiofs) rather than baked into the image
	// because dockerTools layering corrupts go:embed files into
	// "irregular" symlinks; see exportVendorTo / the flake comment.
	exportVendorTo(t, srcDir)

	// The nix-less check image already ships a long-running
	// entrypoint + "daft-check ready" sentinel, so it loads and
	// pushes directly — no withLongRunningEntrypoint rewrap.
	imgPath := requireEnvPath(t, "DAFT_CHECK_IMAGE",
		"nix build .#daft-check-image -o bin/daft-check-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6c4-daft-selfbuild:v1")

	// No egress proxy: the build is fully offline, so a cluster
	// without network egress is the stronger assertion — if
	// anything reaches for cache.nixos.org or proxy.golang.org the
	// build fails rather than silently falling back to the network.
	c := testdaemons.NewCluster(t)
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
		// `go test ./...` compiling + running every package in
		// parallel is well past the 256 MiB default. 8 GiB / 4 CPUs
		// mirrors the envelope a developer actually uses on the host.
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

	c.WaitForSandboxdOutput(t, "daft-check ready", 60*time.Second)

	// Wrapper rationale:
	//   * env vars are inline because crun-exec doesn't inherit the
	//     container entrypoint's process.env.
	//   * CGO_ENABLED=0 because the image ships no C compiler (the
	//     daft binaries are all static CGO=0 anyway). Without it the
	//     default CGO=1 makes golangci-lint's typecheck try to
	//     cgo-preprocess the `net` package and fail with "could not
	//     import C".
	//   * GOTOOLCHAIN=local pins the baked Go (go.mod asks for an
	//     older minor than the image ships, so without this Go would
	//     try to fetch the exact toolchain over the network).
	//   * GOFLAGS=-mod=vendor + GOPROXY=off force the offline build
	//     from /workspace/vendor (symlinked to the image's baked
	//     /opt/daft/vendor). GOCACHE + GOTMPDIR both go to
	//     host-backed /workspace: the compile temp (golangci-lint
	//     export data, link objects) and the cumulative build cache
	//     each exceed the 256 MiB /tmp tmpfs. GOTMPDIR doubles as
	//     the test binary's TMPDIR (go test sets it), so the short
	//     /workspace/.gotmp prefix keeps unix-socket test paths under
	//     the 108-byte sockaddr_un limit.
	//   * `make check` exits 0 on success; the trailing
	//     `&& echo DAFT-SELFBUILD-OK` lets waitForFileContains
	//     detect success without relying on the absence of error
	//     output (which `go test` produces in volume even green).
	// `timeout --signal=KILL Ns` bounds the inner pipeline; on
	// overrun SIGKILL fires, the wrapper shell exits, `daft
	// workspace exec` propagates the non-zero exit, and
	// waitForFileContains fails-fast on `proc.exited()` instead of
	// waiting out the outer cap.
	execShell := fmt.Sprintf(
		"exec env HOME=/workspace/.home USER=root PATH=/bin "+
			"CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=vendor GOPROXY=off "+
			"GOCACHE=/workspace/.gocache GOPATH=/workspace/.gopath GOTMPDIR=/workspace/.gotmp "+
			"/bin/sh -c 'mkdir -p /workspace/.home /workspace/.gotmp && cd /workspace && "+
			"timeout --signal=KILL %ds make check && echo %s'",
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

// exportVendorTo copies the vendored Go module tree (daft.goModules,
// env DAFT_CHECK_VENDOR) into dst/vendor as regular, writable files.
// The offline `make check` then builds with -mod=vendor and never
// touches the network. cp -rL (dereference, copy content) — not a
// symlink — because the tree is virtiofs-served into the guest where
// a symlink to a host store path would dangle, and because go:embed
// rejects symlinked embed files (the reason this isn't baked into the
// image; see the flake comment).
func exportVendorTo(t *testing.T, dst string) {
	t.Helper()
	vendorSrc := requireEnvPath(t, "DAFT_CHECK_VENDOR",
		"nix build .#daft.goModules -o bin/daft-check-vendor")
	vendorDst := filepath.Join(dst, "vendor")
	if err := os.MkdirAll(vendorDst, 0o755); err != nil {
		t.Fatalf("mkdir vendor: %v", err)
	}
	cp := exec.Command("cp", "-rL", "--no-preserve=mode", vendorSrc+"/.", vendorDst)
	var stderr strings.Builder
	cp.Stderr = &stderr
	if err := cp.Run(); err != nil {
		t.Fatalf("cp vendor: %v (stderr: %s)", err, stderr.String())
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
