//go:build linux && integration

package testdaemons

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// NixOfficialImageRef is the upstream nixos/nix image the D6c.3
// integration tests pull from. Pinned to a digest so the test is
// deterministic across runs; bump by re-resolving
// `docker.io/nixos/nix:latest` and copying the digest here.
const NixOfficialImageRef = "docker.io/nixos/nix@" +
	"sha256:bf1d938835ab96312f098fa6c2e9cab367728e0aad0646ee3e02a787c80d8fb8"

// nixOfficialCacheSubdir is the per-user cache directory the
// pulled tarball lives under. Lives below os.UserCacheDir so a
// `make clean` (which wipes bin/) doesn't blow away ~100 MB of
// pulled image data and force every contributor to redownload.
const nixOfficialCacheSubdir = "daft-test-images"

// FetchNixOfficialImage returns the local path to a docker-format
// tarball of NixOfficialImageRef. On a cache hit it returns
// immediately; on a miss it pulls from the upstream registry and
// writes the tarball to <userCacheDir>/daft-test-images/. Pulls
// are about ~100 MB so the first call on a fresh machine is the
// expensive one; subsequent calls (including across test runs
// and across `make clean` cycles) reuse the cached file.
//
// Network is only required when the cache is empty. Pre-commit
// runs after the first successful pull don't touch docker.io.
func FetchNixOfficialImage(t *testing.T) string {
	t.Helper()
	cacheDir, err := nixOfficialCacheDir()
	if err != nil {
		t.Fatalf("testdaemons: resolve cache dir: %v", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("testdaemons: mkdir cache dir %s: %v", cacheDir, err)
	}
	cachePath := filepath.Join(cacheDir, nixOfficialCacheFile())
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("testdaemons: stat cache %s: %v", cachePath, err)
	}

	ref, err := name.ParseReference(NixOfficialImageRef)
	if err != nil {
		t.Fatalf("testdaemons: parse %s: %v", NixOfficialImageRef, err)
	}
	img, err := crane.Pull(NixOfficialImageRef)
	if err != nil {
		t.Fatalf("testdaemons: pull %s (need network on first run): %v",
			NixOfficialImageRef, err)
	}
	if err := writeImageAtomically(cachePath, ref, img); err != nil {
		t.Fatalf("testdaemons: cache %s: %v", cachePath, err)
	}
	return cachePath
}

// nixOfficialCacheDir resolves the on-disk cache directory.
// Pulled out so a test that wants to override the location can
// set XDG_CACHE_HOME / HOME without monkey-patching the package.
func nixOfficialCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, nixOfficialCacheSubdir), nil
}

// nixOfficialCacheFile derives a stable filename from the pinned
// digest so bumping NixOfficialImageRef invalidates the cache
// without manual cleanup.
func nixOfficialCacheFile() string {
	_, digest, ok := splitDigest(NixOfficialImageRef)
	if !ok {
		return "nixos-nix.tar"
	}
	return fmt.Sprintf("nixos-nix-%s.tar", digest)
}

// splitDigest tears an `<repo>@sha256:<hex>` reference apart so
// the cache filename only carries the short digest. Returns
// ok=false on any malformed input — callers fall back to a
// constant name.
func splitDigest(ref string) (repo, digest string, ok bool) {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '@' {
			repo = ref[:i]
			suffix := ref[i+1:]
			const prefix = "sha256:"
			if len(suffix) <= len(prefix) || suffix[:len(prefix)] != prefix {
				return "", "", false
			}
			return repo, suffix[len(prefix):], true
		}
	}
	return "", "", false
}

// writeImageAtomically lands the tarball at dst via a sibling
// .tmp file + rename. A concurrent test process that finds a
// partially-written file would otherwise try to load garbage
// from crane.Load and fail in a confusing way.
func writeImageAtomically(dst string, ref name.Reference, img v1.Image) error {
	tmp := dst + ".tmp"
	//nolint:gosec // tmp is sibling to dst; both under the per-user cache dir.
	if err := tarball.WriteToFile(tmp, ref, img); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
