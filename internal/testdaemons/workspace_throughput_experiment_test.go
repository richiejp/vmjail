//go:build linux && integration

package testdaemons_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/sandboxnet"
	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceLoopbackThroughput is the third datapoint
// in the bun-test bottleneck investigation, after the two
// internal/sandboxnet benches localized the gvisor switch and
// the TCP forwarder both at ~30 MB/s (~500× the bun-test
// observation of 50 KB/s).
//
// Those benches were gvisor↔gvisor in-process; the real workload
// is Linux-kernel-TCP-in-VM ↔ gvisor-on-host, traversing two more
// layers the bench skipped: CH's virtio-net backend and the
// vsock/UDS hop between CH and sandboxd. This test exercises
// the full real-VM path against a host-loopback listener so the
// upstream-internet variable is held constant.
//
// Decision tree on the reported MB/s:
//   - ≥10 MB/s — the in-VM path is fine; the bun-test 50 KB/s
//     is almost certainly real-network BDP (cache.nixos.org RTT
//     × whatever TCP window the gvisor netstack negotiates).
//     Next step is to inspect window scaling in a tcpdump from
//     inside the VM, not to optimise sandboxd.
//   - 100 KB/s – 10 MB/s — the VM-side path itself contributes
//     a per-byte cost worth pprof'ing on sandboxd. The
//     synthetic bench's gvisor↔gvisor result (~30 MB/s) is
//     unreachable from a Linux-kernel client, so there's
//     TCP-interop slack to recover.
//   - ≤100 KB/s — same floor as the bun-test, root cause is
//     not the internet at all. Next bench drills into the
//     CH/virtio-net/UDS hop or per-packet stalls.
//
// Gated on DAFT_RUN_THROUGHPUT_EXPERIMENT=1 so the default
// `make integration` run doesn't pay the body-generation cost
// (32 MiB sha256 + transfer) and doesn't fail-mask the
// no-decision report (this test t.Logs the rate but never
// fails on a slow rate, only on a wrong byte count or
// sentinel-missing).
func TestClusterWorkspaceLoopbackThroughput(t *testing.T) {
	if os.Getenv("DAFT_RUN_THROUGHPUT_EXPERIMENT") != "1" {
		t.Skip("set DAFT_RUN_THROUGHPUT_EXPERIMENT=1 to run the bun-test bottleneck experiment")
	}

	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6c2-throughput-nix:v1")

	const bodySize = 32 << 20 // 32 MiB
	body := make([]byte, bodySize)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	bodySHA := sha256.Sum256(body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL %q: %v", srv.URL, err)
	}
	targetURL := fmt.Sprintf("https://%s:%s/",
		sandboxnet.DefaultHostLoopbackIP, srvURL.Port())

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	wsName := "d6c2-throughput"

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C2TP", []testdaemons.WorkspaceStep{
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

	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	execCmd := fmt.Sprintf(
		"%s workspace exec %s --tty=false %s -- "+
			"/bin/daft-http-fetch --insecure --url=%s",
		prefix, wsName, orch, targetURL,
	)

	wantSentinel := fmt.Sprintf(
		"daft-http-fetch ok status=200 bytes=%d sha256=%s",
		len(body), hex.EncodeToString(bodySHA[:]),
	)

	// Measure wall time around the exec. Includes dial,
	// handshake, body transfer, sha256 compute. Pre-fetch
	// dispatch overhead (~50-100ms for the tmux Enter +
	// shell prompt) is constant; at 32 MiB the transfer
	// dominates.
	start := time.Now()
	s.Enter(execCmd)
	s.WaitFor(wantSentinel, 5*time.Minute)
	elapsed := time.Since(start)

	mbPerSec := float64(bodySize) / (1024 * 1024) / elapsed.Seconds()
	t.Logf("D6C2TP throughput: %d bytes in %s = %.2f MiB/s "+
		"(synthetic bench peer: 25-38 MB/s gvisor<->gvisor)",
		bodySize, elapsed, mbPerSec)

	// Persist to a stable location so subsequent CI / local
	// reruns can be compared without grepping the test log.
	logPath := os.Getenv("DAFT_THROUGHPUT_EXPERIMENT_OUT")
	if logPath != "" {
		line := fmt.Sprintf("%s loopback bytes=%d elapsed_ms=%d rate_mibps=%.2f\n",
			time.Now().UTC().Format(time.RFC3339),
			bodySize, elapsed.Milliseconds(), mbPerSec)
		if werr := os.WriteFile(logPath, []byte(line),
			0o644); werr != nil { //nolint:gosec // throughput log is benign.
			t.Logf("write %s: %v", logPath, werr)
		}
	}
}
