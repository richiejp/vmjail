package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// upTestDeps builds upDeps wired to client, recording every listen addr
// and serving a real OS-assigned loopback listener per port (so
// forwardOnePort's accept loop blocks until the ctx watchdog closes it).
func upTestDeps(client workspacev1connect.WorkspaceCatalogServiceClient, ports []int, recorded *[]string, mu *sync.Mutex) upDeps {
	return upDeps{
		forwardDeps: forwardDeps{
			dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
				return client, nil
			},
			listenLocal: func(addr string) (net.Listener, error) {
				mu.Lock()
				*recorded = append(*recorded, addr)
				mu.Unlock()
				return net.Listen("tcp", "127.0.0.1:0")
			},
			openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
				return nil // no connections are accepted in these tests
			},
		},
		forwardPorts: func() ([]int, error) { return ports, nil },
	}
}

func TestRunWorkspaceUpOpensAForwardPerPort(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{})
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(client, []int{3000, 8080}, &recorded, &mu)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWorkspaceUpWith(ctx, &buf, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps)
	}()

	// Wait for both listeners to be opened, then tear down via ctx.
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(recorded) == 2
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runWorkspaceUp: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), recorded...)
	mu.Unlock()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "127.0.0.1:3000" || got[1] != "127.0.0.1:8080" {
		t.Fatalf("listen addrs = %v, want :3000 and :8080", got)
	}
	if !strings.Contains(buf.String(), "launched ws") {
		t.Errorf("missing launch line: %q", buf.String())
	}
}

func TestRunWorkspaceUpDefaultsWorkspaceName(t *testing.T) {
	t.Chdir(t.TempDir()) // name defaults to basename(cwd)
	client := fakeCatalogClient(t, &fakeCatalog{})
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(client, nil, &recorded, &mu) // no ports → returns after launch

	var buf bytes.Buffer
	if err := runWorkspaceUpWith(context.Background(), &buf, workspaceClientOpts{}, "127.0.0.1", deps); err != nil {
		t.Fatalf("up with default name: %v", err)
	}
	if !strings.Contains(buf.String(), "launched ") {
		t.Errorf("default-name launch should still report a launch: %q", buf.String())
	}
}

func TestRunWorkspaceUpForwardListenErrorLogged(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{})
	deps := upDeps{
		forwardDeps: forwardDeps{
			dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
				return client, nil
			},
			listenLocal: func(string) (net.Listener, error) { return nil, errString("bind failed") },
		},
		forwardPorts: func() ([]int, error) { return []int{3000}, nil },
	}
	var buf bytes.Buffer
	// listenLocal fails → forwardOnePort returns fast → the per-port error is
	// logged and runWorkspaceUp returns without blocking.
	if err := runWorkspaceUpWith(context.Background(), &buf, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !strings.Contains(buf.String(), "forward 3000:") {
		t.Errorf("a per-port listen failure should be logged: %q", buf.String())
	}
}

func TestRunWorkspaceUpDialError(t *testing.T) {
	deps := upDeps{
		forwardDeps: forwardDeps{
			dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
				return nil, errString("dial failed")
			},
		},
		forwardPorts: func() ([]int, error) { return nil, nil },
	}
	if err := runWorkspaceUpWith(context.Background(), &bytes.Buffer{}, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps); err == nil {
		t.Fatal("a dial error must propagate")
	}
}

func TestRunWorkspaceUpForwardPortsError(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{})
	deps := upDeps{
		forwardDeps: forwardDeps{
			dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
				return client, nil
			},
		},
		forwardPorts: func() ([]int, error) { return nil, errString("read devcontainer failed") },
	}
	// Launch succeeds (fake), then the forwardPorts source errors → propagate.
	if err := runWorkspaceUpWith(context.Background(), &bytes.Buffer{}, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps); err == nil {
		t.Fatal("a forwardPorts resolution error must propagate")
	}
}

