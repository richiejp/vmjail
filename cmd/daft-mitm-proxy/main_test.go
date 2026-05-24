package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/httpcache"
)

// syncBuffer is a mutex-guarded io.Writer + reader for log
// assertions. The proxy logs from its serve goroutine while the
// test reads from the test goroutine; a raw bytes.Buffer would
// be a data race, and reading before the serve goroutine has
// logged would be a timing race. waitForContains polls until
// the substring lands or the deadline passes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) waitForContains(t *testing.T, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline"
	for {
		if strings.Contains(s.String(), substr) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEndToEndMITM is the smoke test: a real httptest TLS
// server serves a body, the proxy MITMs an HTTPS request to it
// from a client that trusts the proxy's CA, the response body
// arrives intact, and the proxy's log records one request line
// with the right URL, status, and byte count.
func TestEndToEndMITM(t *testing.T) {
	body := []byte("mitm-test-body-content-here\n")
	wantSHA := sha256.Sum256(body)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)

	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })

	proxySrv := &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = proxySrv.Serve(proxyLn) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxySrv.Shutdown(ctx)
	})

	pool := x509.NewCertPool()
	pool.AddCert(certs.caCert)
	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	// Upstream's URL is https://127.0.0.1:PORT/. Rewrite the
	// host to "localhost" so the proxy's leaf cert (signed for
	// the SNI hostname) is exercised on a DNS-name path too;
	// the test's RootCA pool trusts our CA, so the leaf cert's
	// CN doesn't need to be 127.0.0.1.
	upURL, _ := url.Parse(upstream.URL)
	_, port, _ := net.SplitHostPort(upURL.Host)
	target := "https://localhost:" + port + "/file"

	// httptest server only listens on 127.0.0.1, but the
	// proxy's CONNECT target lookup uses the host:port we
	// send. Patch the DNS via DialContext so the proxy's
	// upstream Transport routes localhost -> 127.0.0.1.
	p.upstream.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(addr)
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body mismatch: got %q (%d bytes), want %q (%d bytes)",
			got, len(got), body, len(body))
	}
	gotSHA := sha256.Sum256(got)
	if gotSHA != wantSHA {
		t.Fatalf("sha256 mismatch: got %s, want %s",
			hex.EncodeToString(gotSHA[:]), hex.EncodeToString(wantSHA[:]))
	}

	// The proxy logs from its serve goroutine after writing the
	// response back, which races the client's read completing.
	// Poll rather than read once.
	if !logSink.waitForContains(t, "GET https://localhost:"+port+"/file", 2*time.Second) {
		t.Errorf("log missing GET line: %q", logSink.String())
	}
	if !logSink.waitForContains(t, "-> 200 bytes=", 2*time.Second) {
		t.Errorf("log missing 200 status: %q", logSink.String())
	}
}

// TestRunWritesCAToDisk pins the --ca-cert-out behaviour the
// integration wiring needs: invoking the binary with the flag
// writes a PEM-encoded CA cert to the path.
func TestRunWritesCAToDisk(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "proxy-ca.pem")
	// Start in a goroutine since http.Serve blocks; we only
	// care that the file appears before Serve gets to it,
	// which is the order in run().
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{ephemeralAddrFlag, "--ca-cert-out=" + caPath}, &out)
	}()

	// Poll: file should land within a few hundred ms of binary
	// start. If the binary errored out, done fires with non-zero;
	// re-check below.
	deadline := time.Now().Add(5 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by 5s deadline"
	for {
		if _, err := os.Stat(caPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ca-cert-out %s not written within 5s; binary out=%q", caPath, out.String())
		}
		select {
		case code := <-done:
			t.Fatalf("binary exited code=%d before CA was written; out=%q",
				code, out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}

	caPEM, err := os.ReadFile(caPath) //nolint:gosec // caPath is t.TempDir + constant.
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	if !bytes.HasPrefix(caPEM, []byte("-----BEGIN CERTIFICATE-----")) {
		t.Errorf("CA file does not start with PEM CERTIFICATE block: %s", caPEM[:min(len(caPEM), 64)])
	}
}

// TestProxyRejectsPlainHTTP confirms the small-surface choice:
// non-CONNECT requests get 405. nix uses HTTPS for the workload
// the proxy was built to study, so handling plain HTTP would
// be dead surface.
func TestProxyRejectsPlainHTTP(t *testing.T) {
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	p := newProxy(certs, true, io.Discard)

	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/anything") //nolint:noctx // bounded by srv.Close.
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), "only CONNECT") {
		t.Errorf("body = %q, want substring 'only CONNECT'", bodyBytes)
	}
}

