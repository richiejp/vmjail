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
	"io"
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

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/sandboxdcli"
	"github.com/richiejp/darkaifactory/internal/sandboxnet"
	"github.com/richiejp/darkaifactory/internal/testtmux"
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

// Cluster is a one-test orchestrator plus one or more sandboxd Nodes and a
// pre-bootstrapped client config dir the test can hand to the
// daft CLI via --config-dir / --orchestrator.
type Cluster struct {
	// DaftBin is the absolute path to the freshly-built daft CLI
	// binary; tests typically prefix command lines with it.
	DaftBin string
	// MCPBin is the absolute path to the freshly-built daft-mcp stdio
	// server. Tests use it through the MCP SDK's CommandTransport so the
	// executable's configuration and transport boundary are both exercised.
	MCPBin string
	// OrchestratorURL is the https://host:port the orchestrator
	// is listening on (loopback, OS-assigned port).
	OrchestratorURL string
	// ClientConfigDir is a fresh config dir holding the client's
	// identity + a peer store that approves the orchestrator.
	ClientConfigDir string
	// SandboxdConfigDir is the first Node's durable config/state root. Tests
	// use it only for on-host lifecycle assertions such as retained disk and
	// snapshot staging cleanup.
	SandboxdConfigDir string
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
	// SandboxdFPs contains every Node fingerprint in startup order.
	// SandboxdFP remains an alias for the first entry for existing tests.
	SandboxdFPs []identity.Fingerprint
	ClientFP    identity.Fingerprint

	// ObserverDir is the per-sandbox state root NewCluster points the
	// sandboxd subprocess at (via DAFT_STATE_DIR), so every
	// orchestrator-dispatched VM writes its Tier-1 observer log to
	// <ObserverDir>/<sandbox-uid>/observer.jsonl. It lives under the
	// test's TempDir and is dumped (tail) into the test log on failure
	// — see the dumpObserverLogs cleanup NewCluster registers. See
	// OBSERVABILITY.md for the per-VM /proc + cgroup + net sampler.
	ObserverDir string

	// sandboxdOut accumulates everything the sandboxd subprocess
	// writes to stdout/stderr (in addition to t.Log forwarding).
	// Tests assert on it via WaitForSandboxdOutput.
	sandboxdOut *outputCapture
	sandboxdCmd *exec.Cmd
}

// StopSandboxd terminates the test Node process group without stopping the
// orchestrator. It lets recovery tests exercise a genuine registered-Node
// disconnect while keeping the public control plane available.
func (c *Cluster) StopSandboxd() error {
	if c == nil || c.sandboxdCmd == nil || c.sandboxdCmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-c.sandboxdCmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
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

// newObserverStateDir returns the per-test sandbox state root that
// NewCluster points sandboxd at (via DAFT_STATE_DIR), so every
// dispatched VM's Tier-1 trace lands at <dir>/<uid>/observer.jsonl. It
// registers a failure-only Cleanup that dumps the tail of each log into
// the test log — the TempDir is wiped on cleanup, so citing the path
// alone wouldn't make the trace grabbable. Registered before the
// daemons start, so the Cleanup runs (LIFO) after they're torn down and
// the logs have stopped growing.
func newObserverStateDir(t *testing.T, base string) string {
	t.Helper()
	dir := filepath.Join(base, "observer-state")
	t.Cleanup(func() {
		if t.Failed() {
			dumpObserverLogs(t, dir)
		}
	})
	return dir
}

// observerTailBytes caps how much of each observer log's tail is
// read for the failure dump. ~64 KiB is hundreds of 1 Hz JSON-lines
// samples — far more than enough to see what the VM was doing as a
// test failed, without spilling a 25 MB live log into the test output.
const observerTailBytes = 64 * 1024

// observerTailLines caps the number of trailing sample lines logged
// per observer file, so even a dense log stays a readable tail.
const observerTailLines = 60

// dumpObserverLogs walks the per-test observer-state root and logs the
// path plus a bounded tail of every observer.jsonl (and rotated .1
// backup) it finds. Called from a failure-only Cleanup because the
// test's TempDir — and these logs with it — is wiped once Cleanup
// returns, so the trace has to be surfaced into the test log here or
// it's gone. Best-effort: a sandbox that never booted leaves no file
// and an unreadable one is noted, neither fails the (already-failing)
// test further.
func dumpObserverLogs(t *testing.T, observerDir string) {
	t.Helper()
	var found int
	_ = filepath.WalkDir(observerDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip an unreadable/absent entry and keep walking; a missing log is non-fatal to an already-failing test.
		}
		name := d.Name()
		if name != "observer.jsonl" && name != "observer.jsonl.1" {
			return nil
		}
		found++
		tail, terr := readTail(path, observerTailBytes, observerTailLines)
		if terr != nil {
			t.Logf("observer log %s: unreadable: %v", path, terr)
			return nil
		}
		t.Logf("observer log (Tier-1 host trace) %s — last %d lines:\n%s",
			path, observerTailLines, tail)
		return nil
	})
	if found == 0 {
		t.Logf("no observer logs under %s (no sandbox reached boot?)", observerDir)
	}
}

