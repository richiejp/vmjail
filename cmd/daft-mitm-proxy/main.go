// daft-mitm-proxy is a debug-only HTTPS-MITM forward proxy used
// to understand the bun-test bottleneck (PLAN.md D6c.3 slice 2,
// "Throughput floor under investigation"). The earlier layered
// benches ruled out the gvisor stack but left "what is nix
// actually doing during the 50 KB/s phase?" unanswered. This
// proxy answers it by sitting between the in-VM nix client and
// the real upstream, logging every HTTP request that flows
// through.
//
// Wire shape:
//
//	in-VM nix --https_proxy--> daft-mitm-proxy on host
//	                             ↓ MITM (terminate + re-encrypt)
//	                          real cache.nixos.org / etc.
//
// The proxy generates a fresh CA at startup, signs per-host leaf
// certs on demand, and writes the CA cert PEM to --ca-cert-out
// so the VM can trust it via NIX_SSL_CERT_FILE. Each request is
// logged as one line:
//
//	<ts> <method> <url> -> <status> bytes=<n> elapsed=<d>
//
// NOT FOR PRODUCTION USE. Generated CA + permissive cert
// generation are appropriate for a one-machine investigation,
// not for anything user-facing.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/localai-org/darkaifactory/internal/banner"
	"github.com/localai-org/darkaifactory/internal/httpcache"
)

const (
	readyPrefix = "daft-mitm-proxy listening on "
	defaultAddr = "127.0.0.1:0"
	// caValidity is generous because a persisted CA (--ca-dir) is a
	// trust anchor the sandbox keeps across many runs; a 24h cert
	// would force re-distribution daily. Still a dev/test CA — see
	// the file header.
	caValidity      = 10 * 365 * 24 * time.Hour
	leafValidity    = time.Hour
	caCertName      = "ca.pem"
	caKeyName       = "ca.key"
	caKeyPerm       = 0o600
	caCertPerm      = 0o644
	upstreamTimeout = 5 * time.Minute
	connectOKLine   = "HTTP/1.1 200 OK\r\n\r\n"
	keySize         = 2048
	connectMethod   = "CONNECT"
)

//nolint:gochecknoglobals // test seams.
var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
	// rsaGenerateKey + randomSerialFn are seams so the crypto
	// error paths in newCertCache + leafForHost can be exercised
	// without invasive mocking of the stdlib rand source.
	rsaGenerateKey = rsa.GenerateKey
	randomSerialFn = randomSerialDefault
	createCertFn   = x509.CreateCertificate
)

func main() {
	osExit(run(os.Args[1:], stdout))
}

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("daft-mitm-proxy", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = fmt.Fprint(out, banner.String)
		_, _ = fmt.Fprintln(out, "\nUsage of daft-mitm-proxy:")
		fs.PrintDefaults()
	}
	addr := fs.String("addr", defaultAddr, "listen address (host:port)")
	caCertOut := fs.String("ca-cert-out", "",
		"write the CA cert (PEM) to this path; "+
			"the in-VM client must trust it via NIX_SSL_CERT_FILE")
	caDir := fs.String("ca-dir", "",
		"persist the CA cert+key under this dir and reuse it across "+
			"runs (empty = fresh ephemeral CA each startup)")
	insecureUpstream := fs.Bool("insecure-upstream", false,
		"skip upstream TLS verification (for self-signed test peers)")
	cacheDir := fs.String("cache-dir", "",
		"enable record/replay caching, storing GET/200 responses "+
			"content-addressed under this dir (empty = pure pass-through)")
	offline := fs.Bool("offline", false,
		"serve only from --cache-dir; a cache miss fails the request "+
			"(504) instead of reaching upstream")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	certs, err := loadOrCreateCertCache(*caDir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: ca: %v\n", err)
		return 1
	}
	if *caCertOut != "" {
		if werr := os.WriteFile(*caCertOut, certs.caCertPEM, 0o644); werr != nil { //nolint:gosec // CA cert is public.
			_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: write %s: %v\n", *caCertOut, werr)
			return 1
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: listen %s: %v\n", *addr, err)
		return 1
	}

	p := newProxy(certs, *insecureUpstream, out)
	if *cacheDir != "" {
		cache, cerr := httpcache.Open(*cacheDir, *offline)
		if cerr != nil {
			_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: cache: %v\n", cerr)
			return 1
		}
		p.cache = cache
	}
	_, _ = fmt.Fprintf(out, "%s%s\n", readyPrefix, ln.Addr())
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: serve: %v\n", serr)
		return 1
	}
	return 0
}

