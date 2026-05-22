//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceAgainstNixOfficial is the D6c.3 slice-1
// smoke test: prove the upstream nixos/nix container boots
// inside our sandbox VM and the bare `nix` CLI works.
//
// Upstream's image ENTRYPOINT runs `nix` and exits, which would
// kill the container before `daft workspace exec` could attach.
// Mutate the image config to swap in a long-running entrypoint
// before pushing to the in-process registry; the actual nixos/nix
// layers are untouched, so this is still the upstream binary.
// Slice 2 will replace the bare `nix --version` check with a
// real `nix develop -c <bun-dev-server>` and a port-forward GET.
func TestClusterWorkspaceAgainstNixOfficial(t *testing.T) {
	imgPath := testdaemons.FetchNixOfficialImage(t)

	base, err := tarball.ImageFromPath(imgPath, nil)
	if err != nil {
		t.Fatalf("load %s: %v", imgPath, err)
	}
	img := withLongRunningEntrypoint(t, base, "D6C3-READY")
	ref := pushImageToRegistry(t, img, "test/d6c3-nix-official:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	wsName := "d6c3-official"
	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C3", []testdaemons.WorkspaceStep{
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
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s",
			prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s",
			prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "D6C3-READY", 60*time.Second)

	// Upstream's image installs nix into /root/.nix-profile/bin
	// and sets PATH via /etc/profile.d/nix.sh, which a login shell
	// sources. `daft workspace exec` doesn't inherit the running
	// entrypoint's process.env (crun-exec limitation; same as the
	// D6c.1 test) so a bare `/bin/sh -c 'nix …'` would miss PATH.
	// `sh -lc` runs as a login shell, picks up the profile, and
	// resolves `nix` from PATH. `nix --version` prints a line like
	// "nix (Nix) 2.x.y"; match on the parenthesised name so a
	// future minor-version bump in upstream doesn't break here.
	execCmd := fmt.Sprintf(
		"%s workspace exec %s --tty=false %s -- /bin/sh -lc 'nix --version'",
		prefix, wsName, orch,
	)
	s.Enter(execCmd)
	s.WaitFor("nix (Nix)", 60*time.Second)
}

// withLongRunningEntrypoint clones the image config and swaps in a
// /bin/sh wrapper that prints sentinel + execs sleep infinity, so
// the container stays alive for `daft workspace exec`. Image
// layers are untouched. The exec form (/bin/sh -c …) avoids
// requiring a shell wrapper in PATH lookups.
func withLongRunningEntrypoint(t *testing.T, img v1.Image, sentinel string) v1.Image {
	t.Helper()
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	cfg := *cf
	cfg.Config.Entrypoint = []string{
		"/bin/sh", "-c",
		fmt.Sprintf("echo %s; exec sleep infinity", sentinel),
	}
	cfg.Config.Cmd = nil
	out, err := mutate.ConfigFile(img, &cfg)
	if err != nil {
		t.Fatalf("mutate config: %v", err)
	}
	return out
}

// pushImageToRegistry stands up an in-process OCI registry,
// pushes img under refName, and returns the host-qualified ref
// the sandboxd can pull from. Parallel to
// pushDockerTarballToRegistry but takes a v1.Image directly so
// callers can mutate the upstream image (e.g. swap entrypoint)
// before pushing.
func pushImageToRegistry(t *testing.T, img v1.Image, refName string) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	ref := u.Host + "/" + refName
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push image %s: %v", ref, err)
	}
	return ref
}
