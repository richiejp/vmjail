package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/bundled"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
	"github.com/localai-org/darkaifactory/internal/nodectrlclient"
	"github.com/localai-org/darkaifactory/internal/sandbox"
	"github.com/localai-org/darkaifactory/internal/sandboxboot"
	"github.com/localai-org/darkaifactory/internal/sandboxexec"
	"github.com/localai-org/darkaifactory/internal/sandboxgateway"
	"github.com/localai-org/darkaifactory/internal/version"
)

const serveCmdName = "serve"

// newServeCmd returns `daft-sandboxd serve`, the daemon-mode entry
// point that opens an outbound mTLS control channel to the
// orchestrator, registers itself with a host-resource
// advertisement, and emits heartbeats until SIGTERM / SIGINT.
//
// Slice A1 of the workflow reorientation (PLAN.md). Slice A4 will
// extend the same stream with Dispatch frames driving the existing
// Hello pipeline.
func newServeCmd() *cobra.Command {
	var (
		orchestratorURL   string
		configDir         string
		vsockDir          string
		heartbeatInterval time.Duration
	)
	cmd := &cobra.Command{
		Use:           serveCmdName,
		Short:         "Run daft-sandboxd in daemon mode (register with the orchestrator)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runServe(ctx, cmd.OutOrStdout(),
				orchestratorURL, configDir, vsockDir, heartbeatInterval)
		},
	}
	cmd.Flags().StringVar(&orchestratorURL, "orchestrator", "",
		"orchestrator endpoint URL (e.g. https://orchestrator.example:8443)")
	cmd.Flags().StringVar(&configDir, "config-dir", "", configDirFlagDesc)
	cmd.Flags().StringVar(&vsockDir, "vsock-dir", "",
		"parent directory for per-Sandbox hybrid-vsock UDS files; defaults to <os.TempDir>/daft-sandboxd-vsock")
	cmd.Flags().DurationVar(&heartbeatInterval, "heartbeat-interval",
		nodectrlclient.DefaultHeartbeatInterval,
		"how often to ping the orchestrator with a Heartbeat")
	return cmd
}

// runServe is the testable core of `daft-sandboxd serve`. ctx
// must be cancellable — runServe blocks for the lifetime of the
// daemon and exits cleanly when ctx fires.
func runServe(
	ctx context.Context,
	stdout io.Writer,
	orchestratorURL, configDirOverride, vsockDirOverride string,
	heartbeatInterval time.Duration,
) error {
	if orchestratorURL == "" {
		return errors.New("daft-sandboxd serve: --orchestrator is required")
	}
	dir, err := resolveSandboxdDir(configDirOverride)
	if err != nil {
		return err
	}
	id, err := loadOrGenerateSandboxd(dir)
	if err != nil {
		return err
	}
	peers, err := openSandboxdPeerStore(dir)
	if err != nil {
		return err
	}

	resources, err := nodectrlclient.DetectResources()
	if err != nil {
		return err
	}

	vsockDir, err := resolveVsockDir(vsockDirOverride)
	if err != nil {
		return err
	}

	bridger := sandboxgateway.New()
	attachRegistry := sandboxgateway.NewAttachRegistry()
	dispatcher, err := buildDispatcher(stdout, bridger, attachRegistry, vsockDir)
	if err != nil {
		return err
	}

	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               orchestratorURL,
		Identity:          id,
		Peers:             peers,
		Version:           version.Version,
		HeartbeatInterval: heartbeatInterval,
		SandboxEvents:     dispatcher,
		LiveSnapshot:      dispatcher.LiveSnapshot,
		Bridger:           bridger,
		AttachBridger:     attachRegistry,
		Stderr:            stdout,
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout,
		"daft-sandboxd serve: registering with %s\n"+
			"daft-sandboxd serve: fingerprint %s\n"+
			"daft-sandboxd serve: advertising %d CPUs, %s\n",
		orchestratorURL, id.Fingerprint(),
		resources.CPUs, formatBytes(resources.MemoryBytes))

	return runRegisterAndServe(ctx, client, resources)
}

// runRegisterAndServe is a single test seam — production swaps in
// the real client.RegisterAndServe, tests can swap a fake.
//
//nolint:gochecknoglobals // single test seam, not state.
var runRegisterAndServe = func(
	ctx context.Context,
	c *nodectrlclient.Client,
	res nodectrl.Resources,
) error {
	return c.RegisterAndServe(ctx, res)
}

