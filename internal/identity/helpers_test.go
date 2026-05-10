package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
)

// generateRSAKey returns a small RSA key for tests that need a
// non-Ed25519 fixture. 1024-bit is fine — these keys never escape the
// test process. (gosec G401 — test-only.)
func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	// #nosec G403 — test-only RSA key, never used outside the process.
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}