func TestRunWorkspaceUpNoPorts(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{})
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(client, nil, &recorded, &mu)

	var buf bytes.Buffer
	// No ports → returns immediately (no blocking forward loop).
	if err := runWorkspaceUpWith(context.Background(), &buf, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps); err != nil {
		t.Fatalf("runWorkspaceUp: %v", err)
	}
	if len(recorded) != 0 {
		t.Errorf("no ports should open no listeners, got %v", recorded)
	}
	if !strings.Contains(buf.String(), "no forwardPorts") {
		t.Errorf("missing no-ports line: %q", buf.String())
	}
}

func TestRunWorkspaceUpToleratesAlreadyDispatched(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{
		launchErr: connect.NewError(connect.CodeFailedPrecondition,
			errString("workspace: already dispatched; Stop before relaunching")),
	})
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(client, []int{3000}, &recorded, &mu)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWorkspaceUpWith(ctx, &buf, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps)
	}()
	waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(recorded) == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("already-dispatched should be tolerated, got %v", err)
	}
	if !strings.Contains(buf.String(), "already running") {
		t.Errorf("missing already-running line: %q", buf.String())
	}
}

func TestRunWorkspaceUpLaunchErrorPropagates(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{
		launchErr: connect.NewError(connect.CodeInternal, errString("boom")),
	})
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(client, []int{3000}, &recorded, &mu)

	err := runWorkspaceUpWith(context.Background(), &bytes.Buffer{}, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps)
	if err == nil {
		t.Fatal("a hard launch error must propagate")
	}
	if len(recorded) != 0 {
		t.Errorf("no forwards should open when launch fails, got %v", recorded)
	}
}

func TestRunWorkspaceUpSkipsOutOfRangePort(t *testing.T) {
	client := fakeCatalogClient(t, &fakeCatalog{})
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(client, []int{3000, 70000}, &recorded, &mu)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWorkspaceUpWith(ctx, &buf, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps)
	}()
	// Only the in-range port opens a listener.
	waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(recorded) == 1 })
	cancel()
	<-done
	if !strings.Contains(buf.String(), "out-of-range forwardPort 70000") {
		t.Errorf("out-of-range port should be skipped with a notice: %q", buf.String())
	}
}

// TestWorkspaceUpCmdLaunchError drives the cobra command end to end against
// an unreachable orchestrator: the Launch RPC fails fast, exercising the
// RunE → runWorkspaceUp → defaultUpDeps → launch-error path.
func TestWorkspaceUpCmdLaunchError(t *testing.T) {
	t.Chdir(t.TempDir())
	_, _, err := runCmd(t, "workspace", upSubName, "ws",
		"--orchestrator", "https://127.0.0.1:1",
		"--timeout", "1s",
		"--config-dir", t.TempDir())
	if err == nil {
		t.Fatal("up against an unreachable orchestrator should error")
	}
}

func TestWorkspaceUpCmdNoArgLaunchError(t *testing.T) {
	t.Chdir(t.TempDir()) // no positional → name defaults to basename(cwd)
	_, _, err := runCmd(t, "workspace", upSubName,
		"--orchestrator", "https://127.0.0.1:1",
		"--timeout", "1s",
		"--config-dir", t.TempDir())
	if err == nil {
		t.Fatal("up against an unreachable orchestrator should error")
	}
}

func TestSetDirLocalDevcontainerLoadError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Make ./.daft a regular file so loading ./.daft/config.json fails with
	// ENOTDIR, exercising the helpers' load-error branch.
	if err := os.WriteFile(filepath.Join(dir, ".daft"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setDirLocalDevcontainer("/abs/devcontainer.json"); err == nil {
		t.Error("setDirLocalDevcontainer should surface a config-load error")
	}
	if _, err := dirLocalDevcontainer(); err == nil {
		t.Error("dirLocalDevcontainer should surface a config-load error")
	}
}

func TestForwardPortsFromDevcontainerErrors(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.WriteFile(filepath.Join(dir, "dc.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := rememberDevcontainerPath(&bytes.Buffer{}, "dc.json"); err != nil {
			t.Fatal(err)
		}
		if _, err := forwardPortsFromDevcontainer(); err == nil {
			t.Error("malformed devcontainer.json should error")
		}
	})
	t.Run("missing-file", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := rememberDevcontainerPath(&bytes.Buffer{}, "ghost.json"); err != nil { // records an abs path that doesn't exist
			t.Fatal(err)
		}
		if _, err := forwardPortsFromDevcontainer(); err == nil {
			t.Error("a recorded-but-missing devcontainer.json should error")
		}
	})
}

