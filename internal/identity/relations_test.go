package identity

import "testing"

func TestArchitectureTrustRelations(t *testing.T) {
	store := NewMemoryPeerStore()
	orchestrator, err := Generate(RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate orchestrator: %v", err)
	}
	client, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate client: %v", err)
	}

	if err := store.TrustOrchestrator(orchestrator.Fingerprint(), "controller"); err != nil {
		t.Fatalf("TrustOrchestrator: %v", err)
	}
	assertRelation(t, store, orchestrator.Fingerprint(), RoleOrchestrator, "controller")
	if err := store.RevokeOrchestratorTrust(orchestrator.Fingerprint()); err != nil {
		t.Fatalf("RevokeOrchestratorTrust: %v", err)
	}
	if _, ok := store.Lookup(orchestrator.Fingerprint()); ok {
		t.Fatal("orchestrator trust remains after revocation")
	}

	if err := store.AuthoriseClient(client.Fingerprint(), "developer"); err != nil {
		t.Fatalf("AuthoriseClient: %v", err)
	}
	assertRelation(t, store, client.Fingerprint(), RoleClient, "developer")
	if err := store.RevokeClientAuthorisation(client.Fingerprint()); err != nil {
		t.Fatalf("RevokeClientAuthorisation: %v", err)
	}
	if _, ok := store.Lookup(client.Fingerprint()); ok {
		t.Fatal("client authorization remains after revocation")
	}
}

func assertRelation(t *testing.T, store *PeerStore, fingerprint Fingerprint, role Role, label string) {
	t.Helper()
	peer, ok := store.Lookup(fingerprint)
	if !ok || peer.Role != role || peer.Label != label {
		t.Fatalf("relation = %#v, present=%t; want role=%q label=%q", peer, ok, role, label)
	}
}
