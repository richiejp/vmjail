//go:build linux && integration

package testdaemons_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceEgressProxyEnv is Phase J2's end-to-end proof
// that the *injected* egress proxy is actually used — no per-command
// --proxy / --insecure flags. The cluster auto-starts the caching
// proxy (WithEgressProxy), which sets DAFT_EGRESS_PROXY_URL/CA on
// sandboxd; the executor injects https_proxy + SSL_CERT_FILE and the
// proxy CA into the bundle, and the agent forwards that env into
// `crun exec`. So an in-VM `daft-http-fetch --url=…` (bare) routes
// through the proxy via https_proxy and trusts the MITM leaf via the
// CA the proxy minted (IP-SAN leaf for 127.0.0.1).
//
// A second identical fetch is served from the proxy cache, so the
// upstream is hit exactly once across both — proving the response
// went through the recording proxy, not directly.
func TestClusterWorkspaceEgressProxyEnv(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/j2-egress-nix:v1")

	body := []byte("j2-egress-env-body\n")
	bodySHA := sha256.Sum256(body)
	var upstreamHits atomic.Int64

	// The upstream must NOT be on loopback: Go's ProxyFromEnvironment
	// unconditionally bypasses the proxy for 127.0.0.1 / localhost
	// targets, so an env-driven fetch to a loopback URL would go
	// direct and never reach the proxy. Bind to a real host IP — the
	// host-side proxy can still dial it, and the guest proxies it.
	hostIP := hostNonLoopbackIPv4(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write(body)
	}))
	ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", hostIP, err)
	}
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)
	// The proxy mints an IP-SAN leaf for hostIP that the guest
	// validates against the injected CA.
	targetURL := srv.URL + "/"

	c := testdaemons.NewCluster(t, testdaemons.WithEgressProxy())
	s := testtmux.New(t)
	mountSrc := t.TempDir()
	wsName := "j2-egress"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "J2EGRESS", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf("%s workspace create --name %s %s", prefix, wsName, orch)},
		{Label: "image", Cmd: fmt.Sprintf("%s workspace image %s --name %s %s", prefix, ref, wsName, orch)},
		{Label: "mount", Cmd: fmt.Sprintf("%s workspace mount add /workspace --source %s --name %s %s", prefix, mountSrc, wsName, orch)},
		{Label: "target", Cmd: fmt.Sprintf("%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch)},
		{Label: "launch", Cmd: fmt.Sprintf("%s workspace launch --name %s %s", prefix, wsName, orch)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	wantSentinel := fmt.Sprintf("daft-http-fetch ok status=200 bytes=%d sha256=%s",
		len(body), hex.EncodeToString(bodySHA[:]))

	// First fetch records into the proxy cache; upstream hit once.
	fetchViaEnv(t, c, wsName, targetURL, wantSentinel, filepath.Join(t.TempDir(), "fetch1.log"))
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("upstream hit %d times on first fetch, want 1", n)
	}

	// Second identical fetch is a cache HIT — upstream must stay at 1.
	fetchViaEnv(t, c, wsName, targetURL, wantSentinel, filepath.Join(t.TempDir(), "fetch2.log"))
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("upstream hit %d times total; cached refetch must not re-hit (want 1)", n)
	}
}

// hostNonLoopbackIPv4 returns a non-loopback IPv4 the host owns, so a
// test upstream bound there is proxy-eligible from the guest. Skips
// when the host has only loopback (env-proxy can't be exercised then).
func hostNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("InterfaceAddrs: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
			return ip4.String()
		}
	}
	t.Skip("no non-loopback IPv4 on host; env-proxy bypasses loopback targets")
	return ""
}

// fetchViaEnv execs a bare daft-http-fetch (no --proxy / --insecure)
// inside the workspace and waits for the success sentinel — the proxy
// routing + CA trust come entirely from the injected container env.
func fetchViaEnv(t *testing.T, c *testdaemons.Cluster, wsName, targetURL, wantSentinel, logPath string) {
	t.Helper()
	proc := startDaftSubprocess(
		t, logPath,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/daft-http-fetch", "--url", targetURL,
	)
	waitForFileContains(t, proc, logPath, wantSentinel, 60*time.Second,
		func() string { return "" })
}
