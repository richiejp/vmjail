package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateProducesValidIdentity(t *testing.T) {
	id, err := Generate(RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if id.Cert == nil || id.Key == nil || len(id.DER) == 0 {
		t.Fatalf("Generate returned incomplete identity: %+v", id)
	}
	if !strings.HasPrefix(id.Cert.Subject.CommonName, "daft-orchestrator-") {
		t.Errorf("CN = %q, want daft-orchestrator-<hex>", id.Cert.Subject.CommonName)
	}
	pub, ok := id.Cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("cert public key type %T, want ed25519.PublicKey", id.Cert.PublicKey)
	}
	if !pub.Equal(id.Key.Public()) {
		t.Error("cert public key does not match private key")
	}
}

func TestGenerateRejectsUnknownRole(t *testing.T) {
	_, err := Generate("zalgo")
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleClient, RoleOrchestrator, RoleSandboxHost} {
		if !r.Valid() {
			t.Errorf("%q should be valid", r)
		}
	}
	if Role("nope").Valid() {
		t.Error("nope should not be valid")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File modes must be 0o600.
	for _, name := range []string{certFilename, keyFilename} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Stat %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode != filePerm {
			t.Errorf("%s perm = %o, want %o", name, mode, filePerm)
		}
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Fingerprint() != id.Fingerprint() {
		t.Error("loaded fingerprint differs from saved")
	}
	if !loaded.Key.Equal(id.Key) {
		t.Error("loaded key differs from saved")
	}
}

func TestSaveRejectsNilIdentity(t *testing.T) {
	if err := Save(t.TempDir(), nil); err == nil {
		t.Fatal("expected error for nil identity")
	}
}

func TestSaveRejectsIncompleteIdentity(t *testing.T) {
	if err := Save(t.TempDir(), &Identity{}); err == nil {
		t.Fatal("expected error for empty identity")
	}
}

func TestSaveFailsWhenParentIsAFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(filepath.Join(blocker, "child"), id); err == nil {
		t.Fatal("expected error when parent is a file")
	}
}

// TestSaveFailsWhenTargetIsADir exercises writePrivateFile's Rename
// error branch + oops.Skip cleanup for both the key and cert sides.
// One of the two writePrivateFile calls runs into an existing
// non-empty directory at its destination; rename fails with EISDIR
// (or ENOTEMPTY) and Save propagates the error.
func TestSaveFailsWhenTargetIsADir(t *testing.T) {
	cases := []string{certFilename, keyFilename}
	for _, target := range cases {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			// #nosec G301 — test-only, dir under t.TempDir().
			if err := os.MkdirAll(filepath.Join(dir, target, "child"), 0o755); err != nil {
				t.Fatalf("seed: %v", err)
			}
			id, err := Generate(RoleClient)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if err := Save(dir, id); err == nil {
				t.Fatalf("Save: expected error when %s is a dir", target)
			}
		})
	}
}

func TestLoadMissingDirReturnsErrNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadMissingKeyReturnsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, keyFilename)); err != nil {
		t.Fatalf("Remove key: %v", err)
	}
	if _, err := Load(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadRejectsCorruptCertPEM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, certFilename), []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFilename), []byte("nope"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for non-PEM cert")
	}
}

func TestLoadRejectsWrongPEMTypeForCert(t *testing.T) {
	dir := t.TempDir()
	bogus := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})
	if err := os.WriteFile(filepath.Join(dir, certFilename), bogus, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFilename), bogus, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for wrong PEM type")
	}
}

func TestLoadRejectsCertWithGarbageBytes(t *testing.T) {
	dir := t.TempDir()
	bad := pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: []byte("not asn.1")})
	if err := os.WriteFile(filepath.Join(dir, certFilename), bad, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFilename), []byte("whatever"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected ParseCertificate error")
	}
}

func TestLoadRejectsCorruptKeyPEM(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFilename), []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for non-PEM key")
	}
}

func TestLoadRejectsWrongKeyPEMType(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	bogus := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})
	if err := os.WriteFile(filepath.Join(dir, keyFilename), bogus, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for wrong key PEM type")
	}
}

func TestLoadRejectsKeyWithGarbageBytes(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: []byte("not asn.1")})
	if err := os.WriteFile(filepath.Join(dir, keyFilename), bad, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected ParsePKCS8PrivateKey error")
	}
}

func TestLoadRejectsRSAKey(t *testing.T) {
	// Identity insists on Ed25519. Plant an RSA-typed PKCS8 key so
	// the type-assertion branch fires.
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rsaPriv := generateRSAKey(t)
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaPriv)
	if err != nil {
		t.Fatalf("Marshal RSA: %v", err)
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: rsaDER})
	if err := os.WriteFile(filepath.Join(dir, keyFilename), rsaPEM, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for RSA key")
	}
}

func TestLoadRejectsKeyMismatchedToCert(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Overwrite the key with a freshly generated, unrelated one.
	other, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate other: %v", err)
	}
	otherDER, err := x509.MarshalPKCS8PrivateKey(other.Key)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	otherPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: otherDER})
	if err := os.WriteFile(filepath.Join(dir, keyFilename), otherPEM, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected mismatched-key error")
	}
}

func TestLoadOrGenerateFirstRunPersists(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadOrGenerate(dir, RoleClient)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	id2, err := LoadOrGenerate(dir, RoleClient)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Error("fingerprint changed across calls — second call regenerated")
	}
}

func TestLoadOrGeneratePropagatesCorruptCert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, certFilename), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFilename), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := LoadOrGenerate(dir, RoleClient); err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

func TestLoadOrGenerateRejectsBadRole(t *testing.T) {
	if _, err := LoadOrGenerate(t.TempDir(), "zalgo"); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestLoadOrGeneratePropagatesSaveError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadOrGenerate(filepath.Join(blocker, "child"), RoleClient); err == nil {
		t.Fatal("expected save failure when dir parent is a file")
	}
}

// Patch a non-Ed25519 cert into the directory so the
// "cert.PublicKey is not ed25519" branch fires.
func TestLoadRejectsCertWithRSAPublicKey(t *testing.T) {
	dir := t.TempDir()
	id, err := Generate(RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Save(dir, id); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Build a self-signed RSA cert with the same template shape, but
	// clear the algorithm fields so x509 re-derives them from the RSA
	// signing key.
	rsaPriv := generateRSAKey(t)
	tmpl := *id.Cert
	tmpl.PublicKeyAlgorithm = 0
	tmpl.PublicKey = nil
	tmpl.SignatureAlgorithm = 0
	tmpl.Signature = nil
	rsaDER, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &rsaPriv.PublicKey, rsaPriv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: rsaDER})
	if err := os.WriteFile(filepath.Join(dir, certFilename), rsaPEM, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for RSA cert")
	}
}