// TestRunRejectsBadFlags pins the flag-parse error path so
// future arg additions don't accidentally turn errors into
// hangs.
func TestRunRejectsBadFlags(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--no-such-flag"}, &out)
	if code == 0 {
		t.Errorf("code = 0 on bad flag; want non-zero")
	}
}

// TestRunSurfacesListenError pins the listen-failure path.
func TestRunSurfacesListenError(t *testing.T) {
	// Bind a port, then ask run() to bind the same port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	addr := ln.Addr().String()
	var out bytes.Buffer
	code := run([]string{"--addr=" + addr}, &out)
	if code == 0 {
		t.Errorf("code = 0 on bound port; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "listen") {
		t.Errorf("missing listen err in out: %q", out.String())
	}
}

// TestLeafCacheReturnsSameCertForRepeatedHost is the
// behavioural pin for the per-host leaf-cert cache. A repeated
// request for the same host returns the same *tls.Certificate
// pointer; a different host returns a different one. Without
// this, every CONNECT would burn ~10ms generating an RSA-2048
// key.
func TestLeafCacheReturnsSameCertForRepeatedHost(t *testing.T) {
	c, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	a, err := c.leafForHost("example.com")
	if err != nil {
		t.Fatalf("leafForHost(a): %v", err)
	}
	b, err := c.leafForHost("example.com")
	if err != nil {
		t.Fatalf("leafForHost(b): %v", err)
	}
	if a != b {
		t.Errorf("cache miss for repeated host; got distinct certs")
	}
	d, err := c.leafForHost("other.example")
	if err != nil {
		t.Fatalf("leafForHost(d): %v", err)
	}
	if a == d {
		t.Errorf("cache collision across different hosts; same cert returned")
	}
}

// TestMainCallsRunWithOsArgs pins the trivial main() wrapper.
// Without it main() stays uncovered and drags the per-file gate.
func TestMainCallsRunWithOsArgs(t *testing.T) {
	oldExit, oldOut, oldArgs := osExit, stdout, os.Args
	defer func() { osExit, stdout, os.Args = oldExit, oldOut, oldArgs }()
	os.Args = []string{"daft-mitm-proxy", "--no-such-flag"}
	var buf bytes.Buffer
	stdout = &buf
	code := -1
	osExit = func(c int) { code = c }
	main()
	if code != 2 {
		t.Errorf("code = %d, want 2 (bad-flag exit)", code)
	}
}

// TestRunRejectsBadCAOutPath surfaces the "CA cert write failed"
// error path: pointing --ca-cert-out at a path whose parent
// doesn't exist must fail-fast with the listen step skipped.
func TestRunRejectsBadCAOutPath(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "no-such-dir", "ca.pem")
	var out bytes.Buffer
	code := run([]string{ephemeralAddrFlag, "--ca-cert-out=" + caPath}, &out)
	if code == 0 {
		t.Errorf("code = 0; want non-zero")
	}
	if !strings.Contains(out.String(), "write") {
		t.Errorf("missing 'write' err in out: %q", out.String())
	}
}

// TestRunSurfacesCAError covers run()'s fail-fast when the CA
// can't be built — exercised by forcing the rsaGenerateKey seam
// to error. run() must report "ca:" and return non-zero before
// touching the listener.
func TestRunSurfacesCAError(t *testing.T) {
	old := rsaGenerateKey
	t.Cleanup(func() { rsaGenerateKey = old })
	rsaGenerateKey = func(io.Reader, int) (*rsa.PrivateKey, error) {
		return nil, errFake("forced ca failure")
	}
	var out bytes.Buffer
	code := run([]string{ephemeralAddrFlag}, &out)
	if code == 0 {
		t.Errorf("code = 0; want non-zero")
	}
	if !strings.Contains(out.String(), "ca:") {
		t.Errorf("missing 'ca:' err in out: %q", out.String())
	}
}

