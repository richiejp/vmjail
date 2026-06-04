package sandbox

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

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

	stop, err := startVsockConfigServer(context.Background(), cfg)
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
}

// TestStartVsockConfigServerNoVsockIsNoop confirms a VM with no vsock
// device gets a do-nothing stop and no listener (the agent will fall
// back to the cmdline).
func TestStartVsockConfigServerNoVsockIsNoop(t *testing.T) {
	stop, err := startVsockConfigServer(context.Background(), validCfg())
	if err != nil {
		t.Fatalf("startVsockConfigServer with no Vsock: %v", err)
	}
	stop() // must be safe to call
}

// TestStartVsockConfigServerSurfacesListenError exercises the path
// where the listen syscall fails (parent dir missing). The returned
// stop must be safe to call.
func TestStartVsockConfigServerSurfacesListenError(t *testing.T) {
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: "/no/such/dir/config.sock", GuestCID: 3}
	stop, err := startVsockConfigServer(context.Background(), cfg)
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

	stop, err := startVsockConfigServer(context.Background(), cfg)
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

	cfg := validCfg()
	cfg.Bundle = "/b"
	cfg.Shares = []*ShareConfig{{Tag: "t", Mountpoint: "/m"}}
	gc := buildGuestConfig(cfg)
	if gc.GetBundle() != "/b" {
		t.Errorf("bundle = %q, want /b", gc.GetBundle())
	}
	if len(gc.GetShares()) != 1 || gc.GetShares()[0].GetTag() != "t" {
		t.Errorf("shares = %v, want one t:/m", gc.GetShares())
	}
}
