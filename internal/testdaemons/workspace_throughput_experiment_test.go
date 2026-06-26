//go:build linux && integration

package testdaemons_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/sandboxnet"
	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// Bottleneck investigation: gvisor and sandboxd have both been
// ruled out by layered benches that climb the stack (see PLAN.md
// D6c.3 slice 2 "Throughput floor under investigation"). This
// file holds the two end-to-end variants that exercise the real
// VM path; both are gated on DAFT_RUN_THROUGHPUT_EXPERIMENT=1
// so the default `make integration` doesn't pay the 32 MiB
// transfer cost on every commit.
//
// Decision tree on the reported MiB/s:
//   - ≥10 MiB/s — the in-VM path is fine; the bun-test 50 KB/s
//     is real-network BDP (cache.nixos.org RTT × whatever TCP
//     window the gvisor netstack negotiates).
//   - 100 KB/s – 10 MiB/s — the VM-side path itself contributes
//     a per-byte cost worth pprof'ing on sandboxd.
//   - ≤100 KB/s — same floor as the bun-test, root cause is not
//     the internet at all.
//
// Loopback vs LAN distinguishes the kernel's 127.0.0.0/8 fast-
// path from the local-IP-on-eth0 path. Both stay on the host
// — true off-host traffic would need a second machine — but
// the kernel takes different code paths to each, so a wide gap
// would point at the loopback fast-path masking a real issue.

// TestClusterWorkspaceLoopbackThroughput serves the body on
// 127.0.0.1 and dials the gvisor virtual host-loopback IP
// (NAT'd back to 127.0.0.1). Hits the TCP loopback fast-path
// on the host kernel.
func TestClusterWorkspaceLoopbackThroughput(t *testing.T) {
	if os.Getenv("DAFT_RUN_THROUGHPUT_EXPERIMENT") != "1" {
		t.Skip("set DAFT_RUN_THROUGHPUT_EXPERIMENT=1 to run the bun-test bottleneck experiment")
	}
	runThroughputExperiment(t, throughputVariant{
		label:    "loopback",
		listenIP: "127.0.0.1",
		dialIP:   sandboxnet.DefaultHostLoopbackIP,
	})
}

// TestClusterWorkspaceLANThroughput serves on the host's LAN
// IP and dials the same IP directly from the VM. gvisor's
// promiscuous/spoofing mode accepts the inbound packet even
// though the destination IP isn't in the stack's route table;
// the TCP forwarder catches the unmatched SYN and net.Dial's
// the destination IP unchanged (no NAT entry for the LAN IP).
// The host kernel then routes locally — still loopback under
// the hood for a local IP, but a different code path than
// 127.0.0.0/8 (full IP/TCP processing, no loopback fast-path).
// A close match to the loopback rate means the gvisor stack
// is cleared for any real outbound destination too; a large
// drop would point at the loopback fast-path having hidden a
// real cost.
func TestClusterWorkspaceLANThroughput(t *testing.T) {
	if os.Getenv("DAFT_RUN_THROUGHPUT_EXPERIMENT") != "1" {
		t.Skip("set DAFT_RUN_THROUGHPUT_EXPERIMENT=1 to run the bun-test bottleneck experiment")
	}
	lanIP := discoverHostLANIP(t)
	runThroughputExperiment(t, throughputVariant{
		label:    "lan",
		listenIP: lanIP,
		dialIP:   lanIP,
	})
}