// proxy implements http.Handler. CONNECT requests are MITM'd;
// plain HTTP is rejected — nix uses HTTPS for cache.nixos.org
// and rejecting plain HTTP keeps the surface small.
type proxy struct {
	certs    *certCache
	upstream *http.Transport
	client   *http.Client
	logSink  io.Writer
	logMu    sync.Mutex
	// cache, when non-nil, turns the proxy into a record/replay
	// caching layer (PLAN.md Phase I, slice I1): GET/200 responses
	// are stored content-addressed and replayed on a repeat
	// request. nil keeps the pure pass-through behaviour. Set by
	// run() from --cache-dir; left nil by newProxy so tests that
	// don't care are unaffected.
	cache *httpcache.Cache
}

func newProxy(certs *certCache, insecureUpstream bool, logSink io.Writer) *proxy {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecureUpstream} //nolint:gosec // debug tool, gated by flag.
	tr.ResponseHeaderTimeout = upstreamTimeout
	// client follows redirects so the proxy serves the *final*
	// body to the in-VM client. nix's flake fetch hits
	// github.com/.../archive/<rev>.tar.gz which 302-redirects to
	// codeload.github.com; a bare Transport.RoundTrip would hand
	// the 302 back and nix's libcurl would try to follow it on a
	// path that doesn't survive the MITM, leaving the unpacked
	// tree empty. Following here keeps the chain inside the proxy
	// and lets the big download be observed in one log line.
	return &proxy{
		certs:    certs,
		upstream: tr,
		client:   &http.Client{Transport: tr},
		logSink:  logSink,
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != connectMethod {
		http.Error(w, "daft-mitm-proxy: only CONNECT (HTTPS via proxy) is supported",
			http.StatusMethodNotAllowed)
		return
	}
	p.handleConnect(w, r)
}

func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "daft-mitm-proxy: hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, werr := io.WriteString(clientConn, connectOKLine); werr != nil {
		return
	}

	leaf, err := p.certs.leafForHost(host)
	if err != nil {
		p.logLine("ca-leaf err for %s: %v", host, err)
		return
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
	}
	tlsClient := tls.Server(clientConn, tlsCfg)
	defer func() { _ = tlsClient.Close() }()
	if herr := tlsClient.Handshake(); herr != nil {
		p.logLine("tls handshake from client for %s: %v", host, herr)
		return
	}

	// The client (inside the TLS tunnel) sends one or more
	// HTTP requests. Keep-Alive is rare for cache.nixos.org
	// closure fetches in practice, but loop so we don't cut a
	// pipelined client off prematurely.
	reader := bufio.NewReader(tlsClient)
	//daft:lint:disable=forloopbound reason="exits on EOF / per-request error from the tunnel"
	for {
		req, rerr := http.ReadRequest(reader)
		if rerr != nil {
			return
		}
		req.URL.Scheme = "https"
		req.URL.Host = r.Host
		req.RequestURI = ""
		req.Header.Del("Proxy-Connection")
		p.serveOne(tlsClient, req)
	}
}

