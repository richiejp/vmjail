package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// Role labels which kind of node owns this identity. The role lands
// in the cert's CommonName so a glance at the cert tells you whether
// it's a client, an orchestrator, or a sandbox-host.
type Role string

const (
	RoleClient       Role = "client"
	RoleOrchestrator Role = "orchestrator"
	RoleSandboxHost  Role = "sandbox-host"
)

// Valid reports whether r is one of the recognised roles. Unknown
// roles are rejected by Generate to avoid typos silently shipping
// "daft-cleint-..." certs to peers that the orchestrator can never
// classify.
func (r Role) Valid() bool {
	switch r {
	case RoleClient, RoleOrchestrator, RoleSandboxHost:
		return true
	}
	return false
}

// Identity bundles the long-lived private key with its self-signed
// leaf cert. The DER form is kept alongside the parsed Cert so callers
// can hand it to tls.Certificate without re-marshalling.
type Identity struct {
	Cert *x509.Certificate
	Key  ed25519.PrivateKey
	DER  []byte
}

// certValidity is intentionally long: these are TOFU-pinned self-signed
// certs, not chained from a CA. Rotation is a fingerprint-store
// operation, not a CA operation; until we wire up the CA model
// (deferred per PLAN.md), expiry is not part of the trust decision.
const certValidity = 10 * 365 * 24 * time.Hour

// Generate creates a fresh identity for the given role. The CN is
// "daft-<role>-<8 random hex>" so two orchestrators on the same
// machine don't collide. ExtKeyUsage covers both client and server
// auth because every node both dials and accepts mTLS.
func Generate(role Role) (*Identity, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("identity: unknown role %q", role)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("identity: generate serial: %w", err)
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, fmt.Errorf("identity: generate cn suffix: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("daft-%s-%x", role, suffix),
		},
		NotBefore: now,
		NotAfter:  now.Add(certValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("identity: create cert: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("identity: parse cert: %w", err)
	}

	return &Identity{Cert: cert, Key: priv, DER: der}, nil
}

const (
	certFilename = "cert.pem"
	keyFilename  = "key.pem"
	dirPerm      = 0o700
	filePerm     = 0o600

	pemTypeCert = "CERTIFICATE"
	pemTypeKey  = "PRIVATE KEY"
)

// Save writes the identity to dir/cert.pem + dir/key.pem. The
// directory is created with 0o700; files are 0o600. Cert is also
// 0o600 because the directory containing the key is private to the
// user — there is no use case for world-readable cert files in this
// project.
func Save(dir string, id *Identity) error {
	if id == nil {
		return errors.New("identity: nil identity")
	}
	if id.Cert == nil || id.Key == nil || len(id.DER) == 0 {
		return errors.New("identity: incomplete identity")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("identity: mkdir %s: %w", dir, err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(id.Key)
	if err != nil {
		return fmt.Errorf("identity: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: keyDER})
	if err := writePrivateFile(filepath.Join(dir, keyFilename), keyPEM); err != nil {
		return fmt.Errorf("identity: write key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: id.DER})
	if err := writePrivateFile(filepath.Join(dir, certFilename), certPEM); err != nil {
		return fmt.Errorf("identity: write cert: %w", err)
	}
	return nil
}

// writePrivateFile writes data atomically with 0o600 perms via a
// .tmp sibling + Rename. If the rename fails the partial .tmp is
// removed; an unexpected unlink error there aborts the process via
// oops.Skip's allowlist (the only benign case is "tmp already gone").
func writePrivateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		oops.Skip("identity: remove partial tmp file",
			os.Remove(tmp), os.ErrNotExist)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// Load reads a Save-d directory back into an Identity. A missing
// cert.pem returns os.ErrNotExist (callers can use this to drive a
// "generate-on-first-run" flow); a corrupt PEM or key/cert mismatch
// returns a wrapped error.
func Load(dir string) (*Identity, error) {
	certPath := filepath.Join(dir, certFilename)
	// #nosec G304 — caller-supplied identity dir.
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("identity: read cert: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != pemTypeCert {
		return nil, errors.New("identity: cert.pem is not a CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse cert: %w", err)
	}

	keyPath := filepath.Join(dir, keyFilename)
	// #nosec G304 — caller-supplied identity dir.
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("identity: read key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != pemTypeKey {
		return nil, errors.New("identity: key.pem is not a PRIVATE KEY block")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse key: %w", err)
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: key is %T, want ed25519.PrivateKey", keyAny)
	}

	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("identity: cert public key is %T, want ed25519.PublicKey", cert.PublicKey)
	}
	if !pub.Equal(key.Public()) {
		return nil, errors.New("identity: cert public key does not match private key")
	}

	return &Identity{Cert: cert, Key: key, DER: certBlock.Bytes}, nil
}

// Fingerprint is shorthand for FingerprintCert(id.Cert).
func (id *Identity) Fingerprint() Fingerprint {
	return FingerprintCert(id.Cert)
}

// LoadOrGenerate is the first-run-friendly identity bootstrap:
// load if dir already has cert+key, generate a fresh identity for
// role and persist it if the dir is empty. Anything other than a
// missing-file error is fatal — corrupt PEM, perm errors, etc.
// must never silently regenerate, because regenerating means every
// peer that already approved this fingerprint stops trusting us.
func LoadOrGenerate(dir string, role Role) (*Identity, error) {
	id, err := Load(dir)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("identity: load: %w", err)
	}
	fresh, err := Generate(role)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	if err := Save(dir, fresh); err != nil {
		return nil, fmt.Errorf("identity: save: %w", err)
	}
	return fresh, nil
}
