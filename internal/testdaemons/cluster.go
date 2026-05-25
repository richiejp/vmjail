//go:build linux && integration

// Package testdaemons spawns daft-orchestrator and daft-sandboxd
// as real subprocesses for black-box workflow tests. Each
// daemon runs in its own process group (Setpgid) and is
// auto-killed on parent death (Pdeathsig=SIGKILL); the test's
// Cleanup SIGTERMs the whole group, so VMs / pumps / connections
// the daemons spawned inside themselves never outlive the test.
//
// Identities for orchestrator + sandboxd + client are generated
// up front and cross-approved via internal/identity APIs before
// any daemon starts. This is the same handshake the in-process
// e2e tests do, lifted to the subprocess world.
//
// Outside the project's Nix dev shell the daft binaries are
// rebuilt lazily via `go build` at test setup. The Go build
// cache makes this a no-op on warm runs; the first run after a
// source change is ~2s.
package testdaemons

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/sandboxnet"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// WorkspaceStep names one `daft workspace …` command in a test's
// setup sequence. RunWorkspaceSteps wraps each Cmd in a unique
// success sentinel before typing it into a tmux pane.
type WorkspaceStep struct {
	Label string
	Cmd   string
}

// RunWorkspaceSteps types each step into pane and waits for a
// per-index success sentinel `<tag>-<N*1000>`. The sentinel
// renders only in command output (not the typed echo) because the
// trailing printf expands shell arithmetic against the previous
// command's $? — so a substring match in the pane buffer is
// unambiguous. Per-step timeout is uniform; tests handle longer
// post-launch waits (cold image pull, VM boot) separately.
func RunWorkspaceSteps(t *testing.T, pane *testtmux.Session, tag string, steps []WorkspaceStep, timeout time.Duration) {
	t.Helper()
	for i, st := range steps {
		base := (i + 1) * 1000
		full := fmt.Sprintf(`%s; printf '%s-%%d\n' $((%d+$?))`, st.Cmd, tag, base)
		pane.Enter(full)
		pane.WaitFor(fmt.Sprintf("%s-%d", tag, base), timeout)
	}
}

// peersFile mirrors the daemon CLIs' own convention so the
// bootstrap writes to exactly the same path they'll read from.
const peersFile = "peers.json"

// daemonReadyTimeout caps how long NewCluster waits for the
// orchestrator's listener to accept connections.
const daemonReadyTimeout = 10 * time.Second

// Cluster is a one-test orchestrator + sandboxd pair plus a
// pre-bootstrapped client config dir the test can hand to the
// daft CLI via --config-dir / --orchestrator.
type Cluster struct {
	// DaftBin is the absolute path to the freshly-built daft CLI
	// binary; tests typically prefix command lines with it.
	DaftBin string
	// OrchestratorURL is the https://host:port the orchestrator
	// is listening on (loopback, OS-assigned port).
	OrchestratorURL string
	// ClientConfigDir is a fresh config dir holding the client's
	// identity + a peer store that approves the orchestrator.
	ClientConfigDir string
	// MitmProxyBin is the absolute path to the freshly-built
	// daft-mitm-proxy binary, for tests that route in-VM egress
	// through the caching proxy (PLAN.md Phase I).
	MitmProxyBin string
	// EgressProxyURL is the guest-facing URL of the in-stack egress
	// proxy when the cluster was built WithEgressProxy (empty
	// otherwise) — the fixed netstack gateway:port the executor injects
	// as https_proxy.
	EgressProxyURL string

	OrchestratorFP identity.Fingerprint
	SandboxdFP     identity.Fingerprint
	ClientFP       identity.Fingerprint

	// sandboxdOut accumulates everything the sandboxd subprocess
	// writes to stdout/stderr (in addition to t.Log forwarding).
	// Tests assert on it via WaitForSandboxdOutput.
	sandboxdOut *outputCapture
}

