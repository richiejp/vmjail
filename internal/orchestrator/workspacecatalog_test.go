package orchestrator

import (
	"path/filepath"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// TestServerExposesWorkspaceCatalog mirrors
// TestServerExposesSandboxRegistry — a configured catalog flows
// through New and is returned by Server.WorkspaceCatalog().
func TestServerExposesWorkspaceCatalog(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForWorkspaceCatalog(t)
	cat := workspacecatalog.NewStore(nil)
	srv, err := New(Config{
		Identity:         srvID,
		Peers:            store,
		WorkspaceCatalog: cat,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.WorkspaceCatalog() != cat {
		t.Error("WorkspaceCatalog() did not return the configured catalog")
	}
}

// TestServerCreatesDefaultWorkspaceCatalog verifies New mints a
// default catalog when Config.WorkspaceCatalog is nil.
func TestServerCreatesDefaultWorkspaceCatalog(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForWorkspaceCatalog(t)
	srv, err := New(Config{Identity: srvID, Peers: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.WorkspaceCatalog() == nil {
		t.Error("WorkspaceCatalog() = nil, want default")
	}
}

func mustOpenStoreForWorkspaceCatalog(t *testing.T) *identity.PeerStore {
	t.Helper()
	s, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return s
}