// TestHandleConnectRequestLoop drives the proxy's request loop
// directly: establish the CONNECT tunnel, complete the TLS
// handshake trusting the proxy's CA, send one request (which
// 502s because the upstream dial is stubbed to fail), then
// close. This is the only test that exercises the loop's normal
// iteration + EOF-driven exit; TestEndToEndMITM goes through it
// via an http.Client whose keep-alive makes the exit racy.
func TestHandleConnectRequestLoop(t *testing.T) {
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)
	p.upstream.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errFake("no upstream in this test")
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	raw, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = raw.Close() }()

	if _, werr := io.WriteString(raw,
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); werr != nil {
		t.Fatalf("write CONNECT: %v", werr)
	}
	// Consume the "200 OK\r\n\r\n" CONNECT response off the raw
	// conn before starting TLS.
	if err := readUntilBlankLine(raw); err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(certs.caCert)
	tlsConn := tls.Client(raw, &tls.Config{
		RootCAs:    pool,
		ServerName: "example.com",
		MinVersion: tls.VersionTLS12,
	})
	if herr := tlsConn.Handshake(); herr != nil {
		t.Fatalf("client handshake: %v", herr)
	}
	if _, werr := io.WriteString(tlsConn,
		"GET /pkg HTTP/1.1\r\nHost: example.com\r\n\r\n"); werr != nil {
		t.Fatalf("write request: %v", werr)
	}
	// Expect the 502 (upstream dial stubbed to fail), then close
	// so the proxy's ReadRequest loop hits EOF and returns.
	_, _ = tlsConn.Read(make([]byte, 256))
	_ = tlsConn.Close()
	_ = raw.Close()

	if !logSink.waitForContains(t, "GET https://example.com:443/pkg -> err=", 2*time.Second) {
		t.Errorf("expected 502/err log for the request; got %q", logSink.String())
	}
}

// readUntilBlankLine consumes bytes from r one at a time until
// it sees the CRLFCRLF that terminates an HTTP response head.
// Byte-at-a-time keeps it from over-reading into the TLS bytes
// that follow on the same conn.
func readUntilBlankLine(r net.Conn) error {
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	var tail [4]byte
	one := make([]byte, 1)
	//daft:lint:disable=forloopbound reason="bounded by 5s read deadline"
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return err
		}
		copy(tail[:], tail[1:])
		tail[3] = one[0]
		if tail == [4]byte{'\r', '\n', '\r', '\n'} {
			return nil
		}
	}
}

// TestHandleConnectLeafGenError covers handleConnect's
// "ca-leaf err" branch: the proxy is built successfully (CA +
// construction work), then the createCertFn seam is broken so
// the lazy per-host leaf generation fails on the CONNECT. The
// proxy must log it and close without panicking.
func TestHandleConnectLeafGenError(t *testing.T) {
	old := createCertFn
	t.Cleanup(func() { createCertFn = old })

	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	// Break leaf generation only now — CA + proxy already built.
	createCertFn = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
		return nil, errFake("leaf gen broken")
	}

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := io.WriteString(conn,
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); werr != nil {
		t.Fatalf("write CONNECT: %v", werr)
	}
	_, _ = conn.Read(make([]byte, 64)) // 200 OK

	if !logSink.waitForContains(t, "ca-leaf err for example.com", 2*time.Second) {
		t.Errorf("expected ca-leaf err in log; got %q", logSink.String())
	}
}

// TestServeOneSurfacesUpstreamError exercises the 502 path in
// serveOne when the upstream Transport's RoundTrip returns an
// error (the proxy must log it and emit a 502 back).
func TestServeOneSurfacesUpstreamError(t *testing.T) {
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &bytes.Buffer{}
	p := newProxy(certs, true, logSink)
	// Force RoundTrip to fail by pointing DialContext at a
	// nonexistent peer with an immediate-fail dialer.
	p.upstream.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, errFakeDial
	}

	clientBuf := &bytes.Buffer{}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://example.invalid/", http.NoBody)
	p.serveOne(clientBuf, req)

	if !strings.Contains(clientBuf.String(), "502 Bad Gateway") {
		t.Errorf("expected 502 response written to client; got %q", clientBuf.String())
	}
	if !strings.Contains(logSink.String(), "err=") {
		t.Errorf("expected err= in log; got %q", logSink.String())
	}
}