// readTail returns the last up-to-maxLines lines from the final
// maxBytes of the file at path, without reading the whole file (an
// observer log can reach ~25 MB before rotation).
func readTail(path string, maxBytes int64, maxLines int) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: test-harness dump of a path under the test's own TempDir.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		// The first line is probably a fragment of a sample split by the
		// byte cap — drop it so every logged line is whole JSON.
		lines = lines[1:]
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n"), nil
}

// NewCluster generates identities, cross-approves them, builds
// the daft binaries (lazily, shared across tests in the same
// process via sync.Once), spawns the orchestrator + sandboxd
// subprocesses, and waits for the orchestrator's listener and
// sandboxd registration to be reachable.
//
// Registers Cleanup that SIGTERMs each daemon's process group
// and waits for exit; Pdeathsig=SIGKILL guarantees that even a
// crashed test binary doesn't leak daemons.
func NewCluster(t *testing.T, opts ...ClusterOption) *Cluster {
	t.Helper()

	o := clusterOpts{sandboxdCount: 1}
	for _, fn := range opts {
		fn(&o)
	}
	if o.sandboxdCount < 1 {
		t.Fatalf("sandboxd count must be at least 1, got %d", o.sandboxdCount)
	}

	// The bundled binary carries its artefacts embedded, so it needs
	// neither the artefact env vars nor (below) a go-built binary.
	if o.bundledSandboxd == "" {
		requireSandboxArtefacts(t)
	}
	bins := ensureBinaries(t)
	sandboxdBin := bins.sandboxd
	if o.bundledSandboxd != "" {
		sandboxdBin = o.bundledSandboxd
	}

	base := t.TempDir()
	orchDir := mkConfigDir(t, base, "orch")
	cliDir := mkConfigDir(t, base, "cli")

	orchID := mustLoadOrGen(t, orchDir, identity.RoleOrchestrator)
	sbxDirs := make([]string, o.sandboxdCount)
	sbxFPs := make([]identity.Fingerprint, o.sandboxdCount)
	clientRole := o.clientRole
	if clientRole == "" {
		clientRole = identity.RoleClient
	}
	cliID := mustLoadOrGen(t, cliDir, clientRole)

	// Cross-approve every Node before starting the orchestrator, which loads
	// its peer store at startup. Nodes and the client only dial orchestrator.
	for i := range o.sandboxdCount {
		sbxDirs[i] = mkConfigDir(t, base, fmt.Sprintf("sbx-%d", i))
		sbxID := mustLoadOrGen(t, sbxDirs[i], identity.RoleSandboxHost)
		sbxFPs[i] = sbxID.Fingerprint()
		approvePeer(t, orchDir, sbxFPs[i], identity.RoleSandboxHost)
		approvePeer(t, sbxDirs[i], orchID.Fingerprint(), identity.RoleOrchestrator)
	}
	approvePeer(t, orchDir, cliID.Fingerprint(), clientRole)
	approvePeer(t, cliDir, orchID.Fingerprint(), identity.RoleOrchestrator)

	addr := freeLoopbackAddr(t)
	url := "https://" + addr

	orchOut := newOutputCapture(t, "daft-orchestrator")
	orchestratorBin := bins.orchestrator
	orchReady := daemonReadyTimeout
	if o.bundledOrchestrator != "" {
		orchestratorBin = o.bundledOrchestrator
		// The bundled binary seeds its embedded build image (hundreds of
		// MiB) into the registry before it listens, so give startup room.
		orchReady = 60 * time.Second
	}
	startDaemon(t, orchOut, nil, nil, orchestratorBin,
		"serve", "--addr", addr, "--config-dir", orchDir)
	waitForTCP(t, addr, orchReady)

	// When the egress proxy is requested, enable the in-stack proxy via
	// sandboxd's env: sandboxd builds the proxy (and its CA) in-process
	// and serves it on every VM's netstack gateway, so every
	// image-backed dispatch routes guest TLS through it. Upstream
	// verification is skipped because tests use self-signed upstreams.
	// Caching mode is resolved by resolveEgressCache. A cluster that opted
	// into the recorded cache (WithEgressReplayCache) consults the artifact
	// (envEgressCacheArtifact, exported by `make integration`) and replays
	// it OFFLINE — deterministic, no upstream hit, a miss 504s and fails
	// loudly. Every other WithEgressProxy cluster ignores the env and
	// records live into a temp dir (online), so a real bin/egress-cache —
	// which only holds the bun test's URLs — can't force unrelated egress
	// tests offline into a guaranteed miss. See PLAN.md K2.
	// The proxy is on by default in sandboxd, so tests that don't ask
	// for it must explicitly opt out — otherwise every cluster would
	// run default-deny and break workloads that egress directly.
	sbxEnv := []string{"DAFT_EGRESS_PROXY=0"}
	var proxyURL string
	if o.egressProxy {
		proxyURL = "http://" + net.JoinHostPort(
			sandboxnet.DefaultGatewayIP, strconv.Itoa(int(sandboxnet.DefaultProxyPort)),
		)
		artifact, record := "", ""
		if o.egressReplayCache {
			artifact, record = os.Getenv(envEgressCacheArtifact), os.Getenv(envEgressCacheRecord)
		}
		cacheDir, offline := resolveEgressCache(artifact, record, t.TempDir)
		sbxEnv = []string{
			"DAFT_EGRESS_PROXY=1",
			"DAFT_EGRESS_PROXY_INSECURE_UPSTREAM=1",
			"DAFT_EGRESS_PROXY_CACHE_DIR=" + cacheDir,
			"DAFT_EGRESS_PROXY_OFFLINE=" + offline,
		}
	}

	// Opt the node into in-sandbox devcontainer builds when the test
	// supplied a builder ref (#101 slice 3c).
	if o.buildImageRef != "" {
		sbxEnv = append(sbxEnv, "DAFT_BUILD_IMAGE_REF="+o.buildImageRef)
	}

	var sbxScrub []string
	if o.bundledSandboxd != "" {
		// A bundled binary no longer requires the artefact env vars at
		// startup (ConfigFromEnv(false)) — the //go:embed'd artefacts are
		// the source of truth. Drop the real (Makefile-injected) paths
		// entirely: the VM can boot only if extraction won. This mirrors
		// the production `daft cluster up` scenario where no artefact env
		// vars are exported at all.
		sbxScrub = artefactEnvKeys
	}
	observerDirs := make([]string, o.sandboxdCount)
	sandboxdOuts := make([]*outputCapture, o.sandboxdCount)
	sandboxdCmds := make([]*exec.Cmd, o.sandboxdCount)
	for i := range o.sandboxdCount {
		observerDirs[i] = newObserverStateDir(t, filepath.Join(base, fmt.Sprintf("node-%d", i)))
		sandboxdOuts[i] = newOutputCapture(t, fmt.Sprintf("daft-sandboxd-%d", i))
		nodeEnv := append([]string(nil), sbxEnv...)
		nodeEnv = append(nodeEnv, sandboxdcli.EnvStateDir+"="+observerDirs[i])
		sandboxdCmds[i] = startDaemon(t, sandboxdOuts[i], nodeEnv, sbxScrub, sandboxdBin,
			"serve", "--orchestrator", url, "--config-dir", sbxDirs[i])
		waitForNode(t, bins.daft, cliDir, url, sbxFPs[i], daemonReadyTimeout)
	}

	return &Cluster{
		DaftBin:           bins.daft,
		MCPBin:            bins.mcp,
		OrchestratorURL:   url,
		ClientConfigDir:   cliDir,
		SandboxdConfigDir: sbxDirs[0],
		MitmProxyBin:      bins.mitmProxy,
		EgressProxyURL:    proxyURL,
		OrchestratorFP:    orchID.Fingerprint(),
		SandboxdFP:        sbxFPs[0],
		SandboxdFPs:       sbxFPs,
		ClientFP:          cliID.Fingerprint(),
		ObserverDir:       observerDirs[0],
		sandboxdOut:       sandboxdOuts[0],
		sandboxdCmd:       sandboxdCmds[0],
	}
}

