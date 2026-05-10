package identity

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// ErrUnknownPeer is what VerifyPeerByFingerprint returns when the
// presented cert doesn't match any approved fingerprint in the store.
// Tests and request-time loggers can use errors.Is on it.
var ErrUnknownPeer = errors.New("identity: peer fingerprint not approved")

// ErrNoPeerCert is returned when the TLS handshake somehow produced
// no peer cert at all (a misconfigured ClientAuth mode, or a future
// transport that bypasses the leaf cert path).
var ErrNoPeerCert = errors.New("identity: no peer cert in handshake")

// ServerTLSConfig builds a *tls.Config for an mTLS server. Every
// inbound connection must present a client cert; the leaf cert's
// SPKI fingerprint is looked up in store, and unknown peers are
// rejected at handshake time. The same template suits the client
// side too — see ClientTLSConfig.
func ServerTLSConfig(id *Identity, store *PeerStore) (*tls.Config, error) {
	cfg, err := baseTLSConfig(id, store)
	if err != nil {
		return nil, err
	}
	cfg.ClientAuth = tls.RequireAnyClientCert
	return cfg, nil
}

// ClientTLSConfig is the dial side of the same trust model: the
// client offers id.Cert and verifies the server's leaf-cert
// fingerprint against the same PeerStore. InsecureSkipVerify is set
// because we never use a CA — VerifyPeerCertificate is the trust
// authority. If serverNameOverride is non-empty it lands in
// tls.Config.ServerName so SNI / hostname-routing still works on
// the wire even though it doesn't gate trust.
func ClientTLSConfig(id *Identity, store *PeerStore, serverNameOverride string) (*tls.Config, error) {
	cfg, err := baseTLSConfig(id, store)
	if err != nil {
		return nil, err
	}
	// We bypass the standard library's chain verification because
	// our trust anchor is the per-peer fingerprint, not a CA. The
	// VerifyPeerCertificate callback wired in baseTLSConfig is the
	// only thing that decides whether to proceed.
	cfg.InsecureSkipVerify = true //nolint:gosec // see comment above
	cfg.ServerName = serverNameOverride
	return cfg, nil
}

func baseTLSConfig(id *Identity, store *PeerStore) (*tls.Config, error) {
	if id == nil {
		return nil, errors.New("identity: nil identity")
	}
	if store == nil {
		return nil, errors.New("identity: nil peer store")
	}
	if id.Cert == nil || id.Key == nil || len(id.DER) == 0 {
		return nil, errors.New("identity: incomplete identity")
	}
	cert := tls.Certificate{
		Certificate: [][]byte{id.DER},
		PrivateKey:  id.Key,
		Leaf:        id.Cert,
	}
	verify := makeVerifier(store)
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: verify,
		// Session resumption skips VerifyPeerCertificate. We resync
		// the trust check on every connection by re-running it via
		// VerifyConnection (gosec G123). For belt-and-braces, also
		// disable session tickets so resumption is not an option.
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			rawCerts := make([][]byte, 0, len(cs.PeerCertificates))
			for _, c := range cs.PeerCertificates {
				rawCerts = append(rawCerts, c.Raw)
			}
			return verify(rawCerts, nil)
		},
	}, nil
}

// makeVerifier returns a tls.Config.VerifyPeerCertificate callback
// closed over store. The first raw cert is the peer's leaf; we hash
// its SPKI and look it up. Verified chains (the second arg) are
// always empty when InsecureSkipVerify is set or the server is
// running with RequireAnyClientCert, so we can't rely on them.
func makeVerifier(store *PeerStore) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCert
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("identity: parse peer cert: %w", err)
		}
		fp := FingerprintCert(leaf)
		if _, ok := store.Lookup(fp); !ok {
			return fmt.Errorf("%w: %s", ErrUnknownPeer, fp)
		}
		return nil
	}
}
