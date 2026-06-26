// Package localcluster brings up a co-located daft control plane —
// the orchestrator plus one sandbox-host — on a single machine, with
// the three mTLS identities (orchestrator, sandbox-host, client)
// minted and cross-approved automatically in one state directory.
//
// It is the shared core behind `daft cluster up/down` and the dev /
// boot scripts, which previously hand-rolled the same identity dance
// and daemon lifecycle in shell. The control plane runs as two child
// processes rather than one merged server: daft-sandboxd does cgroup +
// userns re-exec at its entry point, so it must own its process. The
// sandbox-host only ever dials out (it opens no listener), so it talks
// to a loopback orchestrator with no extra wiring.
//
// Trust is fingerprint-based TOFU: each identity is a self-signed cert,
// and approval is membership in the peer's peers.json. WireIdentities
// writes those files directly; the handshake checks fingerprint
// membership only (roles are descriptive). Three distinct config dirs
// are required because mutual TLS needs three distinct certs.
package localcluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	// Subdirectories of the state dir, one per identity. Distinct dirs
	// because mutual TLS needs three distinct certs.
	orchSubdir   = "orch"
	sbxSubdir    = "sbx"
	clientSubdir = "client"
	logsSubdir   = "logs"

	// peersFilename matches the name every daft binary uses for its
	// TOFU peer store (internal/cli/config.go, internal/sandboxdcli,
	// internal/orchestratorcli all agree on "peers.json").
	peersFilename = "peers.json"
	// pidFilename records the spawned daemons' process-group ids so
	// Down can tear them down; infoFilename is the machine-readable
	// handle (URL, client dir, fingerprints) for later commands.
	pidFilename  = "cluster.pid"
	infoFilename = "cluster.json"

	// DefaultAddr is the orchestrator's loopback listen address. The
	// port must be concrete (not :0) — the child picks the socket, so a
	// random port would be unknowable to this supervisor. 14443 keeps the
	// HTTPS-mnemonic lineage while dodging the crowded ones (8443 is
	// alt-HTTPS / Tomcat / k8s; 18443 is Bitcoin regtest), and sits below
	// the Linux ephemeral range (32768+) so it won't clash with an
	// outbound socket. Override with `--addr` if it's still taken.
	DefaultAddr = "127.0.0.1:14443"

	// defaultReadyTimeout bounds how long Up waits for the orchestrator
	// to accept and the sandbox-host to register.
	defaultReadyTimeout = 60 * time.Second

	orchestratorBinName = "daft-orchestrator"
	sandboxdBinName     = "daft-sandboxd"
)

// Info is the result of wiring the three identities: where they live
// and their fingerprints (hex strings).
type Info struct {
	OrchDir, SandboxDir, ClientDir      string
	OrchestratorFP, SandboxFP, ClientFP string
}

// Config parameterises Up.
type Config struct {
	// StateDir roots the cluster: identities, logs, pidfile, cluster.json.
	StateDir string
	// Addr is the orchestrator's listen address (default DefaultAddr).
	Addr string
	// OrchestratorBin / SandboxdBin override the daemon binaries.
	// Empty means "sibling of the running executable, else PATH".
	OrchestratorBin, SandboxdBin string
	// EgressProxy leaves the sandbox-host's egress proxy on. Off by
	// default — single-box dev rarely wants workload egress filtering.
	EgressProxy bool
	// ReadyTimeout bounds startup readiness (default defaultReadyTimeout).
	ReadyTimeout time.Duration
	// Progress is where human-readable startup progress is written
	// (nil discards). Daemon stdout/stderr always go to log files.
	Progress io.Writer
	// Env is the base environment for the daemon children (default
	// os.Environ()). The sandbox-host needs the DAFT_* VM-artefact
	// paths present here to boot VMs.
	Env []string
}

// Cluster is a running (detached) control plane: the handle a caller
// uses to reach it and, via Down, to tear it down.
type Cluster struct {
	URL             string
	ClientConfigDir string
	OrchestratorFP  string
	SandboxFP       string
	Addr            string
	StateDir        string
	PIDs            []int
}

// clusterFile is the JSON persisted at <state>/cluster.json.
type clusterFile struct {
	URL             string `json:"url"`
	ClientConfigDir string `json:"client_config_dir"`
	OrchestratorFP  string `json:"orchestrator_fp"`
	SandboxFP       string `json:"sandbox_fp"`
	Addr            string `json:"addr"`
	PIDs            []int  `json:"pids"`
	StateDir        string `json:"state_dir"`
}

