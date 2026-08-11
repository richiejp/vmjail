//go:build linux && integration

package testdaemons_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/sandboxnet"
	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceHTTPSEgress is the D6c.2 slice-4 proof: a
// container inside the sandbox VM reaches an HTTPS endpoint
// running on the sandboxd-host's loopback through the sandboxnet
// userspace TCP/IP stack. The full byte path:
//
//	container → tap0 → vsock (HyperKit framing) → sandboxd's
//	sandboxnet (gvisor-tap-vsock) → NAT 192.168.127.254→127.0.0.1
//	→ host's httptest TLS server.
//
// daft-http-fetch runs inside the container via
// `daft workspace exec` and prints
//
//	daft-http-fetch ok status=200 bytes=<n> sha256=<hex>
//
// on success. The test waits for that sentinel on the tmux pane
// and cross-checks bytes/sha256 against what the host served, so
// a partial-read or framing-corruption regression surfaces as a
// mismatch rather than a sentinel-missing timeout.
//
// --insecure is required because httptest.NewTLSServer ships a
// self-signed cert; daft-http-fetch's --insecure unit test pins
// that the default (verify-on) path still rejects the same cert,
// so this slice doesn't accidentally pass on a TLS-skip default.
func TestClusterWorkspaceHTTPSEgress(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6c2-s4-nix:v1")

	body := []byte("d6c2-slice4-https-egress-body\n")
	bodySHA := sha256.Sum256(body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL %q: %v", srv.URL, err)
	}
	// httptest binds 127.0.0.1 on a free port. Rewrite the host
	// to sandboxnet.DefaultHostLoopbackIP so the container dials
	// the NAT'd address — port is unchanged because the netstack
	// preserves the destination port when translating.
	targetURL := fmt.Sprintf("https://%s:%s/",
		sandboxnet.DefaultHostLoopbackIP, srvURL.Port())

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	wsName := "d6c2-s4"

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C2S4", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --preserve-command --name %s %s",
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

	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	// One-shot exec — daft-http-fetch GETs the URL once and
	// exits. --tty=false drops the local raw-mode dance; the
	// sentinel surfaces in the pane buffer as the only output the
	// container produces, so WaitFor lands deterministically.
	execCmd := fmt.Sprintf(
		"%s workspace exec %s --tty=false %s -- "+
			"/bin/daft-http-fetch --insecure --url=%s",
		prefix, wsName, orch, targetURL,
	)
	s.Enter(execCmd)
	wantSentinel := fmt.Sprintf(
		"daft-http-fetch ok status=200 bytes=%d sha256=%s",
		len(body), hex.EncodeToString(bodySHA[:]),
	)
	s.WaitFor(wantSentinel, 60*time.Second)
}
