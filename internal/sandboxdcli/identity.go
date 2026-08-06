package sandboxdcli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// sandboxdConfigSubdir is appended to the user's config dir for the
// sandboxd identity store. Sibling to "daft/client" and
// "daft/orchestrator" so a single developer machine can host all
// three independently.
const sandboxdConfigSubdir = "daft/sandboxd"

// peersFilename matches the file name used by the daft client and
// the orchestrator so a single mental model covers all three.
const peersFilename = "peers.json"

func resolveSandboxdDir(override string) (string, error) {
	if override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("daft-sandboxd: --config-dir %q must be absolute", override)
		}
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("daft-sandboxd: locate config dir: %w", err)
	}
	return filepath.Join(base, sandboxdConfigSubdir), nil
}

func loadOrGenerateSandboxd(dir string) (*identity.Identity, error) {
	return identity.LoadOrGenerate(dir, identity.RoleSandboxHost)
}

func openSandboxdPeerStore(dir string) (*identity.PeerStore, error) {
	return identity.OpenPeerStore(filepath.Join(dir, peersFilename))
}
