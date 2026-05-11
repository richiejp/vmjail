// Package identity owns peer identity for daft's mTLS mesh:
// per-process Ed25519 self-signed certs, content-addressed fingerprints,
// and the TOFU peer store that records who has been approved.
package identity

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Fingerprint is the SHA-256 digest of a peer cert's
// SubjectPublicKeyInfo (SPKI). SPKI rather than the full DER cert so
// that a peer rotating its cert (with the same key) keeps the same
// fingerprint and stays approved across rotations.
type Fingerprint [sha256.Size]byte

// ErrInvalidFingerprint is the sentinel returned by ParseFingerprint
// for any malformed input.
var ErrInvalidFingerprint = errors.New("identity: invalid fingerprint")

// PeerFingerprintFromTLS extracts the leaf-cert SPKI fingerprint
// from an mTLS connection state. Returns false if state is nil or
// no peer cert was presented — callers that require identity
// must reject in that case rather than substitute a default.
func PeerFingerprintFromTLS(state *tls.ConnectionState) (Fingerprint, bool) {
	if state == nil || len(state.PeerCertificates) == 0 {
		return Fingerprint{}, false
	}
	return FingerprintCert(state.PeerCertificates[0]), true
}

// FingerprintCert hashes cert.RawSubjectPublicKeyInfo. The cert must
// have been parsed (or freshly created) so RawSubjectPublicKeyInfo
// is populated; an empty SPKI yields the SHA-256 of the empty string,
// which would never match a real peer.
func FingerprintCert(cert *x509.Certificate) Fingerprint {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// String renders the fingerprint as "sha256:AA:BB:CC:..." — uppercase
// hex bytes separated by colons. The "sha256:" prefix is mandatory in
// our wire format so that future digest changes can coexist (e.g.
// "sha256-spki:..." or "blake3:...").
func (f Fingerprint) String() string {
	var sb strings.Builder
	// "sha256:" + 32*2 hex chars + 31 colons = 70 bytes.
	sb.Grow(70)
	sb.WriteString("sha256:")
	for i, b := range f {
		if i > 0 {
			sb.WriteByte(':')
		}
		fmt.Fprintf(&sb, "%02X", b)
	}
	return sb.String()
}

// ParseFingerprint accepts our canonical "sha256:AA:BB:..." form, the
// same hex without colons, and either case. The "sha256:" prefix is
// optional for ergonomics — if a user pastes a bare hex string from
// `openssl dgst -sha256` it should still work.
func ParseFingerprint(s string) (Fingerprint, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "sha256:"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "SHA256:"); ok {
		s = rest
	}
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 2*sha256.Size {
		return Fingerprint{}, fmt.Errorf("%w: want %d hex chars, got %d",
			ErrInvalidFingerprint, 2*sha256.Size, len(s))
	}
	var fp Fingerprint
	if _, err := hex.Decode(fp[:], []byte(s)); err != nil {
		return Fingerprint{}, fmt.Errorf("%w: %w", ErrInvalidFingerprint, err)
	}
	return fp, nil
}

// MarshalText emits the canonical sha256:... form. Implementing
// encoding.TextMarshaler lets Fingerprint be used directly as JSON
// map keys and JSON struct values without a custom wrapper.
func (f Fingerprint) MarshalText() ([]byte, error) {
	return []byte(f.String()), nil
}

// UnmarshalText accepts whatever ParseFingerprint accepts.
func (f *Fingerprint) UnmarshalText(b []byte) error {
	parsed, err := ParseFingerprint(string(b))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}