var errFakeDial = errFake("forced dial failure")

type errFake string

func (e errFake) Error() string { return string(e) }

// ephemeralAddrFlag asks run() to bind an OS-assigned port on
// loopback. Shared by the run()-level tests.
const ephemeralAddrFlag = "--addr=127.0.0.1:0"

// TestHandleConnectGracefullyClosesOnTLSFail covers the
// post-hijack TLS-handshake-error branch: a client that hijacks
// the CONNECT but then speaks garbage instead of a TLS
// ClientHello must just log + close, not crash.
func TestHandleConnectGracefullyClosesOnTLSFail(t *testing.T) {
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)

	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := io.WriteString(conn,
		"CONNECT example.invalid:443 HTTP/1.1\r\nHost: example.invalid:443\r\n\r\n"); werr != nil {
		t.Fatalf("write CONNECT: %v", werr)
	}
	// Read the 200 OK then send bogus bytes instead of a TLS ClientHello.
	br := make([]byte, 64)
	n, _ := conn.Read(br)
	if !strings.Contains(string(br[:n]), "200") {
		t.Fatalf("CONNECT response not 200: %q", br[:n])
	}
	_, _ = io.WriteString(conn, "this is not a TLS ClientHello at all\n")

	if !logSink.waitForContains(t, "tls handshake", 2*time.Second) {
		t.Fatalf("never logged tls handshake err; log=%q", logSink.String())
	}
}

// TestNewProxyTransportFallback exercises the rare branch
// where http.DefaultTransport is not the standard *Transport
// (e.g. test harness has installed a stub).
func TestNewProxyTransportFallback(t *testing.T) {
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	http.DefaultTransport = stubRoundTripper{}

	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	p := newProxy(certs, false, io.Discard)
	if p.upstream == nil {
		t.Errorf("upstream Transport not constructed in fallback path")
	}
}

type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errFake("stub")
}

// TestHandleConnectAcceptsHostWithoutPort exercises the
// SplitHostPort-fails branch by sending a CONNECT with no port.
// Real clients won't do this, but the code falls back to the
// raw Host string, so the path must not panic and must still
// log something useful when the downstream TLS handshake fails.
func TestHandleConnectAcceptsHostWithoutPort(t *testing.T) {
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := io.WriteString(conn,
		"CONNECT just-a-host HTTP/1.1\r\nHost: just-a-host\r\n\r\n"); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	// Read the 200 then drop the connection — handshake will
	// fail, branch is logged.
	_, _ = conn.Read(make([]byte, 64))
	_ = conn.Close()

	if !logSink.waitForContains(t, "just-a-host", 2*time.Second) {
		t.Fatalf("expected log entry referencing just-a-host; got %q", logSink.String())
	}
}

// TestHandleConnectRejectsNonHijacker covers the rare branch
// where the http.ResponseWriter doesn't implement Hijacker.
// httptest's responseWriter always does, so the only way to
// exercise this is a hand-rolled fake.
func TestHandleConnectRejectsNonHijacker(t *testing.T) {
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	p := newProxy(certs, true, io.Discard)

	w := &nonHijackingRecorder{header: http.Header{}}
	req := &http.Request{
		Method: connectMethod,
		Host:   "example.com:443",
		Header: http.Header{},
	}
	p.ServeHTTP(w, req)

	if w.code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.code, http.StatusInternalServerError)
	}
	if !strings.Contains(w.body.String(), "hijack not supported") {
		t.Errorf("body = %q, want substring 'hijack not supported'", w.body.String())
	}
}

// nonHijackingRecorder is an http.ResponseWriter that
// deliberately does NOT implement http.Hijacker. httptest's
// recorder type implements Hijacker on some Go versions and
// not others; a hand-rolled fake keeps the test stable.
type nonHijackingRecorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (r *nonHijackingRecorder) Header() http.Header { return r.header }
func (r *nonHijackingRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *nonHijackingRecorder) WriteHeader(code int) { r.code = code }