// WaitForSandboxdOutput polls the captured sandboxd output until
// substr appears, or fatals the test on timeout (dumping the
// whole capture for diagnosis). Used by tests that need to wait
// on async daemon-side events — like the in-VM container
// banner — that don't surface through the tmux pane.
func (c *Cluster) WaitForSandboxdOutput(t *testing.T, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline via the if-fatal below"
	for {
		got := c.sandboxdOut.Snapshot()
		if strings.Contains(got, substr) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitForSandboxdOutput %q: timeout after %s\n"+
				"--- sandboxd capture ---\n%s",
				substr, timeout, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// NewCluster generates identities, cross-approves them, builds
// the daft binaries (lazily, shared across tests in the same
// process via sync.Once), spawns the orchestrator + sandboxd
// subprocesses, and waits for the orchestrator's listener to be
// reachable.
//
// Registers Cleanup that SIGTERMs each daemon's process group
// and waits for exit; Pdeathsig=SIGKILL guarantees that even a
// crashed test binary doesn't leak daemons.
func NewCluster(t *testing.T, opts ...ClusterOption) *Cluster {
	t.Helper()

	var o clusterOpts
	for _, fn := range opts {
		fn(&o)
	}

	requireSandboxArtefacts(t)
	bins := ensureBinaries(t)

	base := t.TempDir()
	orchDir := mkConfigDir(t, base, "orch")
	sbxDir := mkConfigDir(t, base, "sbx")
	cliDir := mkConfigDir(t, base, "cli")

	orchID := mustLoadOrGen(t, orchDir, identity.RoleOrchestrator)
	sbxID := mustLoadOrGen(t, sbxDir, identity.RoleSandboxHost)
	cliID := mustLoadOrGen(t, cliDir, identity.RoleClient)

	// Four-way cross-approval: each side trusts the others it
	// will interact with. Orchestrator talks to sandboxd + client;
	// sandboxd + client both dial the orchestrator.
	approvePeer(t, orchDir, sbxID.Fingerprint())
	approvePeer(t, orchDir, cliID.Fingerprint())
	approvePeer(t, sbxDir, orchID.Fingerprint())
	approvePeer(t, cliDir, orchID.Fingerprint())

	addr := freeLoopbackAddr(t)
	url := "https://" + addr

	orchOut := newOutputCapture(t, "daft-orchestrator")
	sbxOut := newOutputCapture(t, "daft-sandboxd")

	startDaemon(t, orchOut, nil, bins.orchestrator,
		"serve", "--addr", addr, "--config-dir", orchDir)
	waitForTCP(t, addr, daemonReadyTimeout)

	// When the egress proxy is requested, enable the in-stack proxy via
	// sandboxd's env: sandboxd builds the proxy (and its CA) in-process
	// and serves it on every VM's netstack gateway, so every
	// image-backed dispatch routes guest TLS through it. Caching is on
	// (a temp dir) so a repeat fetch replays without a second upstream
	// hit; upstream verification is skipped because tests use
	// self-signed upstreams.
	// The proxy is on by default in sandboxd, so tests that don't ask
	// for it must explicitly opt out — otherwise every cluster would
	// run default-deny and break workloads that egress directly.
	sbxEnv := []string{"DAFT_EGRESS_PROXY=0"}
	var proxyURL string
	if o.egressProxy {
		proxyURL = "http://" + net.JoinHostPort(
			sandboxnet.DefaultGatewayIP, strconv.Itoa(int(sandboxnet.DefaultProxyPort)),
		)
		sbxEnv = []string{
			"DAFT_EGRESS_PROXY=1",
			"DAFT_EGRESS_PROXY_INSECURE_UPSTREAM=1",
			"DAFT_EGRESS_PROXY_CACHE_DIR=" + t.TempDir(),
		}
	}

	startDaemon(t, sbxOut, sbxEnv, bins.sandboxd,
		"serve", "--orchestrator", url, "--config-dir", sbxDir)
	// Sandboxd registration is asynchronous — the test asserts on
	// daft node list output to decide when registration completed.

	return &Cluster{
		DaftBin:         bins.daft,
		OrchestratorURL: url,
		ClientConfigDir: cliDir,
		MitmProxyBin:    bins.mitmProxy,
		EgressProxyURL:  proxyURL,
		OrchestratorFP:  orchID.Fingerprint(),
		SandboxdFP:      sbxID.Fingerprint(),
		ClientFP:        cliID.Fingerprint(),
		sandboxdOut:     sbxOut,
	}
}

// ClusterOption tunes optional cluster topology.
type ClusterOption func(*clusterOpts)

type clusterOpts struct {
	egressProxy bool
}

// WithEgressProxy makes the cluster run the in-stack egress proxy:
// sandboxd builds the proxy (and its CA) in-process and serves it on
// every VM's netstack gateway, so guest TLS egress routes through (and
// trusts) it via the executor's Phase-J injection. No separate process
// or VIP — just the DAFT_EGRESS_PROXY* env that NewCluster sets.
func WithEgressProxy() ClusterOption {
	return func(o *clusterOpts) { o.egressProxy = true }
}

// binaries holds absolute paths to the freshly-built daft, daft-
// orchestrator, and daft-sandboxd binaries.
type binaries struct {
	daft         string
	orchestrator string
	sandboxd     string
	mitmProxy    string
}

//nolint:gochecknoglobals // sync.Once shared across tests in the same process so binaries build once.
var (
	binsOnce sync.Once
	bins     binaries
	binsErr  error
)

// ensureBinaries builds daft-* into a per-process temp dir on
// the first call and returns the cached paths thereafter. Build
// happens via `go build`, which uses Go's normal build cache.
func ensureBinaries(t *testing.T) binaries {
	t.Helper()
	binsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "daft-testdaemons-bin-")
		if err != nil {
			binsErr = fmt.Errorf("MkdirTemp: %w", err)
			return
		}
		root, err := repoRoot()
		if err != nil {
			binsErr = err
			return
		}
		built := binaries{}
		targets := []struct{ name, pkg string }{
			{"daft", "./cmd/daft"},
			{"daft-orchestrator", "./cmd/daft-orchestrator"},
			{"daft-sandboxd", "./cmd/daft-sandboxd"},
			{"daft-mitm-proxy", "./cmd/daft-mitm-proxy"},
		}
		for _, tgt := range targets {
			outPath := filepath.Join(dir, tgt.name)
			//nolint:gosec // G204: tgt.name + tgt.pkg are package constants; test-only.
			cmd := exec.Command("go", "build", "-o", outPath, tgt.pkg)
			cmd.Dir = root
			if out, berr := cmd.CombinedOutput(); berr != nil {
				binsErr = fmt.Errorf("go build %s: %w\n%s", tgt.name, berr, out)
				return
			}
			switch tgt.name {
			case "daft":
				built.daft = outPath
			case "daft-orchestrator":
				built.orchestrator = outPath
			case "daft-sandboxd":
				built.sandboxd = outPath
			case "daft-mitm-proxy":
				built.mitmProxy = outPath
			}
		}
		bins = built
	})
	if binsErr != nil {
		t.Fatalf("testdaemons: build binaries: %v", binsErr)
	}
	return bins
}