// TestClusterWorkspaceLatencyThroughput is the LAN variant with
// `tc qdisc add dev lo root netem delay <ms>ms` injected
// between cluster bring-up and the fetch. Real RTT to a
// far-internet peer is ~50–150ms; loopback is ~10µs.
// Injecting latency on lo simulates the WAN condition gvisor's
// netstack has to handle when talking to cache.nixos.org —
// without it, the LAN test only proves the *low-RTT* path is
// fast, which doesn't rule out gvisor having a high-RTT
// pathology (small fixed receive window, Reno-vs-CUBIC ramp,
// no buffer auto-tuning, per-packet syscall cost mounting on
// a full pipe of in-flight segments, etc.).
//
// Requires passwordless `sudo tc`; t.Skip otherwise. Cleans
// up the qdisc unconditionally via t.Cleanup, so a crash
// won't leave 50ms latency on lo persistently. The 50ms
// figure is the median continent-to-continent RTT; tune via
// DAFT_THROUGHPUT_LATENCY_MS.
func TestClusterWorkspaceLatencyThroughput(t *testing.T) {
	if os.Getenv("DAFT_RUN_THROUGHPUT_EXPERIMENT") != "1" {
		t.Skip("set DAFT_RUN_THROUGHPUT_EXPERIMENT=1 to run the bun-test bottleneck experiment")
	}
	if err := canRunSudoTc(); err != nil {
		t.Skipf("netem variant needs passwordless `sudo tc`: %v", err)
	}
	latencyMs := 50
	if s := os.Getenv("DAFT_THROUGHPUT_LATENCY_MS"); s != "" {
		var parsed int
		if _, err := fmt.Sscanf(s, "%d", &parsed); err != nil || parsed <= 0 {
			t.Fatalf("DAFT_THROUGHPUT_LATENCY_MS=%q is not a positive int", s)
		}
		latencyMs = parsed
	}
	lanIP := discoverHostLANIP(t)
	runThroughputExperiment(t, throughputVariant{
		label:     fmt.Sprintf("latency%dms", latencyMs),
		listenIP:  lanIP,
		dialIP:    lanIP,
		latencyMs: latencyMs,
	})
}

type throughputVariant struct {
	label     string // log tag; "loopback" / "lan" / "latency50ms"
	listenIP  string // httptest binds here
	dialIP    string // in-VM client dials https://<dialIP>:port/
	latencyMs int    // optional netem delay on lo (0 = no injection)
}

const throughputBodySize = 32 << 20