// TestServeOneSurfacesWritebackError covers the writeback-err
// path in serveOne: upstream replies fine, but writing the
// response back to the client fails (e.g. client TLS conn
// closed mid-response). The proxy must log it and not panic.
func TestServeOneSurfacesWritebackError(t *testing.T) {
	body := []byte("body-bytes-here\n")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)

	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &bytes.Buffer{}
	p := newProxy(certs, true, logSink)
	upURL, _ := url.Parse(upstream.URL)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		upstream.URL+"/file", http.NoBody)
	req.URL.Host = upURL.Host
	req.URL.Scheme = "https"
	req.RequestURI = ""

	writer := &failingWriter{failAfter: 0}
	p.serveOne(writer, req)

	if !strings.Contains(logSink.String(), "writeback-err=") {
		t.Errorf("expected writeback-err in log; got %q", logSink.String())
	}
}

// failingWriter is io.Writer that returns an error on the
// first Write. Used to exercise serveOne's writeback-error
// branch.
type failingWriter struct {
	writes    int
	failAfter int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errFake("forced writeback failure")
	}
	return len(p), nil
}

// TestCertErrorPaths exercises the crypto failure branches in
// newCertCache + leafForHost via the package-level seams. These
// paths are otherwise unreachable — rsa.GenerateKey,
// x509.CreateCertificate, and rand.Int effectively never fail
// on a healthy machine — but they must propagate the error
// rather than panic or return a half-built cert.
func TestCertErrorPaths(t *testing.T) {
	restore := func() {
		rsaGenerateKey = rsa.GenerateKey
		randomSerialFn = randomSerialDefault
		createCertFn = x509.CreateCertificate
	}
	t.Cleanup(restore)

	t.Run("rsa key gen fails", func(t *testing.T) {
		t.Cleanup(restore)
		rsaGenerateKey = func(io.Reader, int) (*rsa.PrivateKey, error) {
			return nil, errFake("rsa boom")
		}
		if _, err := newCertCache(); err == nil {
			t.Error("want error from newCertCache when rsa gen fails")
		}
	})

	t.Run("serial gen fails", func(t *testing.T) {
		t.Cleanup(restore)
		randomSerialFn = func() (*big.Int, error) { return nil, errFake("serial boom") }
		if _, err := newCertCache(); err == nil {
			t.Error("want error from newCertCache when serial gen fails")
		}
	})

	t.Run("ca cert creation fails", func(t *testing.T) {
		t.Cleanup(restore)
		createCertFn = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
			return nil, errFake("createcert boom")
		}
		if _, err := newCertCache(); err == nil {
			t.Error("want error from newCertCache when CreateCertificate fails")
		}
	})

	t.Run("leaf key gen fails", func(t *testing.T) {
		t.Cleanup(restore)
		c, err := newCertCache()
		if err != nil {
			t.Fatalf("newCertCache: %v", err)
		}
		rsaGenerateKey = func(io.Reader, int) (*rsa.PrivateKey, error) {
			return nil, errFake("leaf rsa boom")
		}
		if _, lerr := c.leafForHost("example.com"); lerr == nil {
			t.Error("want error from leafForHost when leaf rsa gen fails")
		}
	})

	t.Run("leaf cert creation fails", func(t *testing.T) {
		t.Cleanup(restore)
		c, err := newCertCache()
		if err != nil {
			t.Fatalf("newCertCache: %v", err)
		}
		createCertFn = func(io.Reader, *x509.Certificate, *x509.Certificate, any, any) ([]byte, error) {
			return nil, errFake("leaf createcert boom")
		}
		if _, lerr := c.leafForHost("leaf-fail.example"); lerr == nil {
			t.Error("want error from leafForHost when leaf CreateCertificate fails")
		}
	})
}

// startCachingHarness wires a TLS upstream (serving via h, wrapped
// in a hit counter), a proxy with caching enabled at cacheDir, and a
// client that trusts the proxy's CA. Returns the client, the https
// target it should GET, the proxy log sink, and the upstream hit
// counter — so a test can prove a replay never touched upstream.
func startCachingHarness(
	t *testing.T, cacheDir string, offline bool, h http.HandlerFunc,
) (*http.Client, string, *syncBuffer, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(upstream.Close)

	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)
	cache, cerr := httpcache.Open(cacheDir, offline, 0)
	if cerr != nil {
		t.Fatalf("cache open: %v", cerr)
	}
	p.cache = cache

	upURL, _ := url.Parse(upstream.URL)
	_, port, _ := net.SplitHostPort(upURL.Host)
	p.upstream.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, pt, _ := net.SplitHostPort(addr)
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+pt)
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	proxySrv := &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = proxySrv.Serve(proxyLn) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxySrv.Shutdown(ctx)
	})

	pool := x509.NewCertPool()
	pool.AddCert(certs.caCert)
	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	client := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}}
	return client, "https://localhost:" + port + "/pkg", logSink, &hits
}

