package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/trace"
)

// nodeListSubName is the verb under `daft node`. Promoted to a
// constant so tests reference the same string the command tree
// registers.
const nodeListSubName = "list"

// newNodeListCmd returns `daft node list --orchestrator <url>`:
// dial the orchestrator over mTLS and print the snapshot returned by
// NodeControlService.ListNodes.
func newNodeListCmd() *cobra.Command {
	var (
		orchestrator string
		timeout      time.Duration
		configDir    string
	)
	cmd := &cobra.Command{
		Use:   nodeListSubName,
		Short: "List Nodes currently registered with the orchestrator",
		Long: "Dial the orchestrator over HTTP/2 + mTLS using the local daft\n" +
			"client identity and call NodeControlService.ListNodes.\n" +
			"Prints one row per live registration: fingerprint, version, CPUs,\n" +
			"memory, and last-seen age.\n\n" +
			"The orchestrator's fingerprint must already be approved with\n" +
			"`daft auth approve <fingerprint>`.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if orchestrator == "" {
				return fmt.Errorf("daft node list: --orchestrator is required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runNodeList(ctx, cmd.OutOrStdout(), orchestrator, configDir, time.Now)
		},
	}
	cmd.Flags().StringVar(&orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port) to query")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second,
		"overall timeout for the RPC")
	cmd.Flags().StringVar(&configDir, "config-dir", "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	return cmd
}

// runNodeList is the testable core. now is injected so test output
// is deterministic for the "last seen" column.
func runNodeList(
	ctx context.Context,
	stdout io.Writer,
	orchestrator, configDirOverride string,
	now func() time.Time,
) error {
	tracer, owned := tracerForCommand(ctx)
	ctx, span := tracer.Start(
		ctx, "daft.node.list",
		trace.WithAttributes(trace.KV{Key: "rpc.addr", Value: orchestrator}),
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
	client := nodectrlv1connect.NewNodeControlServiceClient(
		httpc, orchestrator, interceptor,
	)

	resp, err := client.ListNodes(ctx, connect.NewRequest(&nodectrlv1.ListNodesRequest{}))
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft node list: %w", err))
	}

	if err := writeNodeList(stdout, resp.Msg.GetNodes(), now()); err != nil {
		return trace.RecordError(ctx, err)
	}
	if owned {
		// Strip the scheme so the trace exporter can reach the same
		// host:port as the Connect client.
		shipTraces(ctx, tracer, httpc, trimURLScheme(orchestrator), "daft node list")
	}
	return nil
}

// writeNodeList renders the snapshot in a tab-aligned table.
// Kept separate from the RPC plumbing so it's trivial to test with
// hand-built protos.
func writeNodeList(w io.Writer, nodes []*nodectrlv1.Node, refNow time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "FINGERPRINT\tVERSION\tCPUS\tMEMORY\tLAST SEEN"); err != nil {
		return err
	}
	for _, s := range nodes {
		lastSeen := s.GetLastSeenAt().AsTime()
		age := "—"
		if !lastSeen.IsZero() {
			age = formatAge(refNow.Sub(lastSeen))
		}
		version := s.GetVersion()
		if version == "" {
			version = "—"
		}
		if _, err := fmt.Fprintf(
			tw, "%s\t%s\t%d\t%s\t%s\n",
			s.GetFingerprint(),
			version,
			s.GetResources().GetCpus(),
			formatBytes(s.GetResources().GetMemoryBytes()),
			age,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// formatBytes prints a SI-suffix-style size (MiB / GiB). Local to
// this file so the CLI doesn't depend on internal/sandboxdcli.
func formatBytes(n uint64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// formatAge renders a sub-day duration as "<N>s" / "<N>m<S>s"; older
// than a day collapses to "<N>d".
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// trimURLScheme strips a leading "https://" / "http://" so trace
// shipping can reuse the host:port. Inputs without a scheme pass
// through unchanged.
func trimURLScheme(u string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(u) >= len(prefix) && u[:len(prefix)] == prefix {
			return u[len(prefix):]
		}
	}
	return u
}
