package localcluster

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
	"github.com/localai-org/darkaifactory/internal/orchestrator"
)

// loopbackAny is a loopback address with an OS-assigned port.
const loopbackAny = "127.0.0.1:0"

func TestWireIdentitiesCreatesAndCrossApproves(t *testing.T) {
	state := t.TempDir()
	info, err := WireIdentities(state)
	if err != nil {
		t.Fatalf("WireIdentities: %v", err)
	}

	// Each identity dir has a cert.
	for _, dir := range []string{info.OrchDir, info.SandboxDir, info.ClientDir} {
		if _, err := identity.Load(dir); err != nil {
			t.Fatalf("identity.Load(%s): %v", dir, err)
		}
	}

	// orch trusts {sbx, client}; sbx trusts {orch}; client trusts {orch}.
	orchPeers := mustPeers(t, info.OrchDir)
	if !approved(t, orchPeers, info.SandboxFP) || !approved(t, orchPeers, info.ClientFP) {
		t.Errorf("orchestrator peers missing sbx/client: %v", orchPeers.List())
	}
	sbxPeers := mustPeers(t, info.SandboxDir)
	if !approved(t, sbxPeers, info.OrchestratorFP) {
		t.Errorf("sandbox-host peers missing orch")
	}
	cliPeers := mustPeers(t, info.ClientDir)
	if !approved(t, cliPeers, info.OrchestratorFP) {
		t.Errorf("client peers missing orch")
	}
}

func TestWireIdentitiesIdempotent(t *testing.T) {
	state := t.TempDir()
	first, err := WireIdentities(state)
	if err != nil {
		t.Fatalf("WireIdentities #1: %v", err)
	}
	second, err := WireIdentities(state)
	if err != nil {
		t.Fatalf("WireIdentities #2: %v", err)
	}
	if first.OrchestratorFP != second.OrchestratorFP ||
		first.SandboxFP != second.SandboxFP ||
		first.ClientFP != second.ClientFP {
		t.Errorf("fingerprints changed across runs: %+v vs %+v", first, second)
	}
}

func TestURLForAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8443": testClusterURL,
		"0.0.0.0:8443":   testClusterURL,
		"[::]:9000":      "https://127.0.0.1:9000",
		"noport":         "https://noport",
	}
	for in, want := range cases {
		if got := URLForAddr(in); got != want {
			t.Errorf("URLForAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveBin(t *testing.T) {
	if got := resolveBin("/explicit/path", "daft-x"); got != "/explicit/path" {
		t.Errorf("override = %q, want /explicit/path", got)
	}
	// No sibling next to the test binary, not in PATH → bare name (PATH
	// lookup deferred to exec).
	if got := resolveBin("", "daft-definitely-not-real-xyz"); got != "daft-definitely-not-real-xyz" {
		t.Errorf("fallback = %q, want bare name", got)
	}
}

func TestEnsureAddrFree(t *testing.T) {
	// A free port passes.
	if err := ensureAddrFree(loopbackAny); err != nil {
		t.Errorf("ensureAddrFree on a free port: %v", err)
	}
	// An occupied port fails fast with an actionable error.
	ln, err := net.Listen("tcp", loopbackAny)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	err = ensureAddrFree(ln.Addr().String())
	if err == nil {
		t.Fatal("ensureAddrFree on an occupied port: want error")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("error = %v, want 'already in use'", err)
	}
}

// TestWaitOrchestratorHealthy: the probe succeeds against a live mTLS
// orchestrator even with an empty node registry (unlike
// waitNodeRegistered, it only needs the RPC to answer).
func TestWaitOrchestratorHealthy(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := identity.LoadOrGenerate(clientDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}
	url := startOrchestrator(t, clientDir, cliID, nodectrl.NewRegistry(nil))
	if err := waitOrchestratorHealthy(context.Background(), url, clientDir, 5*time.Second); err != nil {
		t.Errorf("waitOrchestratorHealthy against a live orchestrator: %v", err)
	}
}

// TestWaitOrchestratorHealthyRejectsForeignListener: a plain-TCP service
// squatting on the port (no TLS) must NOT be mistaken for our
// orchestrator — the mTLS probe fails where a bare TCP-accept probe
// would have falsely passed.
func TestWaitOrchestratorHealthyRejectsForeignListener(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := identity.LoadOrGenerate(clientDir, identity.RoleClient); err != nil {
		t.Fatalf("client identity: %v", err)
	}
	ln, err := net.Listen("tcp", loopbackAny)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go acceptAndClose(ln)
	url := "https://" + ln.Addr().String()
	if err := waitOrchestratorHealthy(context.Background(), url, clientDir, 600*time.Millisecond); err == nil {
		t.Error("waitOrchestratorHealthy against a foreign TCP listener: want error")
	}
}

// acceptAndClose accepts and immediately closes connections so a TLS
// handshake against the listener fails fast (rather than stalling).
func acceptAndClose(ln net.Listener) {
	//daft:lint:disable=forloopbound reason="exits when the listener is closed (Accept errors)"
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func TestWaitNodeRegistered(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := identity.LoadOrGenerate(clientDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}

	reg := nodectrl.NewRegistry(nil)
	node, _ := identity.Generate(identity.RoleSandboxHost)
	reg.Register(node.Fingerprint(), "", "v1", nodectrl.Resources{CPUs: 1, MemoryBytes: 1 << 30})

	url := startOrchestrator(t, clientDir, cliID, reg)
	if err := waitNodeRegistered(context.Background(), url, clientDir, 5*time.Second); err != nil {
		t.Errorf("waitNodeRegistered with a node present: %v", err)
	}
}

func TestWaitNodeRegisteredTimesOutWithNoNode(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := identity.LoadOrGenerate(clientDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}
	reg := nodectrl.NewRegistry(nil) // empty — no node ever registers
	url := startOrchestrator(t, clientDir, cliID, reg)
	if err := waitNodeRegistered(context.Background(), url, clientDir, 600*time.Millisecond); err == nil {
		t.Errorf("waitNodeRegistered with no node: want timeout error")
	}
}

func TestUpWritesHandleAndDownStops(t *testing.T) {
	stubReadiness(t, nil, nil)
	state := t.TempDir()
	fake := fakeDaemon(t)

	c, err := Up(context.Background(), Config{
		StateDir:        state,
		Addr:            loopbackAny,
		OrchestratorBin: fake,
		SandboxdBin:     fake,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if c.ClientConfigDir != filepath.Join(state, clientSubdir) {
		t.Errorf("ClientConfigDir = %q", c.ClientConfigDir)
	}
	if len(c.PIDs) != 2 {
		t.Fatalf("want 2 pids, got %v", c.PIDs)
	}
	// pidfile + cluster.json written.
	if _, err := os.Stat(filepath.Join(state, pidFilename)); err != nil {
		t.Errorf("pidfile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, infoFilename)); err != nil {
		t.Errorf("cluster.json: %v", err)
	}
	// Both daemons alive.
	for _, pid := range c.PIDs {
		if syscall.Kill(pid, 0) != nil {
			t.Errorf("pid %d not alive after Up", pid)
		}
	}

	if err := Down(state, false); err != nil {
		t.Fatalf("Down: %v", err)
	}
	// Pidfile gone, processes reaped.
	if _, err := os.Stat(filepath.Join(state, pidFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile should be gone after Down")
	}
	for _, pid := range c.PIDs {
		if waitDead(pid, 3*time.Second) != nil {
			t.Errorf("pid %d still alive after Down", pid)
		}
	}
}

func TestUpReadinessFailureTearsDownChildren(t *testing.T) {
	// Orchestrator readiness "succeeds" but the sandbox never registers.
	stubReadiness(t, nil, func(context.Context, string, string, time.Duration) error {
		return errors.New("never registered")
	})
	state := t.TempDir()
	fake := fakeDaemon(t)

	_, err := Up(context.Background(), Config{
		StateDir:        state,
		Addr:            loopbackAny,
		OrchestratorBin: fake,
		SandboxdBin:     fake,
	})
	if err == nil {
		t.Fatal("Up: want error when sandbox-host never registers")
	}
	// No pidfile should be left behind on a failed Up.
	if _, statErr := os.Stat(filepath.Join(state, pidFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("failed Up left a pidfile")
	}
}

func TestUpFailsFastWhenDaemonDies(t *testing.T) {
	// Readiness "blocks" (as if a foreign listener held the port), but
	// the spawned daemon exits immediately — Up must detect the dead
	// child via the liveness poll and fail fast, well before the
	// readiness timeout.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	stubReadiness(t, func(context.Context, string, string, time.Duration) error {
		<-block
		return nil
	}, nil)

	state := t.TempDir()
	dead := filepath.Join(t.TempDir(), "dead-daemon")
	if err := os.WriteFile(dead, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test fixture.
		t.Fatalf("write: %v", err)
	}

	start := time.Now()
	_, err := Up(context.Background(), Config{
		StateDir: state, Addr: loopbackAny,
		OrchestratorBin: dead, SandboxdBin: dead,
		ReadyTimeout: time.Minute,
	})
	if err == nil {
		t.Fatal("Up: want error when the daemon exits during startup")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("error = %v, want 'exited during startup'", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Up took %s — should fail fast, not wait out readiness", elapsed)
	}
}

// TestUpFailsFastWhenAddrInUse is the foreign-service case: something
// already holds the orchestrator's address. Up must fail fast at the
// pre-flight — before spawning any daemon — with a clear "already in
// use" error, not limp on to a misleading downstream failure.
func TestUpFailsFastWhenAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", loopbackAny)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	state := t.TempDir()
	_, err = Up(context.Background(), Config{
		StateDir:        state,
		Addr:            ln.Addr().String(),
		OrchestratorBin: "/no/such/daft-orchestrator",
		SandboxdBin:     "/no/such/daft-sandboxd",
	})
	if err == nil {
		t.Fatal("Up with an occupied addr: want error")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("error = %v, want 'already in use'", err)
	}
	// Pre-flight runs before any spawn, so no pidfile is left behind.
	if _, statErr := os.Stat(filepath.Join(state, pidFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("failed pre-flight left a pidfile")
	}
}

func TestUpRejectsEmptyStateDir(t *testing.T) {
	if _, err := Up(context.Background(), Config{}); err == nil {
		t.Errorf("Up with empty StateDir: want error")
	}
}

func TestDownNoPidfileIsNoError(t *testing.T) {
	if err := Down(t.TempDir(), false); err != nil {
		t.Errorf("Down on empty dir: %v", err)
	}
}

func TestDownPurgeRemovesState(t *testing.T) {
	state := t.TempDir()
	if _, err := WireIdentities(state); err != nil {
		t.Fatalf("WireIdentities: %v", err)
	}
	if err := Down(state, true); err != nil {
		t.Fatalf("Down purge: %v", err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("purge should remove state dir")
	}
}

func TestReadPIDsRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, pidFilename)
	if err := os.WriteFile(path, []byte("notanumber\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readPIDs(path); err == nil {
		t.Errorf("readPIDs on garbage: want error")
	}
}

func TestSleepCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Errorf("sleepCtx with a cancelled ctx: want error")
	}
}

func TestWaitOrchestratorHealthyHonoursContext(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := identity.LoadOrGenerate(clientDir, identity.RoleClient); err != nil {
		t.Fatalf("client identity: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Nothing listens on :1; with the ctx already cancelled the probe
	// returns promptly (via the cancelled RPC / inter-attempt sleep)
	// rather than running out the long timeout.
	if err := waitOrchestratorHealthy(ctx, "https://127.0.0.1:1", clientDir, time.Hour); err == nil {
		t.Errorf("waitOrchestratorHealthy with a cancelled ctx: want error")
	}
}

func TestKillGroupGuards(t *testing.T) {
	// pid <= 1 is a no-op (must not signal pid 1 / the whole session).
	killGroup(0)
	killGroup(1)
}

func TestKillGroupEscalatesToSigkill(t *testing.T) {
	// A process group leader that ignores SIGTERM must still be killed
	// (SIGKILL after the grace period). We must wait for the shell to
	// actually install its trap before signalling — otherwise SIGTERM
	// races startup and the default action kills it before the trap is
	// in place (so killGroup would never reach the SIGKILL escalation).
	// The trailing loop also stops the shell exec-optimising into
	// `sleep` (which would drop the trap). killGroup reaps it, so no
	// separate Wait is needed.
	cmd := exec.Command("/bin/sh", "-c",
		"trap '' TERM; echo ready; while :; do sleep 1; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	buf := make([]byte, 8)
	if _, err := stdout.Read(buf); err != nil { // blocks until "ready\n"
		t.Fatalf("read ready: %v", err)
	}
	killGroup(pid) // ignores SIGTERM → escalates to SIGKILL after the grace
	if waitDead(pid, 6*time.Second) != nil {
		t.Errorf("pid %d survived killGroup", pid)
	}
}

func TestDownRejectsGarbagePidfile(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, pidFilename), []byte("xyz\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Down(state, false); err == nil {
		t.Errorf("Down with a garbage pidfile: want error")
	}
}

func TestWriteInfoErrorsOnMissingDir(t *testing.T) {
	if err := writeInfo(filepath.Join(t.TempDir(), "does-not-exist"),
		&Cluster{PIDs: []int{123}}); err == nil {
		t.Errorf("writeInfo into a missing dir: want error")
	}
}

func TestReadPIDsSkipsBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, pidFilename)
	if err := os.WriteFile(path, []byte("11\n\n22\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pids, err := readPIDs(path)
	if err != nil {
		t.Fatalf("readPIDs: %v", err)
	}
	if len(pids) != 2 {
		t.Errorf("readPIDs = %v, want 2 pids (blank line skipped)", pids)
	}
}

func TestReadPIDsParsesWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, pidFilename)
	if err := os.WriteFile(path, []byte("11\n22"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pids, err := readPIDs(path)
	if err != nil {
		t.Fatalf("readPIDs: %v", err)
	}
	if len(pids) != 2 || pids[0] != 11 || pids[1] != 22 {
		t.Errorf("readPIDs = %v, want [11 22]", pids)
	}
}

func TestUpErrorsWhenStateDirIsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Up(context.Background(), Config{StateDir: f}); err == nil {
		t.Errorf("Up with a file as the state dir: want error")
	}
}

func TestUpErrorsOnMissingDaemonBinary(t *testing.T) {
	// The orchestrator binary doesn't exist → its spawn fails inside
	// launchDaemon and Up bails before starting the sandbox-host.
	_, err := Up(context.Background(), Config{
		StateDir:        t.TempDir(),
		Addr:            loopbackAny,
		OrchestratorBin: "/no/such/daft-orchestrator",
	})
	if err == nil {
		t.Errorf("Up with a missing orchestrator binary: want error")
	}
}

func TestWireIdentitiesErrorsOnGarbagePeerStore(t *testing.T) {
	state := t.TempDir()
	// Pre-seed the orchestrator's peers.json with junk so the
	// cross-approval step's OpenPeerStore fails (identities still mint).
	if err := os.MkdirAll(filepath.Join(state, "orch"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "orch", peersFilename),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := WireIdentities(state); err == nil {
		t.Errorf("WireIdentities with a corrupt peer store: want error")
	}
}

func TestReadPIDsErrorsWhenPidfileIsADir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, pidFilename), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := readPIDs(filepath.Join(dir, pidFilename)); err == nil {
		t.Errorf("readPIDs on a directory: want error")
	}
}

func TestWriteInfoErrorsWhenClusterJSONIsADir(t *testing.T) {
	state := t.TempDir()
	// cluster.json as a directory makes its WriteFile fail while the
	// pidfile write succeeds.
	if err := os.Mkdir(filepath.Join(state, infoFilename), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeInfo(state, &Cluster{PIDs: []int{1}}); err == nil {
		t.Errorf("writeInfo with cluster.json as a dir: want error")
	}
}

func TestWaitNodeRegisteredHonoursContextCancellation(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := identity.LoadOrGenerate(clientDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}
	reg := nodectrl.NewRegistry(nil) // never registers a node
	url := startOrchestrator(t, clientDir, cliID, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := waitNodeRegistered(ctx, url, clientDir, time.Hour); err == nil {
		t.Errorf("waitNodeRegistered with a cancelled ctx: want error")
	}
}

func TestWaitNodeRegisteredErrorsOnGarbagePeerStore(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := identity.LoadOrGenerate(clientDir, identity.RoleClient); err != nil {
		t.Fatalf("client identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, peersFilename),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := waitNodeRegistered(context.Background(),
		"https://127.0.0.1:1", clientDir, time.Second); err == nil {
		t.Errorf("waitNodeRegistered with a corrupt peer store: want error")
	}
}

func TestWireIdentitiesErrorsWhenStateDirIsAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "iam-a-file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Subdirs can't be created under a regular file → identity bootstrap
	// fails.
	if _, err := WireIdentities(f); err == nil {
		t.Errorf("WireIdentities under a file: want error")
	}
}

func TestSpawnDaemonErrorsOnBadLogPath(t *testing.T) {
	if _, err := spawnDaemon("/bin/true", nil, nil,
		"/no/such/dir/daemon.log"); err == nil {
		t.Errorf("spawnDaemon with an unwritable log path: want error")
	}
}

func TestWaitNodeRegisteredErrorsWithoutClientIdentity(t *testing.T) {
	// Empty client dir → no identity to present.
	if err := waitNodeRegistered(context.Background(),
		"https://127.0.0.1:1", t.TempDir(), time.Second); err == nil {
		t.Errorf("waitNodeRegistered without a client identity: want error")
	}
}

// --- helpers ---

func mustPeers(t *testing.T, dir string) *identity.PeerStore {
	t.Helper()
	s, err := identity.OpenPeerStore(filepath.Join(dir, peersFilename))
	if err != nil {
		t.Fatalf("OpenPeerStore(%s): %v", dir, err)
	}
	return s
}

func approved(t *testing.T, s *identity.PeerStore, fpHex string) bool {
	t.Helper()
	for _, p := range s.List() {
		if p.Fingerprint.String() == fpHex {
			return true
		}
	}
	return false
}

// startOrchestrator runs an in-process orchestrator that approves
// cliID and serves reg, with cliID's store approving the orchestrator.
// Returns the https URL.
func startOrchestrator(t *testing.T, clientDir string, cliID *identity.Identity, reg *nodectrl.Registry) string {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("orchestrator identity: %v", err)
	}
	srvStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), peersFilename))
	if err != nil {
		t.Fatalf("server peer store: %v", err)
	}
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("approve client: %v", err)
	}
	srv, err := orchestrator.New(orchestrator.Config{
		Identity: srvID, Peers: srvStore, Addr: loopbackAny, NodeRegistry: reg,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	cliStore, err := identity.OpenPeerStore(filepath.Join(clientDir, peersFilename))
	if err != nil {
		t.Fatalf("client peer store: %v", err)
	}
	if err := cliStore.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("approve orchestrator: %v", err)
	}
	return "https://" + srv.Addr()
}

// stubReadiness swaps the readiness seams for the test and restores
// them on cleanup. A nil func means "succeed immediately".
func stubReadiness(
	t *testing.T,
	orch func(context.Context, string, string, time.Duration) error,
	node func(context.Context, string, string, time.Duration) error,
) {
	t.Helper()
	origOrch, origNode := waitOrchestratorReady, waitSandboxRegistered
	if orch == nil {
		orch = func(context.Context, string, string, time.Duration) error { return nil }
	}
	if node == nil {
		node = func(context.Context, string, string, time.Duration) error { return nil }
	}
	waitOrchestratorReady, waitSandboxRegistered = orch, node
	t.Cleanup(func() { waitOrchestratorReady, waitSandboxRegistered = origOrch, origNode })
}

// fakeDaemon writes an executable that ignores its args and sleeps, so
// Up's spawn + pidfile + Down teardown can be exercised without real
// daemons.
func fakeDaemon(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-daemon")
	script := "#!/bin/sh\nexec sleep 60\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture.
		t.Fatalf("write fake daemon: %v", err)
	}
	return path
}

func waitDead(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Reap if it's our (now-dead) child, so a zombie doesn't read as
		// alive; a non-child / already-gone pid shows up via Kill ESRCH.
		var ws syscall.WaitStatus
		if wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); wpid == pid || errors.Is(err, syscall.ECHILD) {
			return nil
		}
		if syscall.Kill(pid, 0) != nil {
			//nolint:nilerr // non-nil Kill(sig 0) ⇒ process gone ⇒ dead, the success case.
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("still alive")
}