func (p *proxy) serveOne(tlsClient io.Writer, req *http.Request) {
	reqURL := req.URL.String()
	key := req.Method + " " + reqURL

	if p.cache != nil {
		if entry, ok := p.cache.Lookup(key); ok {
			p.serveFromCache(tlsClient, req, "HIT", entry)
			return
		}
		if p.cache.Offline() {
			p.logLine("%s %s -> MISS offline (504)", req.Method, reqURL)
			_, _ = io.WriteString(tlsClient,
				"HTTP/1.1 504 Gateway Timeout\r\nContent-Length: 0\r\n\r\n")
			return
		}
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		p.logLine("%s %s -> err=%v", req.Method, reqURL, err)
		_, _ = io.WriteString(tlsClient, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// resp.Request is the final request after any redirects; log
	// it alongside the original so a github->codeload hop is
	// visible rather than hidden behind a transparent 200.
	finalURL := reqURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	// Cacheable responses are stored into the CAS and then replayed
	// from it, so the store path and the hit path share one code
	// path (and one set of bugs). The body is consumed by Put.
	if p.cache != nil && cacheable(req, resp) {
		entry, perr := p.cache.Put(key, resp.StatusCode, resp.Header, resp.Body)
		if perr != nil {
			p.logLine("%s %s -> cache-store-err=%v", req.Method, finalURL, perr)
			_, _ = io.WriteString(tlsClient, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		p.logLine("%s %s -> %d bytes=%d elapsed=%s STORE",
			req.Method, finalURL, entry.Status, entry.Size, time.Since(start))
		p.serveFromCache(tlsClient, req, "STORE-REPLAY", entry)
		return
	}

	cw := &countingWriter{w: tlsClient}
	wErr := resp.Write(cw)
	elapsed := time.Since(start)
	if wErr != nil {
		p.logLine("%s %s -> %d bytes=%d elapsed=%s writeback-err=%v",
			req.Method, finalURL, resp.StatusCode, cw.n, elapsed, wErr)
		return
	}
	p.logLine("%s %s -> %d bytes=%d elapsed=%s",
		req.Method, finalURL, resp.StatusCode, cw.n, elapsed)
}

// serveFromCache reconstructs a stored response from the CAS body +
// recorded status/headers and writes it back to the client. The
// stored header set has framing headers stripped (see httpcache), so
// resp.Write derives Content-Length from the known body size.
func (p *proxy) serveFromCache(tlsClient io.Writer, req *http.Request, tag string, e httpcache.Entry) {
	body, err := p.cache.Body(e.Digest)
	if err != nil {
		p.logLine("%s %s -> cache-read-err=%v", req.Method, req.URL, err)
		_, _ = io.WriteString(tlsClient, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = body.Close() }()

	resp := &http.Response{
		StatusCode:    e.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        e.Header.Clone(),
		Body:          body,
		ContentLength: int64(e.Size), //nolint:gosec // G115: cached body size fits int64.
		Request:       req,
	}
	cw := &countingWriter{w: tlsClient}
	if wErr := resp.Write(cw); wErr != nil {
		p.logLine("%s %s -> %d bytes=%d %s writeback-err=%v",
			req.Method, req.URL, e.Status, cw.n, tag, wErr)
		return
	}
	p.logLine("%s %s -> %d bytes=%d %s",
		req.Method, req.URL, e.Status, cw.n, tag)
}

// cacheable reports whether a response may be recorded for replay.
// Conservative for slice I1: only idempotent GETs that returned 200.
// Range responses (206) and error statuses are passed through
// untouched so a partial or transient body never poisons the cache.
func cacheable(req *http.Request, resp *http.Response) bool {
	return req.Method == http.MethodGet && resp.StatusCode == http.StatusOK
}

func (p *proxy) logLine(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_, _ = fmt.Fprintf(p.logSink, "%s %s\n",
		time.Now().UTC().Format(time.RFC3339Nano), line)
}

// countingWriter is io.Writer that records bytes written. Used
// to record response sizes (headers+body) so the log shows the
// volume each request transferred.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// certCache holds the proxy's CA and a per-host leaf-cert
// cache. Leaves are signed on demand the first time a host is
// seen; subsequent CONNECTs to the same host reuse the cached
// leaf. Cache has no expiry policy — the process is meant to
// be short-lived (the duration of an investigation).
type certCache struct {
	caCert    *x509.Certificate
	caKey     *rsa.PrivateKey
	caCertPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func newCertCache() (*certCache, error) {
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
	return &certCache{
		caCert:    cert,
		caKey:     caKey,
		caCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves:    make(map[string]*tls.Certificate),
	}, nil
}

// loadOrCreateCertCache returns a certCache. With caDir == "" it
// mints a fresh ephemeral CA (the investigation-tool default). With
// caDir set it loads ca.pem + ca.key if both are present, otherwise
// mints a fresh CA and persists it there — so the sandbox's trust
// anchor stays stable across proxy restarts and the CA only has to be
// distributed once. A corrupt (non-missing) CA on disk is a hard
// error rather than a silent regenerate, so a stale-trust mismatch
// surfaces instead of hiding.
func loadOrCreateCertCache(caDir string) (*certCache, error) {
	if caDir == "" {
		return newCertCache()
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
	cc, err = newCertCache()
	if err != nil {
		return nil, err
	}
	if perr := cc.persist(caDir, certPath, keyPath); perr != nil {
		return nil, perr
	}
	return cc, nil
}

// loadCertCache reconstructs a certCache from a PEM cert + PKCS1 key
// on disk. A missing file propagates as fs.ErrNotExist (the caller
// treats that as "mint a fresh one"); a present-but-malformed file
// is a distinct hard error.
func loadCertCache(certPath, keyPath string) (*certCache, error) {
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
	return &certCache{
		caCert:    cert,
		caKey:     key,
		caCertPEM: certPEM,
		leaves:    make(map[string]*tls.Certificate),
	}, nil
}

// persist writes the CA cert (world-readable; it's public) and key
// (0600; it's the signing secret) under caDir.
func (c *certCache) persist(caDir, certPath, keyPath string) error {
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

func (c *certCache) leafForHost(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
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
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(leafValidity),
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
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, c.caCert.Raw},
		PrivateKey:  key,
		Leaf:        nil,
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
