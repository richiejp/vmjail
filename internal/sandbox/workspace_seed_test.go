package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceSeedServerStreamsAndCleansSocket(t *testing.T) {
	cfg := Config{Vsock: &VsockConfig{HostSocket: filepath.Join(t.TempDir(), "vm.sock")}}
	cfg.WorkspaceSeeds = []WorkspaceSeed{{
		Port: WorkspaceSeedBaseVsockPort,
		Stream: func(_ context.Context, destination io.Writer) (bool, error) {
			_, err := io.WriteString(destination, "archive")
			return true, err
		},
	}}
	stop, err := startWorkspaceSeedServers(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := workspaceSeedSocket(cfg, WorkspaceSeedBaseVsockPort)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil || string(payload) != "archive" {
		t.Fatalf("payload = %q, %v", payload, err)
	}
	stop()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed socket remains: %v", err)
	}
}

func TestWorkspaceSeedServerValidatesConfiguration(t *testing.T) {
	cfg := Config{Vsock: &VsockConfig{HostSocket: filepath.Join(t.TempDir(), "vm.sock")}}
	for _, seed := range []WorkspaceSeed{{}, {Port: WorkspaceSeedBaseVsockPort}} {
		cfg.WorkspaceSeeds = []WorkspaceSeed{seed}
		if stop, err := startWorkspaceSeedServers(t.Context(), cfg); err == nil {
			stop()
			t.Fatalf("invalid seed accepted: %#v", seed)
		}
	}
	if stop, err := startWorkspaceSeedServers(t.Context(), Config{}); err != nil {
		t.Fatal(err)
	} else {
		stop()
	}
	badPath := Config{Vsock: &VsockConfig{HostSocket: filepath.Join(t.TempDir(), "missing", "vm.sock")}}
	badPath.WorkspaceSeeds = []WorkspaceSeed{{Port: WorkspaceSeedBaseVsockPort, Stream: func(context.Context, io.Writer) (bool, error) { return false, nil }}}
	if stop, err := startWorkspaceSeedServers(t.Context(), badPath); err == nil {
		stop()
		t.Fatal("uncreatable seed socket accepted")
	}
}

func TestConfigValidatesWorkspaceSeedBindings(t *testing.T) {
	stream := func(context.Context, io.Writer) (bool, error) { return false, nil }
	tests := []Config{
		func() Config {
			cfg := validCfg()
			cfg.WorkspaceSeeds = []WorkspaceSeed{{Port: WorkspaceSeedBaseVsockPort, Stream: stream}}
			return cfg
		}(),
		func() Config {
			cfg := validCfg()
			cfg.Vsock = &VsockConfig{HostSocket: "/run/daft/vm.sock"}
			cfg.WorkspaceSeeds = []WorkspaceSeed{{}}
			return cfg
		}(),
		func() Config {
			cfg := validCfg()
			cfg.Vsock = &VsockConfig{HostSocket: "/run/daft/vm.sock"}
			cfg.WorkspaceSeeds = []WorkspaceSeed{
				{Port: WorkspaceSeedBaseVsockPort, Stream: stream},
				{Port: WorkspaceSeedBaseVsockPort, Stream: stream},
			}
			return cfg
		}(),
	}
	for index := range tests {
		if err := tests[index].validate(); err == nil {
			t.Fatalf("invalid seed config %d accepted", index)
		}
	}
}