func runThroughputExperiment(t *testing.T, v throughputVariant) {
	t.Helper()
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	imageRef := pushDockerTarballToRegistry(t, imgPath,
		"test/d6c2-tp-"+v.label+"-nix:v1")

	body := make([]byte, throughputBodySize)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	bodySHA := sha256.Sum256(body)

	srv := startTLSServerOn(t, v.listenIP, body)
	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL %q: %v", srv.URL, err)
	}
	targetURL := fmt.Sprintf("https://%s:%s/", v.dialIP, srvURL.Port())

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)
	mountSrc := t.TempDir()
	wsName := "d6c2-tp-" + v.label

	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C2TP-"+strings.ToUpper(v.label),
		[]testdaemons.WorkspaceStep{
			{Label: "create", Cmd: fmt.Sprintf("%s workspace create --name %s %s", prefix, wsName, orch)},
			{Label: "image", Cmd: fmt.Sprintf("%s workspace image %s --name %s %s", prefix, imageRef, wsName, orch)},
			{Label: "mount", Cmd: fmt.Sprintf("%s workspace mount add /workspace --source %s --name %s %s", prefix, mountSrc, wsName, orch)},
			{Label: "target", Cmd: fmt.Sprintf("%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch)},
			{Label: "launch", Cmd: fmt.Sprintf("%s workspace launch --name %s %s", prefix, wsName, orch)},
		}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	// netem on lo is added AFTER cluster bring-up so the
	// orchestrator↔sandboxd gRPCs don't pay the latency cost
	// during launch (their timeouts are tuned for ~10µs lo).
	// The fetch and its TCP control plane are the only thing
	// that needs to see WAN-realistic RTT.
	if v.latencyMs > 0 {
		injectLoopbackLatency(t, v.latencyMs)
	}

	execCmd := fmt.Sprintf(
		"%s workspace exec %s --tty=false %s -- "+
			"/bin/daft-http-fetch --insecure --url=%s",
		prefix, wsName, orch, targetURL,
	)
	wantSentinel := fmt.Sprintf(
		"daft-http-fetch ok status=200 bytes=%d sha256=%s",
		len(body), hex.EncodeToString(bodySHA[:]),
	)

	// Wall-clock around exec. Includes dial, TLS handshake,
	// body transfer, sha256 compute. Pre-fetch dispatch
	// overhead (~50-100ms for the tmux Enter + shell prompt)
	// is constant; at 32 MiB the transfer dominates.
	start := time.Now()
	s.Enter(execCmd)
	s.WaitFor(wantSentinel, 5*time.Minute)
	elapsed := time.Since(start)

	mbPerSec := float64(throughputBodySize) / (1024 * 1024) / elapsed.Seconds()
	t.Logf("D6C2TP/%s throughput: %d bytes in %s = %.2f MiB/s "+
		"(synthetic bench peer: 25-38 MB/s gvisor<->gvisor)",
		v.label, throughputBodySize, elapsed, mbPerSec)

	if logPath := os.Getenv("DAFT_THROUGHPUT_EXPERIMENT_OUT"); logPath != "" {
		line := fmt.Sprintf("%s %s bytes=%d elapsed_ms=%d rate_mibps=%.2f\n",
			time.Now().UTC().Format(time.RFC3339),
			v.label, throughputBodySize,
			elapsed.Milliseconds(), mbPerSec)
		f, err := os.OpenFile(logPath, //nolint:gosec // throughput log is benign.
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Logf("open %s: %v", logPath, err)
		} else {
			if _, werr := f.WriteString(line); werr != nil {
				t.Logf("write %s: %v", logPath, werr)
			}
			_ = f.Close()
		}
	}
}

// startTLSServerOn binds a TLS server to listenIP:0 and returns
// it. httptest.NewTLSServer always binds 127.0.0.1; we need the
// LAN variant to bind elsewhere, so build the server manually
// via NewUnstartedServer + replaced listener. The auto-cert's
// SAN won't cover a non-loopback IP, but daft-http-fetch is
// invoked with --insecure for these experiments anyway.
func startTLSServerOn(t *testing.T, listenIP string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	ln, err := net.Listen("tcp", net.JoinHostPort(listenIP, "0"))
	if err != nil {
		t.Fatalf("net.Listen %s:0: %v", listenIP, err)
	}
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// discoverHostLANIP returns the first IPv4 address on an
// up, non-loopback, non-virtual interface. DAFT_THROUGHPUT_LAN_IP
// overrides the scan; useful when the heuristic picks the wrong
// interface on a multi-NIC host.
func discoverHostLANIP(t *testing.T) string {
	t.Helper()
	if ip := os.Getenv("DAFT_THROUGHPUT_LAN_IP"); ip != "" {
		return ip
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ipv4 := ipNet.IP.To4()
			if ipv4 == nil {
				continue
			}
			if ipv4.IsLoopback() || ipv4.IsLinkLocalUnicast() {
				continue
			}
			// Skip sandboxnet's own subnet so a stray
			// host-side virtual interface in the same
			// range can't masquerade as the LAN.
			if ipv4[0] == 192 && ipv4[1] == 168 && ipv4[2] == 127 {
				continue
			}
			return ipv4.String()
		}
	}
	t.Skip("no usable LAN IPv4 on this host; set DAFT_THROUGHPUT_LAN_IP")
	return ""
}

// canRunSudoTc checks whether `sudo -n tc` will work without a
// password prompt; used by the netem-latency variant to decide
// between running the experiment and t.Skip'ing.
func canRunSudoTc() error {
	out, err := exec.Command("sudo", "-n", "tc", "-help").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// injectLoopbackLatency adds `netem delay <ms>ms` to the lo
// device's root qdisc and registers t.Cleanup to remove it.
// Affects ALL loopback traffic — including the cluster's own
// orchestrator↔sandboxd gRPC — so callers should only invoke
// this after bring-up has completed. Cleanup runs even on
// panic via the t.Cleanup contract, so a crashed test doesn't
// strand 50ms latency on the dev machine's lo.
func injectLoopbackLatency(t *testing.T, ms int) {
	t.Helper()
	add := exec.Command("sudo", "-n", "tc", "qdisc", "add", "dev", "lo",
		"root", "netem", "delay", fmt.Sprintf("%dms", ms))
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("tc qdisc add netem %dms on lo: %v: %s",
			ms, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		del := exec.Command("sudo", "-n", "tc", "qdisc", "del", "dev", "lo", "root")
		if out, err := del.CombinedOutput(); err != nil {
			t.Logf("tc qdisc del lo (may already be gone): %v: %s",
				err, strings.TrimSpace(string(out)))
		}
	})
}

// isVirtualInterface filters out the bridges + tunnels that
// would route to the host kernel anyway but via paths that
// don't represent "real" LAN traffic (docker bridges loop the
// packet back through the same kernel stack; tailscale's wg
// has its own quirks; veth pairs are typically docker
// internals). enp* / eth* / wlp* / wlan* / en0 etc are kept.
func isVirtualInterface(name string) bool {
	for _, p := range []string{"docker", "br-", "tailscale", "veth", "virbr", "tun", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
