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

// IsZero reports whether f is the zero Fingerprint — the value that
// proto-decoded fields take when unset, or that callers can use to
// mean "no owner / no lock holder". Distinguishing this from a real
// fingerprint matters at every projection boundary; callers should
// not String() the zero value (the result would be a valid-looking
// hash of nothing).
func (f Fingerprint) IsZero() bool {
	return f == Fingerprint{}
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

// abbrevBytes is how many leading fingerprint bytes Abbrev renders.
// Four bytes (8 hex chars) is enough to be unique in any realistic
// single-orchestrator fleet while staying glanceable in a table.
const abbrevBytes = 4

// Abbrev renders a short, human-glanceable prefix of the fingerprint —
// "AB:CD:EF:01…" — for listings where the full 70-char canonical form
// is too noisy. The rendered hex is a valid prefix to type back into a
// prefix-accepting selector (see HasHexPrefix); the trailing ellipsis
// is display-only and is not part of that prefix.
func (f Fingerprint) Abbrev() string {
	var sb strings.Builder
	for i := 0; i < abbrevBytes && i < len(f); i++ {
		if i > 0 {
			sb.WriteByte(':')
		}
		fmt.Fprintf(&sb, "%02X", f[i])
	}
	sb.WriteString("…")
	return sb.String()
}

// normalizeFingerprintHex lowercases s and strips the sha256:/SHA256:
// algorithm tag, all colons, and surrounding space — reducing any
// accepted fingerprint spelling (canonical, bare hex, abbreviated) to a
// plain lowercase hex run for prefix comparison.
func normalizeFingerprintHex(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "sha256:"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "SHA256:"); ok {
		s = rest
	}
	return strings.ToLower(strings.ReplaceAll(s, ":", ""))
}

// HasHexPrefix reports whether prefix — an abbreviated fingerprint in
// any accepted spelling (optional sha256: tag, optional colons, any
// case) — is a prefix of f's canonical hex. An empty or all-separator
// prefix matches nothing, so it never silently selects an arbitrary
// node. A non-hex prefix (e.g. a hostname) simply never matches.
func (f Fingerprint) HasHexPrefix(prefix string) bool {
	norm := normalizeFingerprintHex(prefix)
	if norm == "" {
		return false
	}
	return strings.HasPrefix(normalizeFingerprintHex(f.String()), norm)
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