func TestForwardPortsFromDevcontainerConfigLoadError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".daft"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := forwardPortsFromDevcontainer(); err == nil {
		t.Error("a dir-local config load error should propagate")
	}
}

func TestRunWorkspaceUpResolveOrchestratorError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// ./.daft as a file makes resolveOrchestrator's dir-local config load fail.
	if err := os.WriteFile(filepath.Join(dir, ".daft"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var (
		mu       sync.Mutex
		recorded []string
	)
	deps := upTestDeps(fakeCatalogClient(t, &fakeCatalog{}), nil, &recorded, &mu)
	if err := runWorkspaceUpWith(context.Background(), &bytes.Buffer{}, workspaceClientOpts{name: "ws"}, "127.0.0.1", deps); err == nil {
		t.Error("a resolveOrchestrator error should propagate")
	}
}

func TestForwardPortsFromDevcontainer(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	dcPath := filepath.Join(dir, ".devcontainer", "devcontainer.json")
	if err := os.MkdirAll(filepath.Dir(dcPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dcPath, []byte(`{"image":"x","forwardPorts":[3000,5173]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rememberDevcontainerPath(&bytes.Buffer{}, ".devcontainer/devcontainer.json"); err != nil {
		t.Fatal(err)
	}

	ports, err := forwardPortsFromDevcontainer()
	if err != nil {
		t.Fatalf("forwardPortsFromDevcontainer: %v", err)
	}
	if len(ports) != 2 || ports[0] != 3000 || ports[1] != 5173 {
		t.Fatalf("ports = %v, want [3000 5173]", ports)
	}
}

func TestForwardPortsFromDevcontainerNoRecord(t *testing.T) {
	t.Chdir(t.TempDir()) // no .daft/config.json
	ports, err := forwardPortsFromDevcontainer()
	if err != nil || ports != nil {
		t.Fatalf("no record → (nil,nil), got (%v,%v)", ports, err)
	}
}

func TestRememberDevcontainerPathClears(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := rememberDevcontainerPath(&bytes.Buffer{}, ".devcontainer/devcontainer.json"); err != nil {
		t.Fatal(err)
	}
	if got, _ := dirLocalDevcontainer(); !filepath.IsAbs(got) {
		t.Errorf("recorded path should be absolute, got %q", got)
	}
	if err := rememberDevcontainerPath(&bytes.Buffer{}, ""); err != nil { // clear
		t.Fatal(err)
	}
	if got, _ := dirLocalDevcontainer(); got != "" {
		t.Errorf("empty arg should clear the recorded path, got %q", got)
	}
}

func TestRememberDevcontainerPathReturnsLocalStateErrors(t *testing.T) {
	t.Run("working directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := rememberDevcontainerPath(io.Discard, "dc.json"); err == nil {
			t.Fatal("remember succeeded without a working directory")
		}
	})
	t.Run("config path", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.WriteFile(filepath.Join(dir, ".daft"), []byte("blocked"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := rememberDevcontainerPath(io.Discard, "dc.json"); err == nil {
			t.Fatal("remember succeeded with a blocked config path")
		}
	})
}

func TestIsAlreadyDispatched(t *testing.T) {
	yes := connect.NewError(connect.CodeFailedPrecondition,
		errString("workspace: already dispatched; Stop before relaunching"))
	if !isAlreadyDispatched(yes) {
		t.Error("already-dispatched precondition should match")
	}
	no := connect.NewError(connect.CodeFailedPrecondition, errString("not ready, missing: image"))
	if isAlreadyDispatched(no) {
		t.Error("a different precondition must not match")
	}
	if isAlreadyDispatched(nil) {
		t.Error("nil error must not match")
	}
}

// errString is a tiny error wrapper so tests can build connect errors with
// a fixed message without importing errors in every case.
type errString string

func (e errString) Error() string { return string(e) }
