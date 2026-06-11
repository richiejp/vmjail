//go:build linux && integration

package testdaemons_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/localai-org/darkaifactory/internal/agent"
	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceLaunchAgainstHelloBundle is the D6b.2 proof:
// stand up the subprocess control plane (D6b.1), publish the hello
// container image to an in-process OCI registry the sandboxd can
// pull from, then drive `daft workspace create / image / mount /
// target / launch` through a tmux pane and assert the orchestrator
// reports the workspace as Dispatched.
//
// This is the first slice that actually boots a VM via the black-
// box CLI. D6b.3 will swap the hello image for a nix-based one.
func TestClusterWorkspaceLaunchAgainstHelloBundle(t *testing.T) {
	bundlePath := requireBundle(t)
	ref := pushHelloImage(t, bundlePath)

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	wsName := "d6b2-hello"
	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6B", []testdaemons.WorkspaceStep{
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

	// The launch command's response carries the updated workspace
	// summary; `Status: Dispatched` is the proof that the
	// orchestrator marked the workspace as in flight before it
	// replied. Capture once after the final WaitFor so we read
	// the pane snapshot from after launch completed.
	got := s.Capture()
	if !strings.Contains(got, "Dispatched") {
		t.Fatalf("post-launch capture missing Dispatched:\n%s", got)
	}

	// The deeper proof: the sandboxd actually booted a VM, the
	// agent ran crun against the hello image bundle, and the
	// container printed its banner. The banner travels back via
	// the sandboxd's stdout (which testdaemons captures into the
	// cluster's outputCapture).
	c.WaitForSandboxdOutput(t, agent.ContainerBanner, 60*time.Second)

	// With the VM booted, the Tier-1 observer must have sampled the
	// cloud-hypervisor PID at least once into
	// <ObserverDir>/<uid>/observer.jsonl — proof that NewCluster's
	// DAFT_STATE_DIR wiring lights up host-side observability for every
	// testdaemons VM (it was dark before roadmap item #4). The dump-on-
	// failure Cleanup surfaces the same file if this assertion fails.
	waitForObserverSample(t, c.ObserverDir, 15*time.Second)

	// And the single positive proof that per-VM cgroup limits work end to
	// end: the observer samples the same per-VM cgroup applyVMCgroupLimits
	// writes to, so a finite memory.max in the trace means the controllers
	// were enabled (sandboxd self-wrapped into a private scope) and the hard
	// cap was written. The fail-closed startup gate makes every other VM-
	// booting e2e an implicit check that limits *could* be enforced; this is
	// the one test that asserts they actually *were*.
	requireCgroupLimitApplied(t, c.ObserverDir, 15*time.Second)
}

// requireCgroupLimitApplied polls the observer traces until some VM's
// observer.jsonl carries a cgroup sample with a finite (positive) memory.max —
// proof the per-VM cgroup's memory controller was enabled and the hard cap
// was written. memory.max holds the literal "max" (encoded as -1) when no
// limit is set and is absent (0) when the controller isn't enabled, so a
// positive value is conclusive that limitsForConfig was applied.
func requireCgroupLimitApplied(t *testing.T, observerDir string, timeout time.Duration) {
	t.Helper()
	type sampleLine struct {
		Cgroup *struct {
			Memory struct {
				MaxBytes int64 `json:"max_bytes"`
			} `json:"memory"`
		} `json:"cgroup"`
	}
	deadline := time.Now().Add(timeout)
	lastMax := int64(0)
	sawCgroup := false
	for {
		var found bool
		_ = filepath.WalkDir(observerDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "observer.jsonl" {
				return nil //nolint:nilerr // skip unreadable/irrelevant entries; keep walking.
			}
			b, rerr := os.ReadFile(path) //nolint:gosec // path is under the test's own TempDir.
			if rerr != nil {
				return nil //nolint:nilerr // a half-written file is retried on the next tick.
			}
			for _, line := range bytes.Split(b, []byte("\n")) {
				var s sampleLine
				if len(line) == 0 || json.Unmarshal(line, &s) != nil || s.Cgroup == nil {
					continue
				}
				sawCgroup = true
				lastMax = s.Cgroup.Memory.MaxBytes
				if s.Cgroup.Memory.MaxBytes > 0 {
					found = true
				}
			}
			return nil
		})
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no per-VM cgroup hard memory limit in any observer.jsonl under %s after %s "+
				"(sawCgroupSample=%v lastMemoryMax=%d; -1=unlimited, 0=controller off → limits not enforced)",
				observerDir, timeout, sawCgroup, lastMax)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForObserverSample polls observerDir until some VM's
// observer.jsonl holds at least one `"pid"` sample line, or fatals
// citing the dir. The observer samples at ~1 Hz, so a freshly-booted
// VM yields its first line within a second or two.
func waitForObserverSample(t *testing.T, observerDir string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var sampled bool
		_ = filepath.WalkDir(observerDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "observer.jsonl" {
				return nil //nolint:nilerr // skip unreadable/irrelevant entries; keep walking.
			}
			b, rerr := os.ReadFile(path) //nolint:gosec // path is under the test's own TempDir.
			if rerr == nil && bytes.Contains(b, []byte("\"pid\"")) {
				sampled = true
			}
			return nil
		})
		if sampled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no observer sample under %s after %s — Tier-1 wiring dark?",
				observerDir, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// requireBundle fatals with a clear pointer when DAFT_BUNDLE is
// missing — it's a separate concern from the runtime-artefact
// env vars testdaemons.NewCluster already checks because only
// image-pushing tests need the prebuilt hello bundle.
func requireBundle(t *testing.T) string {
	t.Helper()
	p := os.Getenv("DAFT_BUNDLE")
	if p == "" {
		t.Fatalf("testdaemons: DAFT_BUNDLE not set " +
			"(run via `make integration` or set it manually to bin/hello-bundle)")
	}
	return p
}

// pushHelloImage stands up an in-process OCI registry on a free
// loopback port, packages the daft-hello-container binary from
// the bundle into a single-layer image with /hello as the
// entrypoint, and returns the registry ref the sandboxd can pull.
//
// Mirrors internal/sandbox's pushHelloIntegrationImage so the
// two test packages don't have to import each other. The helper
// is small enough that duplication is cheaper than carving out a
// new shared package; if D6b.3 also needs it for the nix image
// path the consolidation moves there.
func pushHelloImage(t *testing.T, bundlePath string) string {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}

	helloBin := readHelloBinary(t, bundlePath)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "hello", Mode: 0o755, Size: int64(len(helloBin))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(helloBin); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatalf("layer: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	img, err = mutate.Config(img, v1.Config{Entrypoint: []string{"/hello"}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	ref := u.Host + "/test/d6b2-hello:v1"
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	return ref
}

// readHelloBinary slurps the prebuilt daft-hello-container binary
// from the bundle and asserts it embeds the banner constant so a
// stale bundle fails fast.
func readHelloBinary(t *testing.T, bundlePath string) []byte {
	t.Helper()
	helloPath := filepath.Join(bundlePath, "rootfs", "hello")
	b, err := os.ReadFile(helloPath) //nolint:gosec // bundlePath is DAFT_BUNDLE; test-only.
	if err != nil {
		t.Fatalf("read hello binary at %s: %v", helloPath, err)
	}
	if len(b) == 0 {
		t.Fatalf("hello binary at %s is empty", helloPath)
	}
	if !bytes.Contains(b, []byte(agent.ContainerBanner)) {
		t.Fatalf("hello binary missing ContainerBanner %q — stale bundle?",
			agent.ContainerBanner)
	}
	return b
}
