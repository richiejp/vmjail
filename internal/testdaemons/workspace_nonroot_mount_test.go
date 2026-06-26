//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceNonRootMountTranslatesOwnership is the #100
// regression. A NON-root container (image USER 1000:1000) must be able
// to read and write a host-backed /workspace whose source dir belongs
// to the dev, with writes persisting host-side as the dev.
//
// Pre-fix symptom: daft-sandboxd re-execs into a userns mapping the dev
// (host uid) -> ns-0, and the workspace virtiofsd identity-passed that
// through, so the dev's files appeared root:root in the guest. A
// non-root container then landed in `other` over a 0700 source dir and
// every write was EPERM. The fix presents each user mount as the
// container uid via bidirectional virtiofsd --translate-uid/-gid keyed
// off the resolved image USER (sandbox.ShareConfig.Translate).
//
// The assertion is deliberately strong: the source dir is 0700
// owner-only, so the container reaches inside it ONLY if the share is
// presented as its own user, and the host-side file must come back
// owned by the test runner (writeback mapped container-1000 -> ns-0 ->
// host). Without the fix the container write EPERMs, the OK sentinel
// never prints, and this fails. See PLAN.md #100.
func TestClusterWorkspaceNonRootMountTranslatesOwnership(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	// The container process (the image USER, not an exec'd command)
	// writes the probe itself, so this exercises the fundamental
	// container-owns-its-mount path independent of `exec --user`. The
	// trailing `exec sleep infinity` keeps the container alive; the
	// OK/FAIL branch is printed (and reaches sandboxd stdout via the
	// agent) only after the write is attempted.
	const (
		okSentinel   = "D100-WS-OK"
		failSentinel = "D100-WS-FAIL"
		probeBody    = "d100-probe"
	)
	script := fmt.Sprintf(
		"if echo %s > /workspace/probe.txt; then echo %s; else echo %s; fi; "+
			"id -u; exec sleep infinity",
		probeBody, okSentinel, failSentinel,
	)

	tarPath := decompressDockerTarball(t, imgPath)
	base, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("load image %s: %v", tarPath, err)
	}
	img := withUserAndEntrypoint(t, base, "1000:1000",
		[]string{"/bin/dash", "-c", script})
	ref := pushImageToRegistry(t, img, "test/d100-nonroot:v1")

	// 0700 owner-only (t.TempDir default), owned by the test runner.
	mountSrc := t.TempDir()

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "d100-nonroot"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D100", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, mountSrc, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	// OK only if the non-root container created the file in the 0700
	// dev-owned mount. failSentinel surfaces in the captured sandboxd
	// log on a regression so the cause is visible, not just a timeout.
	c.WaitForSandboxdOutput(t, okSentinel, 60*time.Second)

	// Host-side: the file persisted with the dev's content, owned by
	// the test runner — proving guest->host writeback mapped the
	// container uid back to the uid that maps to ns-0.
	probe := filepath.Join(mountSrc, "probe.txt")
	data, err := os.ReadFile(probe) //nolint:gosec // probe is under t.TempDir; test-only.
	if err != nil {
		t.Fatalf("host read %s: %v", probe, err)
	}
	if got := strings.TrimSpace(string(data)); got != probeBody {
		t.Fatalf("host probe content = %q, want %q", got, probeBody)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatalf("host stat %s: %v", probe, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("host stat: unexpected Sys() type %T", info.Sys())
	}
	if int(st.Uid) != os.Getuid() {
		t.Fatalf("host probe owner uid = %d, want %d (the dev who ran the test)",
			st.Uid, os.Getuid())
	}
}

// withUserAndEntrypoint clones img's config and sets both the OCI User
// (e.g. "1000:1000", so the container — and the runtime spec daft
// derives from it — runs non-root) and the entrypoint. Image layers
// are untouched.
func withUserAndEntrypoint(t *testing.T, img v1.Image, user string, entrypoint []string) v1.Image {
	t.Helper()
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	cfg := *cf
	cfg.Config.User = user
	cfg.Config.Entrypoint = entrypoint
	cfg.Config.Cmd = nil
	out, err := mutate.ConfigFile(img, &cfg)
	if err != nil {
		t.Fatalf("mutate config: %v", err)
	}
	return out
}