// Seams: readiness checks are package vars so Up is unit-testable with
// fake daemon binaries. Production points them at the real probes.
//
//nolint:gochecknoglobals // test seams, swapped in unit tests.
var (
	waitOrchestratorReady = waitOrchestratorHealthy
	waitSandboxRegistered = waitNodeRegistered
)

// WireIdentities mints (or loads, if already present) the orchestrator,
// sandbox-host, and client identities under stateDir and cross-approves
// them: the orchestrator trusts the sandbox-host and the client; the
// sandbox-host and the client each trust the orchestrator. Idempotent —
// re-running reuses existing identities and re-asserts the approvals.
func WireIdentities(stateDir string) (Info, error) {
	orchDir := filepath.Join(stateDir, orchSubdir)
	sbxDir := filepath.Join(stateDir, sbxSubdir)
	cliDir := filepath.Join(stateDir, clientSubdir)

	orch, err := identity.LoadOrGenerate(orchDir, identity.RoleOrchestrator)
	if err != nil {
		return Info{}, fmt.Errorf("localcluster: orchestrator identity: %w", err)
	}
	sbx, err := identity.LoadOrGenerate(sbxDir, identity.RoleSandboxHost)
	if err != nil {
		return Info{}, fmt.Errorf("localcluster: sandbox-host identity: %w", err)
	}
	cli, err := identity.LoadOrGenerate(cliDir, identity.RoleClient)
	if err != nil {
		return Info{}, fmt.Errorf("localcluster: client identity: %w", err)
	}

	approvals := []struct {
		dir  string
		peer identity.Peer
	}{
		{orchDir, identity.Peer{Fingerprint: sbx.Fingerprint(), Role: identity.RoleSandboxHost, Label: "sbx"}},
		{orchDir, identity.Peer{Fingerprint: cli.Fingerprint(), Role: identity.RoleClient, Label: "client"}},
		{sbxDir, identity.Peer{Fingerprint: orch.Fingerprint(), Role: identity.RoleOrchestrator, Label: "orch"}},
		{cliDir, identity.Peer{Fingerprint: orch.Fingerprint(), Role: identity.RoleOrchestrator, Label: "orch"}},
	}
	for _, a := range approvals {
		store, err := identity.OpenPeerStore(filepath.Join(a.dir, peersFilename))
		if err != nil {
			return Info{}, fmt.Errorf("localcluster: open peer store %s: %w", a.dir, err)
		}
		if err := store.Add(a.peer); err != nil {
			return Info{}, fmt.Errorf("localcluster: approve in %s: %w", a.dir, err)
		}
	}

	return Info{
		OrchDir: orchDir, SandboxDir: sbxDir, ClientDir: cliDir,
		OrchestratorFP: orch.Fingerprint().String(),
		SandboxFP:      sbx.Fingerprint().String(),
		ClientFP:       cli.Fingerprint().String(),
	}, nil
}

