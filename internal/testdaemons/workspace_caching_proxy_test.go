//go:build linux && integration

package testdaemons_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/sandboxnet"
	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// proxyListenPrefix is the readiness line daft-mitm-proxy prints on
// startup (mirrors the binary's readyPrefix const). The test scrapes
// the OS-assigned port from the text after it.
const proxyListenPrefix = "daft-mitm-proxy listening on "

// TestClusterWorkspaceCachingProxy is PLAN.md Phase I slice I1's
// end-to-end proof: an in-VM HTTPS GET routed through daft-mitm-proxy
// (running on the sandboxd-host loopback, reached via sandboxnet's
// DefaultHostLoopbackIP NAT) is recorded into the content-addressed
// cache on the first request and replayed from it on the second —
// with the upstream server *shut down* before the replay, so a
// success can only have come from the cache.
//
// Byte path each request:
//
//	container → daft-http-fetch (--proxy 192.168.127.254:P) →
//	tap0 → vsock → sandboxnet → NAT 192.168.127.254:P → host's
//	daft-mitm-proxy → (record) upstream httptest TLS server /
//	(replay) cache.Store
//
// daft-http-fetch trusts the proxy's MITM leaf via --insecure (same
// flag the D6c.2 egress test uses for the self-signed httptest cert)
// and routes through the proxy via its --proxy flag (the minimal
// nix-shell-image has no /bin/sh to set https_proxy through).
//
// The two phases use separate proxy processes sharing one --cache-dir
// so the test also exercises httpcache's cross-process index
// persistence; the offline proxy serves only from that dir.
func TestClusterWorkspaceCachingProxy(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/i1-cache-nix:v1")

	body := []byte("i1-caching-proxy-body\n")
	bodySHA := sha256.Sum256(body)
	var upstreamHits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write(body)
	}))
	srvClosed := false
	t.Cleanup(func() {
		if !srvClosed {
			srv.Close()
		}
	})

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL %q: %v", srv.URL, err)
	}
	// The proxy runs on the host, so it dials the upstream at the
	// host-local 127.0.0.1:<port>. The VM never resolves this — it
	// only forwards a CONNECT for it to the proxy. Port is preserved
	// across the NAT, so it's also the cache key both phases share.
	targetURL := fmt.Sprintf("https://127.0.0.1:%s/", srvURL.Port())

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)
	mountSrc := t.TempDir()
	cacheDir := t.TempDir()
	wsName := "i1-cache"

	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "I1CACHE", []testdaemons.WorkspaceStep{
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

	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	wantSentinel := fmt.Sprintf("daft-http-fetch ok status=200 bytes=%d sha256=%s",
		len(body), hex.EncodeToString(bodySHA[:]))

	// Phase 1 — record. Online proxy fetches upstream once and stores.
	recordPort := startCachingProxy(t, c, cacheDir, false)
	fetchThroughProxy(t, c, wsName, recordPort, targetURL, wantSentinel,
		filepath.Join(t.TempDir(), "fetch-record.log"))
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("upstream hit %d times during record, want 1", n)
	}

	// Phase 2 — replay offline. Kill the upstream so a refetch is
	// impossible, then serve the same request from the shared cache
	// dir via an --offline proxy. Success ⟹ the body came from cache.
	srv.Close()
	srvClosed = true
	replayPort := startCachingProxy(t, c, cacheDir, true)
	fetchThroughProxy(t, c, wsName, replayPort, targetURL, wantSentinel,
		filepath.Join(t.TempDir(), "fetch-replay.log"))
	if n := upstreamHits.Load(); n != 1 {
		t.Fatalf("upstream hit %d times total; offline replay must not refetch (want 1)", n)
	}
}

// startCachingProxy launches daft-mitm-proxy on the host loopback
// with the shared cacheDir, waits for its listening line, and returns
// the OS-assigned port — which the VM reaches at
// DefaultHostLoopbackIP:<port> through sandboxnet's NAT. offline opens
// the cache in serve-only mode (a miss 504s instead of dialing
// upstream). The process is killed at test cleanup.
func startCachingProxy(t *testing.T, c *testdaemons.Cluster, cacheDir string, offline bool) string {
	t.Helper()
	mode := "record"
	if offline {
		mode = "replay"
	}
	logPath := filepath.Join(t.TempDir(), "proxy-"+mode+".log")
	args := []string{
		"--addr", "127.0.0.1:0",
		"--insecure-upstream", // upstream is httptest's self-signed cert
		"--cache-dir", cacheDir,
	}
	if offline {
		args = append(args, "--offline")
	}
	proc := startDaftSubprocess(t, logPath, c.MitmProxyBin, args...)
	waitForFileContains(t, proc, logPath, proxyListenPrefix, 15*time.Second,
		func() string { return "" })

	data, err := os.ReadFile(logPath) //nolint:gosec // logPath is under t.TempDir(); test-only.
	if err != nil {
		t.Fatalf("read proxy log %s: %v", logPath, err)
	}
	addr := parseProxyAddr(t, string(data))
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split proxy addr %q: %v", addr, err)
	}
	return port
}

// parseProxyAddr extracts the host:port that follows proxyListenPrefix
// in the proxy's log output.
func parseProxyAddr(t *testing.T, logText string) string {
	t.Helper()
	for _, line := range strings.Split(logText, "\n") {
		if i := strings.Index(line, proxyListenPrefix); i >= 0 {
			return strings.TrimSpace(line[i+len(proxyListenPrefix):])
		}
	}
	t.Fatalf("no %q line in proxy log:\n%s", proxyListenPrefix, logText)
	return ""
}

// fetchThroughProxy runs a one-shot daft-http-fetch inside the
// workspace, routed through the host proxy via --proxy, and waits for
// the success sentinel in its own log file (a fresh file per call so
// the two phases' identical sentinels don't cross-match). The binary
// is exec'd directly — the minimal nix-shell-image has no usable
// /bin/sh to set env through, which is why daft-http-fetch takes an
// explicit --proxy rather than reading https_proxy.
func fetchThroughProxy(
	t *testing.T, c *testdaemons.Cluster, wsName, proxyPort, targetURL, wantSentinel, logPath string,
) {
	t.Helper()
	proxyURL := fmt.Sprintf("http://%s:%s", sandboxnet.DefaultHostLoopbackIP, proxyPort)
	proc := startDaftSubprocess(
		t, logPath,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/daft-http-fetch",
		"--insecure", "--proxy", proxyURL, "--url", targetURL,
	)
	waitForFileContains(t, proc, logPath, wantSentinel, 60*time.Second,
		func() string { return "" })
}
