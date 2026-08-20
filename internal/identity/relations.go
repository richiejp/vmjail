package identity

// TrustOrchestrator records the exact controller identity trusted by a Client.
// It is the implementation operation named by the Client architecture model;
// PeerStore remains the generic persistence mechanism beneath that model.
func (s *PeerStore) TrustOrchestrator(fingerprint Fingerprint, label string) error {
	return s.Add(Peer{Fingerprint: fingerprint, Label: label, Role: RoleOrchestrator})
}

// RevokeOrchestratorTrust removes a controller identity from a Client's trust
// store. The operation is idempotent because revocation must be safely retryable.
func (s *PeerStore) RevokeOrchestratorTrust(fingerprint Fingerprint) error {
	return s.Remove(fingerprint)
}

// AuthoriseClient grants the Client role to an exact identity in the
// Orchestrator's authorization store.
func (s *PeerStore) AuthoriseClient(fingerprint Fingerprint, label string) error {
	return s.Add(Peer{Fingerprint: fingerprint, Label: label, Role: RoleClient})
}

// RevokeClientAuthorisation removes a Client identity from the Orchestrator's
// authorization store. The operation is idempotent for bounded retries.
func (s *PeerStore) RevokeClientAuthorisation(fingerprint Fingerprint) error {
	return s.Remove(fingerprint)
}