// Up wires identities, tears down any stale cluster in the same state
// dir, spawns the orchestrator and sandbox-host as detached children,
// waits for both to be ready, and records the pidfile + cluster.json.
// On any failure after a child is started, the started children are
// killed before returning.
func Up(ctx context.Context, cfg Config) (*Cluster, error) {
	r, err := cfg.resolve()
	if err != nil {
		return nil, err
	}

	// A stale cluster on the same state dir would fight us for the
	// address; clear it first (no-op when nothing is recorded).
	_ = Down(cfg.StateDir, false)

	info, err := WireIdentities(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	logsDir := filepath.Join(cfg.StateDir, logsSubdir)
	if err := os.MkdirAll(logsDir, 0o750); err != nil {
		return nil, fmt.Errorf("localcluster: mkdir logs: %w", err)
	}
	url := URLForAddr(r.addr)

	// Pre-flight: fail fast (and clearly) if the orchestrator's address is
	// already taken by a foreign process, before spawning any daemon.
	// Without this the orchestrator child would spawn, fail to bind, and
	// die — while the sandbox-host dials the foreign listener and reports a
	// baffling TLS error. Runs after WireIdentities (which only mints local
	// files and is what proves a defaulted state dir) so the gate is purely
	// about the port, immediately before the first spawn. (Down above
	// already cleared any stale cluster, so a busy port here is genuinely
	// someone else's.)
	if err := ensureAddrFree(r.addr); err != nil {
		return nil, err
	}

	var started []int
	bail := func(e error) (*Cluster, error) {
		for _, pid := range started {
			killGroup(pid)
		}
		return nil, e
	}

	if err := launchDaemon(r.progress, &started, "orchestrator", r.orchBin,
		[]string{"serve", "--addr", r.addr, "--config-dir", info.OrchDir},
		r.baseEnv, filepath.Join(logsDir, "orchestrator.log"),
		func() error { return waitOrchestratorReady(ctx, url, info.ClientDir, r.timeout) }); err != nil {
		return bail(err)
	}

	sbxEnv := r.baseEnv
	if !cfg.EgressProxy {
		sbxEnv = append(append([]string{}, r.baseEnv...), "DAFT_EGRESS_PROXY=0")
	}
	if err := launchDaemon(r.progress, &started, "sandbox-host", r.sbxBin,
		[]string{"serve", "--orchestrator", url, "--config-dir", info.SandboxDir},
		sbxEnv, filepath.Join(logsDir, "sandboxd.log"),
		func() error { return waitSandboxRegistered(ctx, url, info.ClientDir, r.timeout) }); err != nil {
		return bail(err)
	}

	c := &Cluster{
		URL:             url,
		ClientConfigDir: info.ClientDir,
		OrchestratorFP:  info.OrchestratorFP,
		SandboxFP:       info.SandboxFP,
		Addr:            r.addr,
		StateDir:        cfg.StateDir,
		PIDs:            started,
	}
	if err := writeInfo(cfg.StateDir, c); err != nil {
		return bail(err)
	}
	_, _ = fmt.Fprintf(r.progress, "localcluster: ready at %s\n", url)
	return c, nil
}

// resolvedConfig is Config with defaults applied and binaries located.
type resolvedConfig struct {
	addr     string
	timeout  time.Duration
	progress io.Writer
	baseEnv  []string
	orchBin  string
	sbxBin   string
}

// resolve validates cfg and fills in defaults: the listen address,
// readiness timeout, progress writer, child env, and the two daemon
// binary paths.
func (cfg Config) resolve() (resolvedConfig, error) {
	if cfg.StateDir == "" {
		return resolvedConfig{}, errors.New("localcluster: empty state dir")
	}
	r := resolvedConfig{
		addr:     cfg.Addr,
		timeout:  cfg.ReadyTimeout,
		progress: cfg.Progress,
		baseEnv:  cfg.Env,
	}
	if r.addr == "" {
		r.addr = DefaultAddr
	}
	if r.timeout <= 0 {
		r.timeout = defaultReadyTimeout
	}
	if r.progress == nil {
		r.progress = io.Discard
	}
	if r.baseEnv == nil {
		r.baseEnv = os.Environ()
	}
	r.orchBin = resolveBin(cfg.OrchestratorBin, orchestratorBinName)
	r.sbxBin = resolveBin(cfg.SandboxdBin, sandboxdBinName)
	return r, nil
}

// launchDaemon spawns one daemon (recording its pid in *started so a
// failed Up can tear down what it already started) and runs its
// readiness check. Errors are labelled with the daemon name.
func launchDaemon(
	progress io.Writer, started *[]int, label, bin string,
	args, env []string, logPath string, ready func() error,
) error {
	_, _ = fmt.Fprintf(progress, "localcluster: starting %s\n", label)
	pid, err := spawnDaemon(bin, args, env, logPath)
	if err != nil {
		return fmt.Errorf("localcluster: start %s: %w", label, err)
	}
	*started = append(*started, pid)

	// Run the readiness probe in the background and poll the child's
	// liveness alongside it. A daemon that dies during startup (most
	// often: its address is already in use) must fail fast with a clear
	// error rather than blocking until the readiness timeout — or, worse,
	// being mistaken for ready because a *foreign* listener already holds
	// the port and answers the TCP probe.
	exited := fmt.Errorf("localcluster: %s exited during startup (see %s)", label, logPath)
	done := make(chan error, 1)
	go func() { done <- ready() }()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	//daft:lint:disable=forloopbound reason="exits on the readiness result (done) or when the spawned child exits"
	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("localcluster: %s not ready: %w", label, err)
			}
			if childExited(pid) {
				return exited // readiness "passed" but our child is gone
			}
			return nil
		case <-ticker.C:
			if childExited(pid) {
				return exited
			}
		}
	}
}

// childExited reports whether our child pid has terminated, reaping it
// (WNOHANG) if so. A still-running child is left untouched and returns
// false — so this is safe to poll against daemons we intend to keep
// running. We must reap rather than probe with Kill(pid, 0): we are the
// daemon's parent, so a dead-but-unreaped child is a zombie that Kill
// still reports as alive.
func childExited(pid int) bool {
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return wpid == pid || errors.Is(err, syscall.ECHILD)
}

