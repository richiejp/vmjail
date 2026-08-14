package mitmproxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	iofs "io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// caValidity is generous because a persisted CA (LoadOrCreate with
	// a caDir) is a trust anchor the sandbox keeps across many runs; a
	// 24h cert would force re-distribution daily. Still a dev/sandbox
	// CA — see the package doc.
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 24 * time.Hour
	leafRenewal  = time.Hour
	caCertName   = "ca.pem"
	caKeyName    = "ca.key"
	caKeyPerm    = 0o600
	caCertPerm   = 0o644
	keySize      = 2048
)

// rsaGenerateKey / randomSerialFn / createCertFn are seams so the
// crypto error paths in NewCertCache + leafForHost can be exercised
// without invasive mocking of the stdlib rand source.
//
//nolint:gochecknoglobals // test seams, mutated only under sequential tests.
var (
	rsaGenerateKey = rsa.GenerateKey
	randomSerialFn = randomSerialDefault
	createCertFn   = x509.CreateCertificate
	nowFn          = time.Now
)

// CertCache holds the proxy's CA and a per-host leaf-cert cache.
// Leaves are signed on demand the first time a host is seen;
// subsequent CONNECTs to the same host reuse the cached leaf. The
// leaf cache has no expiry policy — a Proxy is meant to be tied to a
// VM's lifetime.
type CertCache struct {
	caCert    *x509.Certificate
	caKey     *rsa.PrivateKey
	caCertPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// CACertPEM returns the CA certificate in PEM form. The guest trusts
// it (via NIX_SSL_CERT_FILE / the image's CA bundle) so the proxy's
// MITM leaves validate.
func (c *CertCache) CACertPEM() []byte { return c.caCertPEM }

// NewCertCache mints a fresh ephemeral CA.
func NewCertCache() (*CertCache, error) {
	caKey, err := rsaGenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, fmt.Errorf("rsa.GenerateKey: %w", err)
	}
	serial, err := randomSerialFn()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "daft-mitm-proxy CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := createCertFn(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("x509.CreateCertificate(ca): %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("x509.ParseCertificate(ca): %w", err)
	}
	return &CertCache{
		caCert:    cert,
		caKey:     caKey,
		caCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves:    make(map[string]*tls.Certificate),
	}, nil
}

// LoadOrCreateCertCache returns a CertCache. With caDir == "" it mints
// a fresh ephemeral CA. With caDir set it loads ca.pem + ca.key if
// both are present, otherwise mints a fresh CA and persists it there —
// so the sandbox's trust anchor stays stable across proxy restarts and
// the CA only has to be distributed once. A corrupt (non-missing) CA
// on disk is a hard error rather than a silent regenerate, so a
// stale-trust mismatch surfaces instead of hiding.
func LoadOrCreateCertCache(caDir string) (*CertCache, error) {
	if caDir == "" {
		return NewCertCache()
	}
	certPath := filepath.Join(caDir, caCertName)
	keyPath := filepath.Join(caDir, caKeyName)
	cc, err := loadCertCache(certPath, keyPath)
	switch {
	case err == nil:
		return cc, nil
	case !errors.Is(err, iofs.ErrNotExist):
		return nil, err
	}
	cc, err = NewCertCache()
	if err != nil {
		return nil, err
	}
	if perr := cc.persist(caDir, certPath, keyPath); perr != nil {
		return nil, perr
	}
	return cc, nil
}

// loadCertCache reconstructs a CertCache from a PEM cert + PKCS1 key on
// disk. A missing file propagates as fs.ErrNotExist (the caller treats
// that as "mint a fresh one"); a present-but-malformed file is a
// distinct hard error.
func loadCertCache(certPath, keyPath string) (*CertCache, error) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // path is caDir + constant.
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // path is caDir + constant.
	if err != nil {
		return nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("ca: no PEM block in %s", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse cert: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca: no PEM block in %s", keyPath)
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse key: %w", err)
	}
	return &CertCache{
		caCert:    cert,
		caKey:     key,
		caCertPEM: certPEM,
		leaves:    make(map[string]*tls.Certificate),
	}, nil
}

// persist writes the CA cert (world-readable; it's public) and key
// (0600; it's the signing secret) under caDir.
func (c *CertCache) persist(caDir, certPath, keyPath string) error {
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return fmt.Errorf("ca: mkdir %s: %w", caDir, err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(c.caKey),
	})
	if err := os.WriteFile(keyPath, keyPEM, caKeyPerm); err != nil {
		return fmt.Errorf("ca: write key: %w", err)
	}
	if err := os.WriteFile(certPath, c.caCertPEM, caCertPerm); err != nil { //nolint:gosec // CA cert is public.
		return fmt.Errorf("ca: write cert: %w", err)
	}
	return nil
}

func (c *CertCache) leafForHost(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok && leaf.Leaf != nil && leaf.Leaf.NotAfter.Sub(nowFn()) > leafRenewal {
		return leaf, nil
	}
	key, err := rsaGenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, fmt.Errorf("rsa.GenerateKey(leaf): %w", err)
	}
	serial, err := randomSerialFn()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    nowFn().Add(-time.Minute),
		NotAfter:     nowFn().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := createCertFn(rand.Reader, tmpl, c.caCert, &key.PublicKey, c.caKey)
	if err != nil {
		return nil, fmt.Errorf("x509.CreateCertificate(leaf %s): %w", host, err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse leaf %s: %w", host, err)
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, c.caCert.Raw},
		PrivateKey:  key,
		Leaf:        parsed,
	}
	c.leaves[host] = leaf
	return leaf, nil
}

func randomSerialDefault() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128) //nolint:mnd // standard cert-serial bit width.
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("rand.Int: %w", err)
	}
	return n, nil
}