// repoRoot returns the absolute path to the module root by
// asking `go env GOMOD` for the active go.mod path.
func repoRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	modPath := strings.TrimSpace(string(out))
	if modPath == "" || modPath == "/dev/null" {
		return "", errors.New("testdaemons: not inside a Go module")
	}
	return filepath.Dir(modPath), nil
}

// requireSandboxArtefacts fatals if the env vars the daft-
// sandboxd binary needs to start are not set. Without them
// sandboxd would exit ~10ms after spawn and the test would only
// notice via the WaitFor timeout 15s later. Failing fast here
// also gives a clearer pointer ("run via make integration" vs.
// a daemon stderr dump in the test log).
func requireSandboxArtefacts(t *testing.T) {
	t.Helper()
	required := []string{
		"DAFT_INITRAMFS", "DAFT_KERNEL", "DAFT_VIRTIOFSD",
		"DAFT_CLOUD_HYPERVISOR",
	}
	var missing []string
	for _, k := range required {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("testdaemons: required env vars not set: %s\n"+
			"run via `make integration` (which sets them) or "+
			"`nix develop` + `make integration-build`",
			strings.Join(missing, ", "))
	}
}

func mkConfigDir(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	return dir
}

func mustLoadOrGen(t *testing.T, dir string, role identity.Role) *identity.Identity {
	t.Helper()
	id, err := identity.LoadOrGenerate(dir, role)
	if err != nil {
		t.Fatalf("LoadOrGenerate(%s, %s): %v", dir, role, err)
	}
	return id
}

func approvePeer(t *testing.T, dir string, fp identity.Fingerprint) {
	t.Helper()
	store, err := identity.OpenPeerStore(filepath.Join(dir, peersFile))
	if err != nil {
		t.Fatalf("OpenPeerStore(%s): %v", dir, err)
	}
	if err := store.Add(identity.Peer{Fingerprint: fp}); err != nil {
		t.Fatalf("PeerStore.Add(%s, %s): %v", dir, fp, err)
	}
}

// freeLoopbackAddr binds 127.0.0.1:0, captures the OS-assigned
// port, then closes the listener so the orchestrator can claim
// it. Brief race with other processes; acceptable for tests.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// startDaemon spawns prog with args under its own process group,
// forwards stdout/stderr through the provided outputCapture (so
// the test can both Log and assert on what the daemon wrote),
// and registers a Cleanup that SIGTERMs the whole group then
// Waits with a bounded delay. Pdeathsig=SIGKILL means a crashed
// test process can't leak the daemon even if Cleanup never runs.
func startDaemon(t *testing.T, out *outputCapture, extraEnv []string, prog string, args ...string) *exec.Cmd {
	t.Helper()
	//nolint:gosec // G204: prog + args are caller-controlled test-only paths.
	cmd := exec.Command(prog, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	cmd.Stdout = out
	cmd.Stderr = out
	// Bound how long cmd.Wait will tolerate a stuck pipe after
	// the process itself has exited; without this, a daemon that
	// leaves a goroutine pinning a stdio fd could hang Cleanup.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", prog, err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		// Negative PID = whole process group. ESRCH for an
		// already-exited group is benign.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
	})
	return cmd
}

// waitForTCP polls Dial("tcp", addr) until it succeeds or the
// timeout expires. Used to gate test progress on the
// orchestrator's listener being ready.
func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline via the if-fatal below"
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForTCP %s: %v after %s", addr, err, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// outputCapture forwards every Write to t.Logf (so daemon output
// shows in the test trace on failure) AND accumulates into a
// thread-safe buffer Snapshot can return. The two-roles split
// lets tests both see daemon-side messages live and assert on
// them with WaitForSandboxdOutput's substring search.
type outputCapture struct {
	t      *testing.T
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

func newOutputCapture(t *testing.T, prefix string) *outputCapture {
	return &outputCapture{t: t, prefix: prefix}
}

func (o *outputCapture) Write(p []byte) (int, error) {
	o.t.Logf("[%s] %s", o.prefix, strings.TrimRight(string(p), "\n"))
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *outputCapture) Snapshot() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}