func waitForNode(t *testing.T, daftBin, configDir, orchestratorURL string, fp identity.Fingerprint, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOutput string
	var lastErr error
	//daft:lint:disable=forloopbound reason="bounded by deadline via the if-fatal below"
	for {
		cmd := exec.Command( //nolint:gosec // test-controlled binary and arguments.
			daftBin, "--config-dir", configDir,
			"node", "list", "--full", "--orchestrator", orchestratorURL,
		)
		out, err := cmd.CombinedOutput()
		lastOutput, lastErr = string(out), err
		if err == nil && strings.Contains(lastOutput, fp.String()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("wait for sandboxd %s: %v after %s\n%s", fp, lastErr, timeout, lastOutput)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ClusterOption tunes optional cluster topology.
type ClusterOption func(*clusterOpts)

type clusterOpts struct {
	egressProxy   bool
	clientRole    identity.Role
	sandboxdCount int
	// egressReplayCache opts THIS cluster into the recorded egress-replay
	// cache env (DAFT_EGRESS_PROXY_CACHE / _RECORD). Without it a
	// WithEgressProxy cluster always records live into a throwaway dir and
	// never goes offline, so a real bin/egress-cache — which only holds the
	// bun test's URLs — can't force unrelated egress tests into a 504 miss.
	egressReplayCache bool
	// bundledSandboxd, when set, runs that daft-sandboxd binary instead
	// of the locally go-built one AND scrubs the artefact env vars from
	// its environment, forcing it onto the //go:embed-bundled artefacts.
	bundledSandboxd string
	// bundledOrchestrator, when set, runs that daft-orchestrator binary
	// instead of the locally go-built one. The Nix-built binary embeds the
	// build image and auto-seeds it into its registry at startup, so a
	// build-based devcontainer needs no DAFT_BUILD_IMAGE_REF.
	bundledOrchestrator string
	// buildImageRef, when set, is exported as DAFT_BUILD_IMAGE_REF so the
	// node can build a build-based devcontainer in an ephemeral sandbox.
	buildImageRef string
}

// WithSandboxdCount starts count independently identified sandbox Nodes.
func WithSandboxdCount(count int) ClusterOption {
	return func(o *clusterOpts) { o.sandboxdCount = count }
}

// WithOperatorClient gives the cluster's trusted client identity the operator
// role required by privileged fleet and AgentRun RPCs. Workspace ownership is
// still bound to the same certificate fingerprint, so one daft-mcp process can
// compose project and operator operations without a self-asserted role.
func WithOperatorClient() ClusterOption {
	return func(o *clusterOpts) { o.clientRole = identity.RoleOperator }
}

// WithEgressProxy makes the cluster run the in-stack egress proxy:
// sandboxd builds the proxy (and its CA) in-process and serves it on
// every VM's netstack gateway, so guest TLS egress routes through (and
// trusts) it via the executor's Phase-J injection. No separate process
// or VIP — just the DAFT_EGRESS_PROXY* env that NewCluster sets.
func WithEgressProxy() ClusterOption {
	return func(o *clusterOpts) { o.egressProxy = true }
}

// WithEgressReplayCache opts the cluster into the recorded egress-replay
// cache: only under it does NewCluster read DAFT_EGRESS_PROXY_CACHE
// (replay offline) / DAFT_EGRESS_PROXY_CACHE_RECORD (record). Set it ONLY
// on a test whose egress URLs are actually in the recorded artifact —
// under offline replay a cache miss 504s. The recorder
// (`make update-egress-cache`) scopes to the bun test, so the bun test is
// the sole consumer; every other WithEgressProxy cluster stays online and
// keeps working even when a real bin/egress-cache is present on disk.
// Has no effect without WithEgressProxy (the cache feeds the proxy).
func WithEgressReplayCache() ClusterOption {
	return func(o *clusterOpts) { o.egressReplayCache = true }
}

// envEgressCacheArtifact names the env var (exported by `make
// integration`) that points at a recorded, unpacked egress-proxy cache
// dir — the pinned OCI artifact realized to bin/egress-cache. When set,
// the egress proxy replays it offline instead of recording live.
const envEgressCacheArtifact = "DAFT_EGRESS_PROXY_CACHE"

// envEgressCacheRecord names the env var (set by `make
// update-egress-cache`) that points at a persistent dir to RECORD the
// egress cache into: the proxy runs online and writes every fetch there,
// producing the artifact that envEgressCacheArtifact later replays.
const envEgressCacheRecord = "DAFT_EGRESS_PROXY_CACHE_RECORD"

// resolveEgressCache decides the proxy's cache dir + offline flag for a
// WithEgressProxy cluster, across three modes:
//   - replay  (artifactDir set): a recorded cache shipped with the repo,
//     replayed read-only and OFFLINE — deterministic, no upstream hit, a
//     miss 504s loudly. Wins if both are set.
//   - record  (recordDir set): a persistent dir the proxy populates
//     online, producing the artifact. Used only by update-egress-cache.
//   - default (neither): a fresh writable temp dir that records live and
//     is wiped on cleanup — today's behaviour for proxy tests at large.
//
// offline is returned as the "1"/"0" string DAFT_EGRESS_PROXY_OFFLINE
// expects.
func resolveEgressCache(artifactDir, recordDir string, mkTemp func() string) (dir, offline string) {
	switch {
	case artifactDir != "":
		return artifactDir, "1"
	case recordDir != "":
		return recordDir, "0"
	default:
		return mkTemp(), "0"
	}
}

// WithBundledSandboxd runs the real, //go:embed-bundled daft-sandboxd
// at binPath (the `nix build .#daft-sandboxd` output) instead of the
// locally go-built binary, with the artefact env vars scrubbed from its
// environment entirely. The bundled binary requires no artefact env
// vars at startup and Extract()s the embedded artefacts, so a VM boots
// only if extraction won — if the embedding were broken, dispatch would
// fail with ErrNotBundled. This turns a normal cluster test into a proof
// of the production embed path (and of the no-env-var startup that
// `daft cluster up` relies on).
func WithBundledSandboxd(binPath string) ClusterOption {
	return func(o *clusterOpts) { o.bundledSandboxd = binPath }
}

// WithBundledOrchestrator runs the real, //go:embed-bundled
// daft-orchestrator at binPath (the `nix build .#daft-orchestrator`
// output) instead of the locally go-built binary. The bundled binary
// embeds the buildah build image and auto-seeds it into its registry at
// startup under ociregistry.BuiltinBuildRef(), so a build-based
// devcontainer boots with no DAFT_BUILD_IMAGE_REF set — the proof that
// the auto-seeded builder path works end to end.
func WithBundledOrchestrator(binPath string) ClusterOption {
	return func(o *clusterOpts) { o.bundledOrchestrator = binPath }
}

// WithBuildImage configures the node with the static image-builder ref so a
// build-based devcontainer is built in an ephemeral sandbox before its
// workload boots (#101 slice 3c). ref must be pullable by the node (a
// loopback test-registry ref is fine — the node pulls host-side). The
// built result is pushed back over the cluster's own hosted-registry mTLS.
func WithBuildImage(ref string) ClusterOption {
	return func(o *clusterOpts) { o.buildImageRef = ref }
}

// artefactEnvKeys are the sandbox-artefact env vars dropped from a
// bundled sandboxd's inherited environment, so it boots purely from its
// //go:embed'd artefacts — the production `daft cluster up` scenario.
//
//nolint:gochecknoglobals // immutable lookup list shared by the harness.
var artefactEnvKeys = []string{
	"DAFT_INITRAMFS", "DAFT_KERNEL", "DAFT_VIRTIOFSD", "DAFT_CLOUD_HYPERVISOR",
}

// binaries holds absolute paths to the freshly-built daft, daft-
// orchestrator, and daft-sandboxd binaries.
type binaries struct {
	daft         string
	mcp          string
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
			{"daft-mcp", "./cmd/daft-mcp"},
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
			case "daft-mcp":
				built.mcp = outPath
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

// scrubEnv returns env with any "KEY=value" entry whose KEY is in keys
// removed. Used to deny the bundled sandboxd its artefact env fallback
// so the test exercises the embedded path or fails.
func scrubEnv(env, keys []string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		drop[k] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := drop[name]; ok {
			continue
		}
		out = append(out, kv)
	}
	return out
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

func approvePeer(t *testing.T, dir string, fp identity.Fingerprint, role identity.Role) {
	t.Helper()
	store, err := identity.OpenPeerStore(filepath.Join(dir, peersFile))
	if err != nil {
		t.Fatalf("OpenPeerStore(%s): %v", dir, err)
	}
	if err := store.Add(identity.Peer{Fingerprint: fp, Role: role}); err != nil {
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
func startDaemon(t *testing.T, out *outputCapture, extraEnv, scrub []string, prog string, args ...string) *exec.Cmd {
	t.Helper()
	//nolint:gosec // G204: prog + args are caller-controlled test-only paths.
	cmd := exec.Command(prog, args...)
	if len(extraEnv) > 0 || len(scrub) > 0 {
		base := os.Environ()
		if len(scrub) > 0 {
			base = scrubEnv(base, scrub)
		}
		cmd.Env = append(base, extraEnv...)
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
