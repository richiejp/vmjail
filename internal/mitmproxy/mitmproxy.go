// Package mitmproxy is an HTTPS-MITM forward proxy: it terminates
// the guest's TLS, re-encrypts to the real upstream, and optionally
// records/replays GET/200 responses through a content-addressed
// cache (see internal/httpcache). It began life as the standalone
// daft-mitm-proxy investigation tool (PLAN.md D6c.3 / D8) and is now
// the egress lane for Phase J: sandboxd serves a Proxy in-process on
// the per-VM netstack's gateway, so a default-deny guest reaches the
// WAN only through this terminating, caching hop.
//
// Wire shape (in-stack, production):
//
//	guest --https_proxy--> gateway:<port> (in-stack listener)
//	                         ↓ MITM (terminate + re-encrypt)
//	                       real cache.nixos.org / etc.
//
// The Proxy signs per-host leaf certs on demand from a CA held by a
// CertCache; the guest trusts that CA via NIX_SSL_CERT_FILE. The CA
// + permissive leaf generation are appropriate for a confined sandbox
// lane, not for a user-facing trust anchor.
package mitmproxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/httpcache"
)

const (
	upstreamTimeout = 5 * time.Minute
	connectOKLine   = "HTTP/1.1 200 OK\r\n\r\n"
	connectMethod   = "CONNECT"
)

// Proxy implements http.Handler. CONNECT requests are MITM'd; plain
// HTTP is rejected — nix uses HTTPS for cache.nixos.org and rejecting
// plain HTTP keeps the surface small.
type Proxy struct {
	certs    *CertCache
	upstream *http.Transport
	client   *http.Client
	logSink  io.Writer
	logMu    sync.Mutex
	// cache, when non-nil, turns the proxy into a record/replay
	// caching layer (PLAN.md Phase I, slice I1): GET/200 responses
	// are stored content-addressed and replayed on a repeat request.
	// nil keeps the pure pass-through behaviour. Set via SetCache.
	cache *httpcache.Cache
}

// New builds a Proxy signing leaves from certs. insecureUpstream skips
// upstream TLS verification (self-signed test peers); logSink receives
// one line per request. Caching is off until SetCache.
func New(certs *CertCache, insecureUpstream bool, logSink io.Writer) *Proxy {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecureUpstream} //nolint:gosec // gated by caller flag for self-signed test peers.
	tr.ResponseHeaderTimeout = upstreamTimeout
	// client follows redirects so the proxy serves the *final* body
	// to the guest. nix's flake fetch hits
	// github.com/.../archive/<rev>.tar.gz which 302-redirects to
	// codeload.github.com; a bare Transport.RoundTrip would hand the
	// 302 back and nix's libcurl would try to follow it on a path that
	// doesn't survive the MITM, leaving the unpacked tree empty.
	// Following here keeps the chain inside the proxy and lets the big
	// download be observed in one log line.
	return &Proxy{
		certs:    certs,
		upstream: tr,
		client:   &http.Client{Transport: tr},
		logSink:  logSink,
	}
}

// SetCache enables record/replay caching on p. Pass an opened
// *httpcache.Cache; nil leaves pure pass-through behaviour.
func (p *Proxy) SetCache(c *httpcache.Cache) { p.cache = c }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != connectMethod {
		http.Error(w, "daft-mitm-proxy: only CONNECT (HTTPS via proxy) is supported",
			http.StatusMethodNotAllowed)
		return
	}
	p.handleConnect(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
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

	// The client (inside the TLS tunnel) sends one or more HTTP
	// requests. Keep-Alive is rare for cache.nixos.org closure
	// fetches in practice, but loop so we don't cut a pipelined
	// client off prematurely.
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

func (p *Proxy) serveOne(tlsClient io.Writer, req *http.Request) {
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

	// resp.Request is the final request after any redirects; log it
	// alongside the original so a github->codeload hop is visible
	// rather than hidden behind a transparent 200.
	finalURL := reqURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	// Cacheable responses are stored into the CAS and then replayed
	// from it, so the store path and the hit path share one code path
	// (and one set of bugs). The body is consumed by Put.
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

	// `resp.Write` formats the status line as
	// `HTTP/<ProtoMajor>.<ProtoMinor> <code> <text>` — so an HTTP/2
	// upstream (Go's default for HTTPS) leaks
	// "HTTP/2.0 404 Not Found" as the wire status line of an
	// HTTP/1.x exchange. Forgiving clients (curl 8.x, Go's own
	// http.Client) downgrade to HTTP/1.0 and keep parsing; strict
	// clients (libcurl + custom layering as in nix's binary-cache
	// substituter) get confused and may treat the body as the
	// payload of the failed parse — the symptom that hung the D6c.4
	// selfbuild test. Normalise to HTTP/1.1 before writing.
	// serveFromCache already constructs its response with
	// Proto="HTTP/1.1" so this only needed fixing on the
	// passthrough path. Regression-guarded by
	// TestPassthroughHTTP2UpstreamWrittenAsHTTP11.
	resp.Proto = "HTTP/1.1"
	resp.ProtoMajor = 1
	resp.ProtoMinor = 1
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
func (p *Proxy) serveFromCache(tlsClient io.Writer, req *http.Request, tag string, e httpcache.Entry) {
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

func (p *Proxy) logLine(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_, _ = fmt.Fprintf(p.logSink, "%s %s\n",
		time.Now().UTC().Format(time.RFC3339Nano), line)
}

// countingWriter is io.Writer that records bytes written. Used to
// record response sizes (headers+body) so the log shows the volume
// each request transferred.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