// buildDispatcher resolves the sandbox.Config baseline from the
// DAFT_* env vars and wires an Executor with it. The Executor is
// the DispatchHandler the nodectrlclient invokes on every
// Dispatch event from the orchestrator. The production Assembler
// pulls + bundles images via the local imagestore so dispatches
// carrying a spec.image_ref actually run a container.
//
// bridger and vsockDir are the C1.3b port-ingress wiring: the
// Executor registers per-(workspace, port) DialGuestVsock closures
// on bridger as dispatches arrive, and the same bridger is shared
// with the nodectrlclient so its OpenPortStream handler finds them.
// attachRegistry is the D2b.3 counterpart for AttachStream — the
// same instance is shared with the nodectrlclient so AttachRequest
// events route through to the executor-populated dial closures.
//
//nolint:gochecknoglobals // single test seam, not state.
var buildDispatcher = func(
	stdout io.Writer,
	bridger *sandboxgateway.Bridger,
	attachRegistry *sandboxgateway.AttachRegistry,
	vsockDir string,
) (*sandboxexec.Executor, error) {
	base, err := sandbox.ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	assembler := sandboxexec.ProductionAssembler(sandboxboot.Deps{
		OpenStore: openSandboxbootStore,
		CacheDir:  userCacheDir,
		Extract:   extractWithEnvFallback(base),
	})
	return sandboxexec.New(sandboxexec.Config{
		Base:           base,
		Stdout:         stdout,
		Assembler:      assembler,
		Bridger:        bridger,
		AttachRegistry: attachRegistry,
		VsockDir:       vsockDir,
		NetEnabled:     true,
	})
}

// extractWithEnvFallback wraps bundled.Extract: when the binary
// has the artefacts embedded (Nix build) it returns those;
// otherwise (plain `go build`) it falls back to the artefact
// paths sandbox.ConfigFromEnv resolved into base. This is the
// "callers should fall back to env-var-supplied paths" pattern
// the bundled package's doc comment specifies. Returns the
// embed error untouched when neither source is available, so a
// genuinely misconfigured daemon still produces a clean
// ErrNotBundled at dispatch time.
func extractWithEnvFallback(base sandbox.Config) func(dir string) (bundled.Paths, error) {
	return func(dir string) (bundled.Paths, error) {
		paths, err := bundled.Extract(dir)
		if !errors.Is(err, bundled.ErrNotBundled) {
			return paths, err
		}
		envPaths := bundled.Paths{
			CloudHypervisor: base.CloudHypervisor,
			Kernel:          base.Kernel,
			Initramfs:       base.Initramfs,
			Virtiofsd:       base.Virtiofsd,
		}
		if envPaths.CloudHypervisor == "" || envPaths.Kernel == "" ||
			envPaths.Initramfs == "" || envPaths.Virtiofsd == "" {
			return bundled.Paths{}, err
		}
		return envPaths, nil
	}
}

// resolveVsockDir returns the absolute, mkdir'd parent directory
// for per-Sandbox hybrid-vsock UDS files. An explicit override
// must already be absolute. Empty override → a per-sandboxd-process
// subdir under os.TempDir so test parallelism and multiple daemons
// on one host don't collide on UDS paths.
func resolveVsockDir(override string) (string, error) {
	dir := override
	if dir == "" {
		dir = filepath.Join(os.TempDir(),
			fmt.Sprintf("daft-sandboxd-vsock-%d", os.Getpid()))
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("daft-sandboxd serve: --vsock-dir %q must be absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("daft-sandboxd serve: mkdir vsock dir: %w", err)
	}
	return dir, nil
}

// openSandboxbootStore is the production OpenStore — it returns
// the same imagestore handle the legacy `run` command uses, but
// typed against sandboxboot.Store so the executor doesn't have
// to import imagestore directly.
func openSandboxbootStore() (sandboxboot.Store, error) {
	return openStore()
}

// formatBytes is a small KiB/MiB/GiB renderer for the startup
// banner. Production traffic doesn't read this output, so a
// best-effort, three-significant-digit rendering is fine.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), suffix)
}
