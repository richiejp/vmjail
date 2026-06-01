package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/ociregistry"
)

// imagePushOpts gathers `daft image push` flags.
type imagePushOpts struct {
	orchestrator string
	configDir    string
	repo         string
	tag          string
}

// newImageCmd returns the `daft image` parent command. Its sole
// sub-command pushes a local image tarball into the orchestrator's
// embedded OCI registry (PLAN.md item 8), addressable as a workspace
// image without an external registry.
func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Work with images in the orchestrator's embedded registry",
	}
	cmd.AddCommand(newImagePushCmd())
	return cmd
}

func newImagePushCmd() *cobra.Command {
	var opts imagePushOpts
	cmd := &cobra.Command{
		Use:   "push <tarball>",
		Short: "Push a local OCI image tarball to the orchestrator's registry",
		Long: "Pushes a local OCI/docker image tarball (raw or gzipped, e.g. a\n" +
			"`nix build .#…-image` output) into the orchestrator's embedded\n" +
			"registry over mTLS, and prints the resulting image reference. Pass\n" +
			"that ref to `daft workspace image` to launch a workspace from it —\n" +
			"no external registry required.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImagePush(cmd.Context(), cmd.OutOrStdout(), args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().StringVar(&opts.repo, "repo", "local",
		"repository name to push under (the image ref is <host>/<repo>:<tag>)")
	cmd.Flags().StringVar(&opts.tag, "tag", "latest", "image tag to push under")
	return cmd
}

// imagePushFunc is the registry-push seam (production: ociregistry.Push),
// injected so the command's wiring is testable without a live registry.
type imagePushFunc func(tarPath, repo, tag string, rt http.RoundTripper) (string, error)

func runImagePush(ctx context.Context, stdout io.Writer, tarPath string, opts imagePushOpts) error {
	return runImagePushWith(ctx, stdout, tarPath, opts, ociregistry.Push)
}

// runImagePushWith builds the mTLS transport to the orchestrator's
// embedded registry, pushes tarPath via push, and prints the resulting
// orchestrator-hosted ref.
func runImagePushWith(_ context.Context, stdout io.Writer, tarPath string, opts imagePushOpts, push imagePushFunc) error {
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	_, id, peers, err := openClientIdentity(opts.configDir)
	if err != nil {
		return err
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return err
	}
	u, err := url.Parse(orchestrator)
	if err != nil {
		return fmt.Errorf("daft image push: parse orchestrator URL %q: %w", orchestrator, err)
	}
	ref, err := push(tarPath, opts.repo, opts.tag, ociregistry.Transport(u.Host, tlsCfg))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, ref)
	return err
}
