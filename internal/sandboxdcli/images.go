package sandboxdcli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/imagestore"
)

// Env vars for image-store configuration. Stable so external scripts
// can rely on them.
const (
	// EnvStoreDir is the directory the image store roots at. When
	// unset, derives from os.UserCacheDir + "daft-sandboxd-images".
	EnvStoreDir = "DAFT_STORE_DIR"
	// EnvStoreQuota is the byte cap on the layer cache. Accepts
	// integer bytes or a suffix-tagged value: 50G, 1T, 100M, 4K.
	// Empty or "0" disables quota enforcement.
	EnvStoreQuota = "DAFT_STORE_QUOTA"
)

// storeSubdir is the default directory under the user cache for the
// image store. Distinct from cacheSubdir so the bundled-artifact
// extract dir and the image cache don't share a parent (they have
// very different size profiles and pruning rules).
const storeSubdir = "daft-sandboxd-images"

func newImagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Manage the local OCI image cache",
		Long: "Sub-commands inspect and prune the content-addressable image\n" +
			"and bundle cache used by daft-sandboxd. Quota and root directory\n" +
			"can be set via the " + EnvStoreDir + " / " + EnvStoreQuota + " env\n" +
			"vars; default quota is 50 GiB and default root is the user\n" +
			"cache directory.",
	}
	cmd.AddCommand(newImagesLsCmd())
	cmd.AddCommand(newImagesRmCmd())
	cmd.AddCommand(newImagesPruneCmd())
	return cmd
}

func newPullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <image-ref>",
		Short: "Fetch an OCI image into the local store",
		Long: "Pull an image by ref (e.g. ghcr.io/owner/repo:tag) into the\n" +
			"shared OCI layout. LRU eviction makes room if the new image\n" +
			"would push the cache past its quota; a single image larger\n" +
			"than the quota is rejected before any download starts.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			img, err := store.Pull(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Pulled %s\n  digest: %s\n  size:   %s\n",
				img.Ref, img.Digest, humanBytes(img.Size))
			return err
		},
	}
}

func newImagesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List images in the local store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			entries, err := store.List()
			if err != nil {
				return err
			}
			return printEntries(cmd.OutOrStdout(), entries)
		},
	}
}

func newImagesRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "rm <ref-or-digest>",
		Short:         "Remove an image from the local store",
		Args:          cobra.MinimumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			entries, err := store.List()
			if err != nil {
				return err
			}
			for _, target := range args {
				digest, ok := resolveTarget(entries, target)
				if !ok {
					return fmt.Errorf("daft-sandboxd: no image matching %q", target)
				}
				if err := store.Remove(digest); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", digest); err != nil {
					return fmt.Errorf("daft-sandboxd: write rm result: %w", err)
				}
			}
			return nil
		},
	}
}

func newImagesPruneCmd() *cobra.Command {
	var olderThan time.Duration
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove images older than --older-than",
		Long: "Delete every image whose last-used timestamp is older than\n" +
			"the given duration. Locked images (a VM is currently using the\n" +
			"bundle) are skipped.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if olderThan <= 0 {
				return fmt.Errorf("daft-sandboxd: --older-than must be > 0")
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			pruned, err := store.Prune(olderThan)
			if err != nil {
				return err
			}
			for _, d := range pruned {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Pruned %s\n", d); err != nil {
					return fmt.Errorf("daft-sandboxd: write prune result: %w", err)
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Pruned %d image(s)\n", len(pruned))
			return err
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0,
		"prune images last used more than this duration ago (e.g. 168h, 7d-equivalents)")
	return cmd
}

// openStore opens the image store rooted at the configured directory
// with the configured quota. Used by every images-* subcommand.
func openStore() (*imagestore.Store, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	quota, err := storeQuota()
	if err != nil {
		return nil, err
	}
	return imagestore.New(root, quota)
}

func storeRoot() (string, error) {
	if v := os.Getenv(EnvStoreDir); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("daft-sandboxd: %s %q must be absolute", EnvStoreDir, v)
		}
		return v, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("daft-sandboxd: locate cache dir: %w", err)
	}
	return filepath.Join(base, storeSubdir), nil
}

func storeQuota() (int64, error) {
	v := os.Getenv(EnvStoreQuota)
	if v == "" {
		return imagestore.DefaultQuota, nil
	}
	return parseBytes(v)
}

// parseBytes accepts plain integer bytes or a value with a single
// K/M/G/T suffix (binary, 1024-based — what disk usage tools use).
// "0" returns 0 (quota disabled).
func parseBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	last := s[len(s)-1]
	mult := int64(1)
	num := s
	switch last {
	case 'K', 'k':
		mult = 1 << 10
		num = s[:len(s)-1]
	case 'M', 'm':
		mult = 1 << 20
		num = s[:len(s)-1]
	case 'G', 'g':
		mult = 1 << 30
		num = s[:len(s)-1]
	case 'T', 't':
		mult = 1 << 40
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("daft-sandboxd: invalid byte size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("daft-sandboxd: byte size %q is negative", s)
	}
	return n * mult, nil
}

// resolveTarget matches a user-supplied "ref-or-digest" against a
// list of entries. Tries: exact digest match, then ref-prefix match,
// then digest-prefix match. Ambiguity is silently resolved to the
// first match — the caller can iterate with explicit digests if they
// need precision.
func resolveTarget(entries []imagestore.Entry, target string) (string, bool) {
	for _, e := range entries {
		if e.Digest == target {
			return e.Digest, true
		}
		if slices.Contains(e.Refs, target) {
			return e.Digest, true
		}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Digest, target) {
			return e.Digest, true
		}
	}
	return "", false
}

func printEntries(w io.Writer, entries []imagestore.Entry) error {
	if len(entries) == 0 {
		_, err := fmt.Fprintln(w, "(empty store)")
		return err
	}
	for _, e := range entries {
		ref := "(orphaned)"
		if len(e.Refs) > 0 {
			ref = e.Refs[0]
		}
		flag := ""
		if e.Locked {
			flag = " [locked]"
		}
		_, err := fmt.Fprintf(
			w,
			"%s\t%s\t%s\t%s%s\n",
			shortDigest(e.Digest), ref,
			humanBytes(e.Size),
			e.LastUsed.Format(time.RFC3339),
			flag,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func shortDigest(d string) string {
	const want = 19 // sha256:abcdef012... (sha256: + 12 hex chars)
	if len(d) <= want {
		return d
	}
	return d[:want]
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sB", float64(n)/float64(div), "KMGTP"[exp:exp+1])
}
