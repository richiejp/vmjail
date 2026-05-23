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
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/localai-org/darkaifactory/internal/banner"
)

const (
	readyPrefix     = "daft-mitm-proxy listening on "
	defaultAddr     = "127.0.0.1:0"
	caValidity      = 24 * time.Hour
	leafValidity    = time.Hour
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
		"write the generated CA cert (PEM) to this path; "+
			"the in-VM client must trust it via NIX_SSL_CERT_FILE")
	insecureUpstream := fs.Bool("insecure-upstream", false,
		"skip upstream TLS verification (for self-signed test peers)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	certs, err := newCertCache()
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
	logSink  io.Writer
	logMu    sync.Mutex
}

func newProxy(certs *certCache, insecureUpstream bool, logSink io.Writer) *proxy {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecureUpstream} //nolint:gosec // debug tool, gated by flag.
	tr.ResponseHeaderTimeout = upstreamTimeout
	return &proxy{certs: certs, upstream: tr, logSink: logSink}
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
	start := time.Now()
	resp, err := p.upstream.RoundTrip(req)
	if err != nil {
		p.logLine("%s %s -> err=%v", req.Method, req.URL, err)
		_, _ = io.WriteString(tlsClient, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	cw := &countingWriter{w: tlsClient}
	wErr := resp.Write(cw)
	elapsed := time.Since(start)
	statusForLog := resp.StatusCode
	if wErr != nil {
		p.logLine("%s %s -> %d bytes=%d elapsed=%s writeback-err=%v",
			req.Method, req.URL, statusForLog, cw.n, elapsed, wErr)
		return
	}
	p.logLine("%s %s -> %d bytes=%d elapsed=%s",
		req.Method, req.URL, statusForLog, cw.n, elapsed)
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
