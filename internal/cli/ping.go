package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"

	healthv1 "github.com/richiejp/darkaifactory/internal/genpb/health/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// pingCmdName is the verb users invoke. Surfaced as a constant so
// tests reference the same string the command tree registers.
const pingCmdName = "ping"

// newPingCmd: `daft ping <addr>` — round-trip a Health.Ping RPC at
// the given address using the local mTLS identity and approved
// peer store. The smallest end-to-end demonstration that the trust
// loop works.
func newPingCmd() *cobra.Command {
	var (
		message   string
		timeout   time.Duration
		configDir string
	)
	cmd := &cobra.Command{
		Use:   pingCmdName + " <addr>",
		Short: "Round-trip a Health.Ping over mTLS to <addr>",
		Long: "Dial <addr> over HTTP/2 + mTLS, authenticate using the local\n" +
			"daft client identity, verify the server's leaf-cert\n" +
			"fingerprint against the approved-peer set, and call\n" +
			"daft.health.v1.HealthService.Ping. Prints the round-tripped\n" +
			"message and exits 0 on success.\n\n" +
			"The remote orchestrator's fingerprint must already have been\n" +
			"approved with `daft auth approve <fingerprint>`, and the\n" +
			"orchestrator must reciprocally have approved this client's\n" +
			"fingerprint (see `daft auth show`).",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runPing(ctx, cmd.OutOrStdout(), args[0], message, configDir)
		},
	}
	cmd.Flags().StringVar(&message, "message", "ping",
		"message to round-trip through Health.Ping")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second,
		"overall timeout for the RPC")
	cmd.Flags().StringVar(&configDir, "config-dir", "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	return cmd
}

// runPing is the testable core of `daft ping`. Split out so tests
// can call it with a fake stdout and an httptest-driven address.
//
// A Tracer can be attached to ctx via trace.ContextWithTracer; tests
// use this to inspect spans without shipping them. When no tracer is
// on ctx, runPing creates a local one and ships its buffer to the
// orchestrator on success.
func runPing(ctx context.Context, stdout io.Writer, addr, message, configDirOverride string) error {
	tracer, owned := tracerForCommand(ctx)
	ctx, span := tracer.Start(
		ctx, "daft.ping",
		trace.WithAttributes(trace.KV{Key: "rpc.addr", Value: addr}),
	)
	defer span.End()

	_, id, peers, err := openClientIdentity(configDirOverride)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return trace.RecordError(ctx, err)
	}

	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	interceptor := connect.WithInterceptors(tracer.ClientInterceptor())
	client := healthv1connect.NewHealthServiceClient(
		httpc, "https://"+addr, interceptor,
	)

	resp, err := client.Ping(ctx, connect.NewRequest(&healthv1.PingRequest{
		Message: message,
	}))
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft ping: %w", err))
	}
	if _, err := fmt.Fprintf(stdout, "pong: %s\n", resp.Msg.GetMessage()); err != nil {
		return trace.RecordError(ctx, err)
	}
	if owned {
		shipTraces(ctx, tracer, httpc, addr, "daft ping")
	}
	return nil
}