// doGet performs one GET and returns status + body.
func doGet(t *testing.T, client *http.Client, target string) (status int, body []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// TestCacheRecordAndReplay is slice I1's headline behaviour: the
// first GET is fetched + stored, the second is served from the cache
// without touching upstream, and both bodies are byte-identical.
func TestCacheRecordAndReplay(t *testing.T) {
	body := []byte("cached-upstream-body-content\n")
	client, target, logSink, hits := startCachingHarness(t, t.TempDir(), false,
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })

	if st, got := doGet(t, client, target); st != 200 || !bytes.Equal(got, body) {
		t.Fatalf("first GET: status=%d body=%q, want 200 / %q", st, got, body)
	}
	if !logSink.waitForContains(t, "STORE", 2*time.Second) {
		t.Errorf("no STORE in log: %q", logSink.String())
	}

	if st, got := doGet(t, client, target); st != 200 || !bytes.Equal(got, body) {
		t.Fatalf("second GET: status=%d body=%q, want 200 / %q", st, got, body)
	}
	if !logSink.waitForContains(t, " HIT", 2*time.Second) {
		t.Errorf("no HIT in log: %q", logSink.String())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hit %d times, want 1 (replay must not refetch)", n)
	}
}

// TestCacheOfflineMiss: offline mode with an empty cache fails the
// request with 504 rather than reaching upstream.
func TestCacheOfflineMiss(t *testing.T) {
	client, target, logSink, hits := startCachingHarness(t, t.TempDir(), true,
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) })

	if st, _ := doGet(t, client, target); st != http.StatusGatewayTimeout {
		t.Errorf("offline miss status = %d, want 504", st)
	}
	if !logSink.waitForContains(t, "MISS offline", 2*time.Second) {
		t.Errorf("no offline-miss log: %q", logSink.String())
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream hit %d times in offline mode, want 0", n)
	}
}

// TestCacheNonOKPassThrough: a non-200 response is passed through
// untouched and NOT cached, so a transient error never poisons the
// cache (the second request hits upstream again).
func TestCacheNonOKPassThrough(t *testing.T) {
	client, target, _, hits := startCachingHarness(t, t.TempDir(), false,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })

	if st, _ := doGet(t, client, target); st != http.StatusNotFound {
		t.Errorf("first status = %d, want 404", st)
	}
	if st, _ := doGet(t, client, target); st != http.StatusNotFound {
		t.Errorf("second status = %d, want 404", st)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream hit %d times, want 2 (404 must not be cached)", n)
	}
}