// Down stops the cluster recorded in stateDir: it SIGTERMs each
// daemon's process group (SIGKILL after a grace period) and removes the
// pidfile + cluster.json. A missing pidfile is not an error. When purge
// is set the whole state dir (identities, logs) is removed too.
func Down(stateDir string, purge bool) error {
	pids, err := readPIDs(filepath.Join(stateDir, pidFilename))
	if err != nil {
		return err
	}
	for _, pid := range pids {
		killGroup(pid)
	}
	_ = os.Remove(filepath.Join(stateDir, pidFilename))
	_ = os.Remove(filepath.Join(stateDir, infoFilename))
	if purge {
		return os.RemoveAll(stateDir)
	}
	return nil
}

// DefaultStateDir is the well-known location a single-box dev cluster
// lives in when no --state is given: $XDG_STATE_HOME/daft/cluster, else
// ~/.local/state/daft/cluster. `cluster up`/`down` default to it and the
// daft client auto-discovers a running cluster there (see the cli
// package), so the common case needs neither a --state flag nor any
// DAFT_* env var to connect.
func DefaultStateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("localcluster: locate home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "daft", "cluster"), nil
}

// Load reads the persisted handle at <stateDir>/cluster.json. It returns
// (nil, nil) when no cluster.json exists, so a caller probing for a
// cluster can treat "absent" as "none up" without inspecting the error.
func Load(stateDir string) (*Cluster, error) {
	// #nosec G304 — caller-supplied state dir.
	data, err := os.ReadFile(filepath.Join(stateDir, infoFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // absent handle is the "no cluster up" signal; callers branch on c==nil.
	}
	if err != nil {
		return nil, fmt.Errorf("localcluster: read cluster.json: %w", err)
	}
	var f clusterFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("localcluster: parse cluster.json: %w", err)
	}
	return &Cluster{
		URL: f.URL, ClientConfigDir: f.ClientConfigDir,
		OrchestratorFP: f.OrchestratorFP, SandboxFP: f.SandboxFP,
		Addr: f.Addr, StateDir: f.StateDir, PIDs: f.PIDs,
	}, nil
}

// Alive reports whether at least one recorded daemon process group is
// still running. A handle left behind by a reboot or crash (cluster.json
// present but the daemons gone) reads as not-alive, so discovery skips it
// rather than pointing the client at a dead orchestrator. Probed with
// signal 0 because the inspecting process (a later daft invocation) is not
// the daemons' parent and so cannot Wait4 them.
func (c *Cluster) Alive() bool {
	for _, pid := range c.PIDs {
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			return true
		}
	}
	return false
}

// URLForAddr turns a host:port listen address into the https URL a
// client dials, rewriting a 0.0.0.0 / :: wildcard host to loopback.
func URLForAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "https://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port)
}

// resolveBin returns override if set, else the named binary next to the
// running executable, else the bare name (PATH lookup at exec time).
func resolveBin(override, name string) string {
	if override != "" {
		return override
	}
	self, err := os.Executable()
	if err != nil {
		return name
	}
	sibling := filepath.Join(filepath.Dir(self), name)
	if _, err := os.Stat(sibling); err == nil {
		return sibling
	}
	return name
}

// spawnDaemon starts bin detached in its own process group with output
// redirected to logPath, and returns its pid (also the pgid). It does
// not Wait — the daemon outlives this process; Down reaps it later.
func spawnDaemon(bin string, args, env []string, logPath string) (int, error) {
	// #nosec G304 — caller-controlled daemon log path (dev supervisor).
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer func() { _ = logf.Close() }()

	cmd := exec.Command(bin, args...) //nolint:gosec // G204: dev supervisor, caller-controlled binary.
	cmd.Env = env
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil // /dev/null
	// New process group so Down can signal the whole tree via -pgid,
	// and so the daemon isn't in our foreground group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// ensureAddrFree fails fast when addr is already occupied — almost
// always a foreign service squatting on the orchestrator's port. The
// error is deliberately actionable: a bare bind failure deep in the
// orchestrator child (whose stderr lands in a log file the user has to
// go hunting for) is replaced by an immediate, legible reason at the
// supervisor level, before any daemon is spawned.
//
// There is a TOCTOU window between this probe and the orchestrator's own
// bind, but it is negligible on a loopback dev box, and if something does
// grab the port in between the orchestrator's bind fails and the
// liveness poll in launchDaemon catches the dead child.
func ensureAddrFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("localcluster: cannot start orchestrator: address %s is already "+
			"in use by another process — free it or pass a different --addr: %w", addr, err)
	}
	return ln.Close()
}

