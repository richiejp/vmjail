package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/devcontainer"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

const upSubName = "up"

// upLaunchTimeout bounds the one-shot Launch RPC `up` issues before it
// settles into the (unbounded) port-forward loop. Used when the caller
// left --timeout at zero.
const upLaunchTimeout = 10 * time.Second

// newWorkspaceUpCmd returns `daft workspace up`: the one-command path to a
// running devcontainer. It dispatches the workspace, then opens a local
// listener for every devcontainer forwardPort and blocks until Ctrl-C —
// mirroring `docker compose up` / VS Code "open folder in container". The
// devcontainer postStartCommand runs automatically in the container
// (agent-side), so `up` only handles the launch + port forwarding.
func newWorkspaceUpCmd() *cobra.Command {
	var opts workspaceClientOpts
	var bind string
	cmd := &cobra.Command{
		Use:   upSubName + " [workspace]",
		Short: "Launch a workspace and auto-open its devcontainer forwardPorts",
		Long: "Dispatches the workspace to its pinned sandbox-host (like\n" +
			"`launch`), then opens a local TCP listener for each forwardPort\n" +
			"declared in the workspace's devcontainer.json and tunnels it into\n" +
			"the container — so a dev server bound inside the VM is reachable on\n" +
			"localhost with no extra `daft workspace forward`. Blocks until\n" +
			"Ctrl-C. The devcontainer postStartCommand runs in the container on\n" +
			"its own. Re-running against an already-running workspace just\n" +
			"(re)attaches the forwards.\n\n" +
			"forwardPorts are read from the devcontainer.json path recorded by\n" +
			"`daft workspace devcontainer` (resolved client-side; the\n" +
			"orchestrator doesn't report them back).",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.name = args[0]
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runWorkspaceUp(ctx, cmd.OutOrStdout(), opts, bind)
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1",
		"interface to bind the local listeners on (127.0.0.1 for localhost-only)")
	return cmd
}

// upDeps is the test seam for `up`: the shared port-forward side effects
// plus the forwardPorts source (default: the dir-local devcontainer.json).
type upDeps struct {
	forwardDeps
	forwardPorts func() ([]int, error)
}

func defaultUpDeps() upDeps {
	return upDeps{forwardDeps: defaultForwardDeps(), forwardPorts: forwardPortsFromDevcontainer}
}

func runWorkspaceUp(ctx context.Context, stdout io.Writer, opts workspaceClientOpts, bind string) error {
	return runWorkspaceUpWith(ctx, stdout, opts, bind, defaultUpDeps())
}

func runWorkspaceUpWith(
	ctx context.Context, stdout io.Writer,
	opts workspaceClientOpts, bind string, deps upDeps,
) error {
	name := opts.name
	if name == "" {
		derived, err := resolveWorkspaceNameDefault()
		if err != nil {
			return err
		}
		name = derived
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	client, err := deps.dialOrchestrator(opts.configDir, orchestrator)
	if err != nil {
		return err
	}

	timeout := opts.timeout
	if timeout <= 0 {
		timeout = upLaunchTimeout
	}
	if err := launchForUp(ctx, stdout, client, name, timeout); err != nil {
		return err
	}

	ports, err := deps.forwardPorts()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		_, _ = fmt.Fprintf(stdout, "daft workspace up: %s launched; no forwardPorts to open\n", name)
		return nil
	}
	return forwardAllPorts(ctx, stdout, client, name, bind, ports, deps.forwardDeps)
}

// launchForUp dispatches the workspace, tolerating an already-dispatched
// one so `up` is idempotent (re-running just re-attaches the forwards).
func launchForUp(
	ctx context.Context, stdout io.Writer,
	client workspacev1connect.WorkspaceCatalogServiceClient,
	name string, timeout time.Duration,
) error {
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := client.Launch(lctx, connect.NewRequest(
		&workspacev1.LaunchRequest{Handle: handleByName(name)},
	))
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(stdout, "daft workspace up: launched %s\n", name)
	case isAlreadyDispatched(err):
		_, _ = fmt.Fprintf(stdout, "daft workspace up: %s already running; attaching forwards\n", name)
	default:
		return err
	}
	return nil
}

// forwardAllPorts opens one forward per container port concurrently,
// sharing the dialed client, and blocks until ctx is cancelled (each
// forwardOnePort runs until its listener is closed by the ctx watchdog).
// A single port's failure is logged and doesn't tear down the others.
func forwardAllPorts(
	ctx context.Context, stdout io.Writer,
	client workspacev1connect.WorkspaceCatalogServiceClient,
	name, bind string, ports []int, deps forwardDeps,
) error {
	_, _ = fmt.Fprintf(stdout, "daft workspace up: forwarding %d port(s) for %s\n", len(ports), name)
	var wg sync.WaitGroup
	for _, p := range ports {
		if p < 1 || p > 65535 {
			_, _ = fmt.Fprintf(stdout, "daft workspace up: skipping out-of-range forwardPort %d\n", p)
			continue
		}
		port := uint32(p) //nolint:gosec // G115: bounded to 1..65535 by the check above.
		opts := workspaceForwardOpts{
			workspace:     name,
			bind:          bind,
			localPort:     port,
			containerPort: port,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := forwardOnePort(ctx, stdout, client, opts, deps); err != nil {
				_, _ = fmt.Fprintf(stdout, "daft workspace up: forward %d: %v\n", opts.localPort, err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// handleByName builds a name-addressed workspace handle.
func handleByName(name string) *workspacev1.WorkspaceHandle {
	return &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Name{Name: name}}
}

// isAlreadyDispatched reports whether err is the orchestrator's
// "already dispatched; Stop before relaunching" precondition, which `up`
// treats as success (the workspace is up; just attach the forwards).
func isAlreadyDispatched(err error) bool {
	return connect.CodeOf(err) == connect.CodeFailedPrecondition &&
		strings.Contains(err.Error(), "already dispatched")
}

// forwardPortsFromDevcontainer reads the devcontainer.json host path
// recorded dir-local by `daft workspace devcontainer` and returns its
// resolved forwardPorts. No record (or no devcontainer) yields nil — `up`
// then launches and forwards nothing, which is fine for an image-based or
// port-less workspace.
func forwardPortsFromDevcontainer() ([]int, error) {
	host, err := dirLocalDevcontainer()
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, nil
	}
	// #nosec G304 — host is a path the user recorded via `daft workspace
	// devcontainer`, read on their own machine.
	data, err := os.ReadFile(host)
	if err != nil {
		return nil, fmt.Errorf("daft workspace up: read devcontainer %s: %w", host, err)
	}
	spec, err := devcontainer.Resolve(data, devcontainer.Substitutions{})
	if err != nil {
		return nil, fmt.Errorf("daft workspace up: parse devcontainer %s: %w", host, err)
	}
	return spec.ForwardPorts, nil
}

// rememberDevcontainerPath records (or clears, for an empty relPath) the
// devcontainer.json's absolute host path dir-local, so `daft workspace up`
// can later read its forwardPorts. Best-effort: a failure is reported but
// never blocks the `devcontainer` command's orchestrator call.
func rememberDevcontainerPath(stderr io.Writer, relPath string) {
	host := ""
	if relPath != "" {
		abs, err := filepath.Abs(relPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "daft workspace devcontainer: record path: %v\n", err)
			return
		}
		host = abs
	}
	if err := setDirLocalDevcontainer(host); err != nil {
		_, _ = fmt.Fprintf(stderr, "daft workspace devcontainer: record path dir-local: %v\n", err)
	}
}