// TestServeFromCacheErrors covers serveFromCache's two failure
// branches: a writeback error (client conn died mid-response) and a
// cache-read error (the index references a body the CAS no longer
// has). Both must log and emit a 502, not panic.
func TestServeFromCacheErrors(t *testing.T) {
	dir := t.TempDir()
	certs, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	logSink := &syncBuffer{}
	p := newProxy(certs, true, logSink)
	cache, cerr := httpcache.Open(dir, false, 0)
	if cerr != nil {
		t.Fatalf("cache open: %v", cerr)
	}
	p.cache = cache
	entry, perr := cache.Put("k", http.StatusOK, http.Header{}, strings.NewReader("hello"))
	if perr != nil {
		t.Fatalf("Put: %v", perr)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://example.invalid/x", http.NoBody)

	p.serveFromCache(&failingWriter{failAfter: 0}, req, "HIT", entry)
	if !logSink.waitForContains(t, "writeback-err", 2*time.Second) {
		t.Errorf("no writeback-err in log: %q", logSink.String())
	}

	// Drop the blob so Body() fails on the next replay.
	if rerr := os.RemoveAll(filepath.Join(dir, "blobs")); rerr != nil {
		t.Fatalf("remove blobs: %v", rerr)
	}
	var buf bytes.Buffer
	p.serveFromCache(&buf, req, "HIT", entry)
	if !strings.Contains(buf.String(), "502") {
		t.Errorf("want 502 on cache-read error; got %q", buf.String())
	}
	if !logSink.waitForContains(t, "cache-read-err", 2*time.Second) {
		t.Errorf("no cache-read-err in log: %q", logSink.String())
	}
}

// TestRunCacheOpenError covers run()'s fail-fast when --cache-dir
// can't be opened (here, its parent is a regular file).
func TestRunCacheOpenError(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	var out bytes.Buffer
	code := run([]string{ephemeralAddrFlag, "--cache-dir=" + filepath.Join(parent, "c")}, &out)
	if code == 0 {
		t.Errorf("code = 0; want non-zero")
	}
	if !strings.Contains(out.String(), "cache:") {
		t.Errorf("missing 'cache:' err in out: %q", out.String())
	}
}

// TestRunStartsWithCacheDir covers run()'s cache-open success path:
// with a valid --cache-dir the proxy opens the cache and reaches the
// listening state. Runs in a goroutine since Serve blocks; the
// listener is reclaimed at process exit (same pattern as
// TestRunWritesCAToDisk).
func TestRunStartsWithCacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	out := &syncBuffer{}
	go func() {
		_ = run([]string{
			ephemeralAddrFlag,
			"--cache-dir=" + cacheDir,
			"--cache-max-bytes=4096",
		}, out)
	}()
	if !out.waitForContains(t, readyPrefix, 5*time.Second) {
		t.Fatalf("proxy did not reach listening state with cache; out=%q", out.String())
	}
}

// TestLoadOrCreateCertCacheEphemeral: no --ca-dir mints a fresh CA.
func TestLoadOrCreateCertCacheEphemeral(t *testing.T) {
	cc, err := loadOrCreateCertCache("")
	if err != nil {
		t.Fatalf("loadOrCreateCertCache(\"\"): %v", err)
	}
	if cc == nil || cc.caCert == nil || cc.caKey == nil {
		t.Fatal("ephemeral CA not fully built")
	}
}

// TestPersistentCAReloads is I2's headline: a CA minted on the first
// call is written to --ca-dir and the second call reloads the *same*
// cert + key rather than minting a new one — so the sandbox's trust
// anchor survives a proxy restart.
func TestPersistentCAReloads(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateCertCache(dir)
	if err != nil {
		t.Fatalf("first loadOrCreate: %v", err)
	}
	for _, n := range []string{caCertName, caKeyName} {
		if _, serr := os.Stat(filepath.Join(dir, n)); serr != nil {
			t.Errorf("expected %s persisted: %v", n, serr)
		}
	}
	second, err := loadOrCreateCertCache(dir)
	if err != nil {
		t.Fatalf("second loadOrCreate: %v", err)
	}
	if !bytes.Equal(first.caCertPEM, second.caCertPEM) {
		t.Error("reloaded CA cert differs from persisted one")
	}
	if first.caKey.N.Cmp(second.caKey.N) != 0 {
		t.Error("reloaded CA key differs from persisted one")
	}
}

// TestPersistentCARegeneratesWhenKeyMissing: a stray cert with no key
// is a partial state, treated as missing → mint fresh + persist both.
func TestPersistentCARegeneratesWhenKeyMissing(t *testing.T) {
	dir := t.TempDir()
	seed, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	if werr := os.WriteFile(filepath.Join(dir, caCertName), seed.caCertPEM, 0o644); werr != nil {
		t.Fatalf("seed cert: %v", werr)
	}
	if _, err := loadOrCreateCertCache(dir); err != nil {
		t.Fatalf("loadOrCreate with missing key: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, caKeyName)); serr != nil {
		t.Errorf("key not persisted on regenerate: %v", serr)
	}
}

// TestPersistentCACorruptCert: a present-but-malformed CA is a hard
// error, not a silent regenerate (that would mask a trust mismatch).
func TestPersistentCACorruptCert(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, caCertName), "not a pem block")
	writeFile(t, filepath.Join(dir, caKeyName), "not a pem block")
	if _, err := loadOrCreateCertCache(dir); err == nil {
		t.Error("want error on corrupt CA cert; got nil")
	}
}