// dialNodeControl builds a NodeControlService client over mTLS using the
// client identity in clientDir. Shared by the two readiness probes — both
// need to prove they are talking to our orchestrator (whose cert the
// client trusts), not a foreign listener on the same port.
func dialNodeControl(url, clientDir string) (nodectrlv1connect.NodeControlServiceClient, error) {
	id, err := identity.Load(clientDir)
	if err != nil {
		return nil, fmt.Errorf("load client identity: %w", err)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(clientDir, peersFilename))
	if err != nil {
		return nil, fmt.Errorf("open client peer store: %w", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return nil, err
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	return nodectrlv1connect.NewNodeControlServiceClient(httpc, url), nil
}

// waitOrchestratorHealthy blocks until the orchestrator answers a
// NodeControl RPC over mTLS — proving the listener on url is OUR
// orchestrator (it presents a cert the client identity trusts and
// accepts the client's), not a foreign service that merely accepts TCP
// on the same port. A bare TCP-accept probe cannot tell the two apart
// and would race the liveness poll into a false "ready"; this cannot.
// Returns on the first successful ListNodes (any node count), else fails
// on timeout.
func waitOrchestratorHealthy(ctx context.Context, url, clientDir string, timeout time.Duration) error {
	client, err := dialNodeControl(url, clientDir)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	//daft:lint:disable=forloopbound reason="bounded by the timeout deadline checked each iteration"
	for {
		if _, err := client.ListNodes(ctx, connect.NewRequest(&nodectrlv1.ListNodesRequest{})); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("orchestrator not healthy on %s within %s: %w", url, timeout, lastErr)
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// waitNodeRegistered blocks until the orchestrator reports at least one
// registered sandbox-host (its ListNodes RPC returns a non-empty set)
// or timeout elapses. It dials with the client identity in clientDir.
func waitNodeRegistered(ctx context.Context, url, clientDir string, timeout time.Duration) error {
	client, err := dialNodeControl(url, clientDir)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	//daft:lint:disable=forloopbound reason="bounded by the timeout deadline checked each iteration"
	for {
		resp, err := client.ListNodes(ctx, connect.NewRequest(&nodectrlv1.ListNodesRequest{}))
		if err == nil && len(resp.Msg.GetNodes()) > 0 {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("no node registered")
			}
			return fmt.Errorf("sandbox-host not registered within %s: %w", timeout, lastErr)
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

// killGroup SIGTERMs the process group led by pid, then SIGKILLs it if
// it is still alive after a short grace period. A pid of 0 / already-
// gone group is ignored.
func killGroup(pid int) {
	if pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		reapIfChild(pid)
		if syscall.Kill(-pid, 0) != nil {
			return // group gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	reapIfChild(pid)
}

// reapIfChild non-blockingly reaps pid if it is our child — so a
// killed-but-unreaped child (a zombie) doesn't keep its process group
// alive and look like it's still running. When pid is not our child
// (the usual case: `cluster down` is a different process from the
// `cluster up` that spawned the daemons), Wait4 returns ECHILD and this
// is a no-op; init reaps the orphan and the kill(-pid, 0) probe sees it
// go.
func reapIfChild(pid int) {
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

// writeInfo persists the pidfile and cluster.json for Down + later
// commands to read.
func writeInfo(stateDir string, c *Cluster) error {
	var buf []byte
	for _, pid := range c.PIDs {
		buf = fmt.Appendf(buf, "%d\n", pid)
	}
	if err := os.WriteFile(filepath.Join(stateDir, pidFilename), buf, 0o644); err != nil { //nolint:gosec // G306: not a secret.
		return fmt.Errorf("localcluster: write pidfile: %w", err)
	}
	data, err := json.MarshalIndent(clusterFile{
		URL: c.URL, ClientConfigDir: c.ClientConfigDir,
		OrchestratorFP: c.OrchestratorFP, SandboxFP: c.SandboxFP,
		Addr: c.Addr, PIDs: c.PIDs, StateDir: c.StateDir,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("localcluster: marshal cluster.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, infoFilename), data, 0o644); err != nil { //nolint:gosec // G306: not a secret.
		return fmt.Errorf("localcluster: write cluster.json: %w", err)
	}
	return nil
}

// readPIDs parses the newline-separated pidfile. A missing file yields
// no pids and no error (nothing to tear down).
func readPIDs(path string) ([]int, error) {
	// #nosec G304 — caller-supplied state dir.
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localcluster: read pidfile: %w", err)
	}
	var pids []int
	for _, line := range splitLines(string(data)) {
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err != nil {
			return nil, fmt.Errorf("localcluster: bad pidfile line %q: %w", line, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// splitLines splits on '\n' without keeping a trailing empty field,
// avoiding a strings import for one call.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
