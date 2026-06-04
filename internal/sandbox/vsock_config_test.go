package sandbox

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
)

// configSockPath is the UDS the config server binds for a given cfg —
// the hybrid-vsock convention <HostSocket>_<port>.
func configSockPath(cfg Config) string {
	return fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultConfigVsockPort)
}

// TestStartVsockConfigServerServesBundleAndShares drives a full
// handshake against the server and asserts it replies with the
// bundle + shares projected from the boot Config.
func TestStartVsockConfigServerServesBundleAndShares(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Bundle = "/bundles/abc"
	cfg.Shares = []*ShareConfig{
		{Tag: testTagWorkspace, Mountpoint: testMountpoint},
		{Tag: "observe", Mountpoint: "/run/observe"},
	}
	cfg.Vsock = &VsockConfig{HostSocket: filepath.Join(dir, "v.sock"), GuestCID: 3}

	stop, bootComplete, err := startVsockConfigServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockConfigServer: %v", err)
	}
	t.Cleanup(stop)

	conn, err := net.Dial("unix", configSockPath(cfg))
	if err != nil {
		t.Fatalf("dial config socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := agent.WriteGuestConfigRequest(conn, &agentv1.GuestConfigRequest{AgentVersion: "1"}); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	got, err := agent.ReadGuestConfig(conn)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if got.GetBundle() != "/bundles/abc" {
		t.Errorf("bundle = %q, want /bundles/abc", got.GetBundle())
	}
	shares := got.GetShares()
	if len(shares) != 2 {
		t.Fatalf("shares = %d, want 2", len(shares))
	}
	if shares[0].GetTag() != testTagWorkspace || shares[0].GetMountpoint() != testMountpoint {
		t.Errorf("share[0] = %v, want workspace:/share", shares[0])
	}
	if shares[1].GetTag() != "observe" || shares[1].GetMountpoint() != "/run/observe" {
		t.Errorf("share[1] = %v, want observe:/run/observe", shares[1])
	}

	// Streaming a BOOT_COMPLETE event back closes the bootComplete chan.
	if err := agent.WriteAgentEvent(conn, &agentv1.AgentEvent{Kind: agentv1.AgentEvent_BOOT_COMPLETE}); err != nil {
		t.Fatalf("write boot-complete: %v", err)
	}
	select {
	case <-bootComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("bootComplete not signalled after BOOT_COMPLETE event")
	}
}

// TestStartVsockConfigServerDropsEarlyClose covers the handler's
// handshake-read error branch: a client that connects and closes before
// sending its handshake is dropped without signalling boot-complete.
func TestStartVsockConfigServerDropsEarlyClose(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: filepath.Join(dir, "v.sock"), GuestCID: 3}

	stop, bootComplete, err := startVsockConfigServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockConfigServer: %v", err)
	}
	t.Cleanup(stop)

	conn, err := net.Dial("unix", configSockPath(cfg))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close() // close before handshake

	select {
	case <-bootComplete:
		t.Fatal("boot-complete signalled for a client that never handshook")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestStartVsockConfigServerNoVsockIsNoop confirms a VM with no vsock
// device gets a do-nothing stop and no listener (the agent will fall
// back to the cmdline).
func TestStartVsockConfigServerNoVsockIsNoop(t *testing.T) {
	stop, bootComplete, err := startVsockConfigServer(context.Background(), validCfg())
	if err != nil {
		t.Fatalf("startVsockConfigServer with no Vsock: %v", err)
	}
	if bootComplete != nil {
		t.Error("expected nil bootComplete channel when no vsock device")
	}
	stop() // must be safe to call
}

// TestStartVsockConfigServerSurfacesListenError exercises the path
// where the listen syscall fails (parent dir missing). The returned
// stop must be safe to call.
func TestStartVsockConfigServerSurfacesListenError(t *testing.T) {
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: "/no/such/dir/config.sock", GuestCID: 3}
	stop, _, err := startVsockConfigServer(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from Listen on bad path")
	}
	stop() // must not panic
}

// TestStartVsockConfigServerPreCleansStaleSocket simulates a previous
// run leaving a stale socket file; the new listener must remove it and
// bind successfully.
func TestStartVsockConfigServerPreCleansStaleSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: filepath.Join(dir, "v.sock"), GuestCID: 3}

	stale, err := net.Listen("unix", configSockPath(cfg))
	if err != nil {
		t.Fatalf("preseed listener: %v", err)
	}
	_ = stale.Close() // leaves the socket file behind

	stop, _, err := startVsockConfigServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockConfigServer against stale socket: %v", err)
	}
	stop()
}

// TestBuildGuestConfigProjectsConfig covers the Config → GuestConfig
// projection directly, including the empty case.
func TestBuildGuestConfigProjectsConfig(t *testing.T) {
	empty := buildGuestConfig(validCfg())
	if empty.GetBundle() != "" || len(empty.GetShares()) != 0 {
		t.Errorf("empty cfg → %v, want zero-valued GuestConfig", empty)
	}

	// An observe-free cfg carries no Observe block at all.
	if empty.GetObserve() != nil {
		t.Errorf("empty cfg → Observe %v, want nil", empty.GetObserve())
	}

	cfg := validCfg()
	cfg.Bundle = "/b"
	cfg.Shares = []*ShareConfig{{Tag: "t", Mountpoint: "/m", Overlay: true}}
	cfg.PortForwards = []PortForward{{VsockPort: 10001, ContainerPort: 8080}}
	cfg.OpensnoopLogPath = "/run/daft/observe/opensnoop.log"
	cfg.ObserveSummaryPath = "/run/daft/observe/progress.json"
	gc := buildGuestConfig(cfg)
	if gc.GetBundle() != "/b" {
		t.Errorf("bundle = %q, want /b", gc.GetBundle())
	}
	if len(gc.GetShares()) != 1 || gc.GetShares()[0].GetTag() != "t" || !gc.GetShares()[0].GetOverlay() {
		t.Errorf("shares = %v, want one t:/m overlay", gc.GetShares())
	}
	if pfs := gc.GetPortForwards(); len(pfs) != 1 || pfs[0].GetVsockPort() != 10001 || pfs[0].GetContainerPort() != 8080 {
		t.Errorf("port_forwards = %v, want one 10001->8080", pfs)
	}
	obs := gc.GetObserve()
	if obs.GetOpensnoop() != "/run/daft/observe/opensnoop.log" || obs.GetSummary() != "/run/daft/observe/progress.json" {
		t.Errorf("observe = %v, want opensnoop+summary set", obs)
	}
	// Paths left unset stay empty rather than echoing another gadget's.
	if obs.GetExecsnoop() != "" || obs.GetIosnoop() != "" {
		t.Errorf("observe = %v, want exec/io unset", obs)
	}
}
