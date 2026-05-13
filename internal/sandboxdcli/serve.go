package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
	"github.com/richiejp/darkaifactory/internal/sandboxctrlclient"
	"github.com/richiejp/darkaifactory/internal/version"
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
				orchestratorURL, configDir, heartbeatInterval)
		},
	}
	cmd.Flags().StringVar(&orchestratorURL, "orchestrator", "",
		"orchestrator endpoint URL (e.g. https://orchestrator.example:8443)")
	cmd.Flags().StringVar(&configDir, "config-dir", "", configDirFlagDesc)
	cmd.Flags().DurationVar(&heartbeatInterval, "heartbeat-interval",
		sandboxctrlclient.DefaultHeartbeatInterval,
		"how often to ping the orchestrator with a Heartbeat")
	return cmd
}

// runServe is the testable core of `daft-sandboxd serve`. ctx
// must be cancellable — runServe blocks for the lifetime of the
// daemon and exits cleanly when ctx fires.
func runServe(
	ctx context.Context,
	stdout io.Writer,
	orchestratorURL, configDirOverride string,
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

	resources, err := sandboxctrlclient.DetectResources()
	if err != nil {
		return err
	}

	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:               orchestratorURL,
		Identity:          id,
		Peers:             peers,
		Version:           version.Version,
		HeartbeatInterval: heartbeatInterval,
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
	c *sandboxctrlclient.Client,
	res sandboxctrl.Resources,
) error {
	return c.RegisterAndServe(ctx, res)
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
