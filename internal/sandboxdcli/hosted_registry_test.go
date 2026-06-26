package sandboxdcli

import (
	"path/filepath"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

func mustSandboxIdentity(t *testing.T) (*identity.Identity, *identity.PeerStore) {
	t.Helper()
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return id, peers
}

func TestHostedRegistryTransport(t *testing.T) {
	id, peers := mustSandboxIdentity(t)
	if rt := hostedRegistryTransport("https://127.0.0.1:8443", id, peers); rt == nil {
		t.Error("want a non-nil transport for a valid orchestrator URL")
	}
	if rt := hostedRegistryTransport("://bad", id, peers); rt != nil {
		t.Error("want a nil transport for an unparseable orchestrator URL")
	}
}

func TestOpenSandboxbootStore(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())

	// nil transport: no hosted registry wired.
	if _, err := openSandboxbootStore(nil); err != nil {
		t.Fatalf("openSandboxbootStore(nil): %v", err)
	}

	// non-nil transport exercises the UseHostedRegistry branch.
	id, peers := mustSandboxIdentity(t)
	rt := hostedRegistryTransport("https://127.0.0.1:8443", id, peers)
	if _, err := openSandboxbootStore(rt); err != nil {
		t.Fatalf("openSandboxbootStore(rt): %v", err)
	}
}
