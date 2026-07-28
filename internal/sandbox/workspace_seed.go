package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// WorkspaceSeedBaseVsockPort is the first dedicated CAS-to-guest seed port.
// Ports are per-VM hybrid-vsock sockets, so the same bounded range is reused by
// every Sandbox without cross-tenant routing.
const WorkspaceSeedBaseVsockPort uint32 = 9400

func startWorkspaceSeedServers(ctx context.Context, cfg Config) (func(), error) {
	if len(cfg.WorkspaceSeeds) == 0 {
		return func() {}, nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	listeners := make([]net.Listener, 0, len(cfg.WorkspaceSeeds))
	var workers sync.WaitGroup
	stop := func() {
		cancel()
		for _, listener := range listeners {
			_ = listener.Close()
		}
		workers.Wait()
		for _, seed := range cfg.WorkspaceSeeds {
			oops.Skip("remove workspace seed socket", os.Remove(workspaceSeedSocket(cfg, seed.Port)), fs.ErrNotExist)
		}
	}
	for _, seed := range cfg.WorkspaceSeeds {
		if seed.Port == 0 || seed.Stream == nil {
			stop()
			return func() {}, fmt.Errorf("sandbox: invalid workspace seed on port %d", seed.Port)
		}
		path := workspaceSeedSocket(cfg, seed.Port)
		oops.Skip("remove stale workspace seed socket", os.Remove(path), fs.ErrNotExist)
		listener, err := net.Listen("unix", path)
		if err != nil {
			stop()
			return func() {}, fmt.Errorf("sandbox: listen workspace seed %d: %w", seed.Port, err)
		}
		listeners = append(listeners, listener)
		workers.Add(1)
		go func(listener net.Listener, seed WorkspaceSeed) {
			defer workers.Done()
			serveWorkspaceSeed(runCtx, listener, seed)
		}(listener, seed)
	}
	return stop, nil
}

func workspaceSeedSocket(cfg Config, port uint32) string {
	return fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, port)
}

func serveWorkspaceSeed(ctx context.Context, listener net.Listener, seed WorkspaceSeed) {
	//daft:lint:disable=forloopbound reason="bounded by listener close during VM teardown"
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_, _ = seed.Stream(ctx, conn)
		}()
	}
}
