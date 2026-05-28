//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// narinfoProbeBody is the body cache.nixos.org returns for a 404
// narinfo (text/plain "404", three bytes — confirmed by host-side
// curl against the live endpoint). The probe asserts curl, going
// through the same egress-proxy path nix uses, receives this body
// unchanged.
const narinfoProbeBody = "404"

// narinfoProbeURL is a deterministic 404 path: an all-zero
// store-hash that has not been a real Nix derivation and never
// will be. cache.nixos.org returns 404 for it every time, with a
// 3-byte "404" body and Content-Type: text/plain. The probe hits
// the live endpoint so the response goes through the same upstream
// the production nix narinfo lookups use; that's the whole point
// of the diagnostic. cache.nixos.org's 404 shape has been stable
// for years.
const narinfoProbeURL = "https://cache.nixos.org/0000000000000000000000000000000000.narinfo"

// TestClusterWorkspace404NarinfoProbe is a focused diagnostic for
// the D6c.4 selfbuild bug. The unit test
// mitmproxy.TestPassthrough404PreservesStatusAndBody already proves
// the proxy itself preserves 404 status + body verbatim in
// isolation; this test runs the same shape end-to-end through the
// sandbox stack — sandboxnet vsock framing, the in-stack egress
// proxy, the per-host leaf cert chain, and the in-VM HTTP client —
// using curl rather than nix, against the real cache.nixos.org
// upstream nix would have hit.
//
// If curl receives 404 + "404", the proxy + sandbox chain are
// faithful and the original bug is purely in nix's narinfo client.
// If curl sees something different (a different status, a body the
// upstream didn't send, a connection reset), the chain is at fault
// and the failure log pins down which layer.
func TestClusterWorkspace404NarinfoProbe(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_TOOLING_IMAGE",
		"nix build .#nix-tooling-image -o bin/nix-tooling-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6c4-narinfo-probe:v1")

	c := testdaemons.NewCluster(t, testdaemons.WithEgressProxy())
	s := testtmux.New(t)

	logDir := t.TempDir()
	curlLog := filepath.Join(logDir, "curl.log")
	wsName := "d6c4-narinfo-probe"
	// Workspace launch requires a mount even if the probe does not
	// read from it; an empty tempdir satisfies the "not ready,
	// missing: [mount]" precondition.
	mountSrc := t.TempDir()

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C4P", []testdaemons.WorkspaceStep{
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

	c.WaitForSandboxdOutput(t, "daft-nix-tooling ready", 60*time.Second)

	// daft workspace exec doesn't inherit the entrypoint's
	// process.env (crun-exec gap; see the bun + selfbuild tests),
	// so set HTTPS_PROXY + CURL_CA_BUNDLE explicitly. Verbose curl
	// flags chosen so a failure surface is rich:
	//   -s   no progress meter (would race the tee)
	//   -v   trace request + response to stderr
	//   -i   include headers in stdout (body capture)
	//   --max-time 10s   bound the probe; the assertions below
	//                    fail fast on either status or body mismatch
	// Trailing `echo DAFT-CURL-DONE exit=$?` gives a stable
	// sentinel that fires after curl exits, regardless of which
	// version-specific verbose-trace strings the curl in the
	// image prints. waitForFileContains anchors on it.
	execShell := fmt.Sprintf(
		"exec env HTTPS_PROXY=%s CURL_CA_BUNDLE=/etc/daft-egress-ca.pem "+
			"/bin/sh -c '/bin/curl -sv -i --max-time 10 %q; "+
			"printf \"\\nDAFT-CURL-DONE exit=%%d\\n\" $?'",
		c.EgressProxyURL, narinfoProbeURL,
	)
	curlProc := startDaftSubprocess(
		t, curlLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", execShell,
	)

	// curl --max-time 10 bounds the inner request; this outer wait
	// gives the daft exec round-trip room. We anchor on curl's "*
	// Closing connection" verbose trace, which fires after the
	// full response (headers + body) has been read — at that point
	// the log file is complete and any assertion below sees curl's
	// final view of the wire. The dump on failure shows whatever
	// curl actually saw so a divergence is diagnosable straight
	// from the test report.
	waitForFileContains(t, curlProc, curlLog,
		"DAFT-CURL-DONE", 30*time.Second,
		func() string { return s.Capture() })

	out, err := os.ReadFile(curlLog) //nolint:gosec // path is under t.TempDir(); test-only.
	if err != nil {
		t.Fatalf("read curl log: %v", err)
	}
	got := string(out)
	// The wire-format regression we care about: the response
	// status line, as printed by curl's `< ` verbose-trace prefix,
	// must say HTTP/1.1. Without mitmproxy's Proto normalisation
	// it said HTTP/2.0 (which curl forgivingly downgraded but
	// strict clients like nix mis-parsed). "HTTP/2.0" appearing
	// anywhere in the response status line is an automatic fail.
	if !strings.Contains(got, "< HTTP/1.1 404") {
		t.Fatalf("wire status not HTTP/1.1 404 — proxy Proto-normalisation regressed?\n%s", got)
	}
	if strings.Contains(got, "< HTTP/2.0") {
		t.Fatalf("wire status leaked upstream HTTP/2 to client:\n%s", got)
	}
	// `DAFT-CURL-DONE exit=0` is printed by the wrapper after curl
	// returns success — the response was complete, the
	// Content-Length the proxy sent matched the body bytes that
	// followed. Body content itself is interleaved with curl's
	// verbose-trace `*` lines and not worth parsing here; the
	// status + exit-code pair is what nails the proxy bug.
	if !strings.Contains(got, "DAFT-CURL-DONE exit=0") {
		t.Fatalf("curl did not exit 0:\n%s", got)
	}
}
