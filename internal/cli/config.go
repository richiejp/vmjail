package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/localai-org/darkaifactory/internal/identity"
)

// clientConfigSubdir is appended to the user's config dir to derive
// the daft client's identity directory. Mirrors orchestratorcli's
// "daft/orchestrator" convention so a developer running both has
// distinct, side-by-side identity stores.
const clientConfigSubdir = "daft/client"

// peersFilename is the on-disk filename for the TOFU peer store,
// living next to cert.pem and key.pem.
const peersFilename = "peers.json"

// resolveClientDir returns the directory the daft client stores its
// identity + peer store in. Order: explicit override (typically a
// --config-dir flag), then XDG-aware os.UserConfigDir + the
// "daft/client" subdir.
func resolveClientDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("daft: locate config dir: %w", err)
	}
	return filepath.Join(base, clientConfigSubdir), nil
}

// loadOrGenerateClient is a thin wrapper around
// identity.LoadOrGenerate fixing the role to RoleClient.
func loadOrGenerateClient(dir string) (*identity.Identity, error) {
	return identity.LoadOrGenerate(dir, identity.RoleClient)
}

// openClientPeerStore opens the peer store sitting under dir. The
// file may not yet exist — OpenPeerStore tolerates that and returns
// an empty store.
func openClientPeerStore(dir string) (*identity.PeerStore, error) {
	return identity.OpenPeerStore(filepath.Join(dir, peersFilename))
}

// openClientIdentity resolves the config dir, loads or generates the
// client identity, and opens the TOFU peer store. Returned for the
// subcommands that don't need a CAS store (attach, ping); see
// openClientCtx in workspace.go for the +CAS variant.
func openClientIdentity(configDirOverride string) (dir string, id *identity.Identity, peers *identity.PeerStore, err error) {
	dir, err = resolveClientDir(configDirOverride)
	if err != nil {
		return "", nil, nil, err
	}
	id, err = loadOrGenerateClient(dir)
	if err != nil {
		return "", nil, nil, err
	}
	peers, err = openClientPeerStore(dir)
	if err != nil {
		return "", nil, nil, err
	}
	return dir, id, peers, nil
}