// TestPersistentCACorruptKey covers the key-parse branch: a valid
// cert beside a malformed key.
func TestPersistentCACorruptKey(t *testing.T) {
	dir := t.TempDir()
	seed, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	if werr := os.WriteFile(filepath.Join(dir, caCertName), seed.caCertPEM, 0o644); werr != nil {
		t.Fatalf("seed cert: %v", werr)
	}
	writeFile(t, filepath.Join(dir, caKeyName),
		"-----BEGIN RSA PRIVATE KEY-----\nbad\n-----END RSA PRIVATE KEY-----\n")
	if _, err := loadOrCreateCertCache(dir); err == nil {
		t.Error("want error on corrupt CA key; got nil")
	}
}

// TestPersistentCAMkdirError: a --ca-dir whose parent is a regular
// file can't be created, so persistence (and the call) fails.
func TestPersistentCAMkdirError(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := loadOrCreateCertCache(filepath.Join(parent, "ca")); err == nil {
		t.Error("want error when ca-dir can't be created; got nil")
	}
}

// TestPersistentCAWriteError covers persist's write-failure branch:
// the ca-dir exists but is read-only, so writing the key into it
// fails after MkdirAll succeeds.
func TestPersistentCAWriteError(t *testing.T) {
	dir := t.TempDir()
	//nolint:gosec // G302: deliberately read-only dir to force a write error.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	//nolint:gosec // G302: restore writability for t.TempDir cleanup.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := loadOrCreateCertCache(dir); err == nil {
		t.Error("want error persisting into read-only ca-dir; got nil")
	}
}

// TestPersistentCAKeyNoPEMBlock covers loadCertCache's keyBlock==nil
// branch: a valid cert beside a key file that holds no PEM block.
func TestPersistentCAKeyNoPEMBlock(t *testing.T) {
	dir := t.TempDir()
	seed, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	if werr := os.WriteFile(filepath.Join(dir, caCertName), seed.caCertPEM, 0o644); werr != nil {
		t.Fatalf("seed cert: %v", werr)
	}
	writeFile(t, filepath.Join(dir, caKeyName), "no pem block here")
	if _, err := loadOrCreateCertCache(dir); err == nil {
		t.Error("want error: key has no PEM block; got nil")
	}
}

// TestPersistentCACertBadDER covers loadCertCache's ParseCertificate
// error branch: a well-formed PEM block wrapping non-certificate DER.
func TestPersistentCACertBadDER(t *testing.T) {
	dir := t.TempDir()
	badCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-valid-der")})
	if werr := os.WriteFile(filepath.Join(dir, caCertName), badCert, 0o644); werr != nil {
		t.Fatalf("seed cert: %v", werr)
	}
	writeFile(t, filepath.Join(dir, caKeyName), "present-but-unread")
	if _, err := loadOrCreateCertCache(dir); err == nil {
		t.Error("want error: cert is not valid DER; got nil")
	}
}

// TestPersistentCARegenerateError covers loadOrCreateCertCache's
// branch where the on-disk CA is absent and minting a fresh one fails
// (forced via the rsaGenerateKey seam).
func TestPersistentCARegenerateError(t *testing.T) {
	old := rsaGenerateKey
	t.Cleanup(func() { rsaGenerateKey = old })
	rsaGenerateKey = func(io.Reader, int) (*rsa.PrivateKey, error) {
		return nil, errFake("forced regenerate failure")
	}
	if _, err := loadOrCreateCertCache(t.TempDir()); err == nil {
		t.Error("want error when regenerate's newCertCache fails; got nil")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLeafForHostIPAddress pins that an IP literal host
// produces a cert with the IP in IPAddresses rather than
// DNSNames. The integration wiring will likely point the VM at
// the host's LAN IP literal (not a DNS name), so this path
// must work.
func TestLeafForHostIPAddress(t *testing.T) {
	c, err := newCertCache()
	if err != nil {
		t.Fatalf("newCertCache: %v", err)
	}
	leaf, err := c.leafForHost("192.168.1.50")
	if err != nil {
		t.Fatalf("leafForHost: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(parsed.IPAddresses) == 0 {
		t.Errorf("leaf has no IPAddresses; got DNSNames=%v", parsed.DNSNames)
	}
	if len(parsed.IPAddresses) > 0 && parsed.IPAddresses[0].String() != "192.168.1.50" {
		t.Errorf("leaf IPAddresses[0] = %s, want 192.168.1.50", parsed.IPAddresses[0])
	}
}
