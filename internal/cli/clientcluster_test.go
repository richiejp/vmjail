package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/localai-org/darkaifactory/internal/localcluster"
)

// Shared fixtures for the cluster/discovery tests (hoisted for goconst).
const (
	testClusterURL       = "https://127.0.0.1:8443"
	testClusterClientDir = "/state/client"
	testStateDir         = "/state"
	testSandboxFP        = "sha256:abc"
	testOrchFP           = "sha256:def"
)

// TestDiscoverLocalClusterReal exercises the un-stubbed discovery closure
// end to end: a handle written at the default (XDG) state dir with a live
// pid is found; an absent one is not.
func TestDiscoverLocalClusterReal(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "daft", "cluster")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	js := fmt.Sprintf(
		`{"url":%q,"client_config_dir":%q,"pids":[%d]}`,
		testClusterURL, filepath.Join(dir, "client"), os.Getpid(),
	)
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	c, ok := discoverLocalCluster()
	if !ok || c == nil || c.URL != testClusterURL {
		t.Fatalf("discoverLocalCluster: ok=%v c=%+v", ok, c)
	}
}

func TestDiscoverLocalClusterAbsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if c, ok := discoverLocalCluster(); ok {
		t.Fatalf("want no cluster discovered, got %+v", c)
	}
}

func TestDiscoverLocalClusterMalformed(t *testing.T) {
	// A malformed handle (Load returns an error) must read as "no cluster",
	// not propagate — discovery is best-effort.
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "daft", "cluster")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, ok := discoverLocalCluster(); ok {
		t.Fatalf("malformed handle should not discover a cluster, got %+v", c)
	}
}

func TestDiscoverLocalClusterStaleNotAlive(t *testing.T) {
	// A handle whose recorded pids are all dead reads as "no cluster".
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "daft", "cluster")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	js := fmt.Sprintf(`{"url":%q,"pids":[%d]}`, testClusterURL, 1<<30)
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, ok := discoverLocalCluster(); ok {
		t.Fatalf("stale (dead-pid) handle should not discover a cluster, got %+v", c)
	}
}

// stubDiscovery swaps the discoverLocalCluster seam for the duration of a
// test, restoring it on cleanup.
func stubDiscovery(t *testing.T, c *localcluster.Cluster, ok bool) {
	t.Helper()
	prev := discoverLocalCluster
	discoverLocalCluster = func() (*localcluster.Cluster, bool) { return c, ok }
	t.Cleanup(func() { discoverLocalCluster = prev })
}

// isolateConfig points XDG dirs at temp locations and clears
// $DAFT_ORCHESTRATOR + cwd dir-local config so resolution is driven only
// by what the test sets up — never the developer's real config.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(orchestratorEnvVar, "")
	t.Chdir(t.TempDir()) // no ./.daft/config.json here
}

func TestResolveOrchestratorSourceLocalCluster(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, &localcluster.Cluster{
		URL:             testClusterURL,
		ClientConfigDir: testClusterClientDir,
	}, true)

	url, src, err := resolveOrchestratorSource("", "")
	if err != nil {
		t.Fatalf("resolveOrchestratorSource: %v", err)
	}
	if url != testClusterURL || src != sourceLocalCluster {
		t.Fatalf("got (%q, %q), want (cluster URL, %q)", url, src, sourceLocalCluster)
	}
}

func TestResolveOrchestratorSourceFlagBeatsCluster(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, &localcluster.Cluster{URL: testClusterURL}, true)

	url, src, err := resolveOrchestratorSource("https://remote:9443", "")
	if err != nil {
		t.Fatalf("resolveOrchestratorSource: %v", err)
	}
	if url != "https://remote:9443" || src != sourceFlag {
		t.Fatalf("explicit flag must win: got (%q, %q)", url, src)
	}
}

func TestResolveOrchestratorSourceDefaultWhenNoCluster(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, nil, false)

	url, src, err := resolveOrchestratorSource("", "")
	if err != nil {
		t.Fatalf("resolveOrchestratorSource: %v", err)
	}
	if url != defaultOrchestratorURL || src != sourceDefault {
		t.Fatalf("no cluster → built-in default: got (%q, %q)", url, src)
	}
}

func TestResolveIdentityDirUsesClusterClientDir(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, &localcluster.Cluster{
		URL:             testClusterURL,
		ClientConfigDir: testClusterClientDir,
	}, true)

	// No --config-dir, orchestrator resolves to the cluster → cluster identity.
	dir, err := resolveIdentityDir("", "")
	if err != nil {
		t.Fatalf("resolveIdentityDir: %v", err)
	}
	if dir != testClusterClientDir {
		t.Fatalf("want cluster client dir, got %q", dir)
	}
}

func TestResolveIdentityDirRemoteKeepsXDG(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, &localcluster.Cluster{
		URL:             testClusterURL,
		ClientConfigDir: testClusterClientDir,
	}, true)

	// Talking to a different orchestrator (URL ≠ cluster URL): the cluster
	// identity must NOT be picked up — fall back to the user's XDG dir.
	dir, err := resolveIdentityDir("", "https://remote:9443")
	if err != nil {
		t.Fatalf("resolveIdentityDir: %v", err)
	}
	xdg, err := resolveClientDir("")
	if err != nil {
		t.Fatalf("resolveClientDir: %v", err)
	}
	if dir != xdg {
		t.Fatalf("remote orchestrator must use XDG dir %q, got %q", xdg, dir)
	}
}

func TestResolveIdentityDirNoClusterXDG(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, nil, false) // no cluster up

	dir, err := resolveIdentityDir("", "")
	if err != nil {
		t.Fatalf("resolveIdentityDir: %v", err)
	}
	xdg, err := resolveClientDir("")
	if err != nil {
		t.Fatalf("resolveClientDir: %v", err)
	}
	if dir != xdg {
		t.Fatalf("no cluster → XDG dir %q, got %q", xdg, dir)
	}
}

func TestResolveIdentityDirOverrideWins(t *testing.T) {
	isolateConfig(t)
	stubDiscovery(t, &localcluster.Cluster{
		URL:             testClusterURL,
		ClientConfigDir: testClusterClientDir,
	}, true)

	dir, err := resolveIdentityDir("/explicit/dir", "")
	if err != nil {
		t.Fatalf("resolveIdentityDir: %v", err)
	}
	if dir != "/explicit/dir" {
		t.Fatalf("explicit --config-dir must win, got %q", dir)
	}
}
