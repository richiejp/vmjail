package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRunFetchesHTTPOK pins the happy plaintext-HTTP path: a 200
// response with a known body lands as a "daft-http-fetch ok ..."
// line with the right status, byte count, and sha256.
func TestRunFetchesHTTPOK(t *testing.T) {
	body := []byte("hello-from-test\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	code := run([]string{"--url=" + srv.URL}, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0; out=%q", code, out.String())
	}
	wantSHA := sha256.Sum256(body)
	want := fmt.Sprintf("daft-http-fetch ok status=200 bytes=%d sha256=%s\n",
		len(body), hex.EncodeToString(wantSHA[:]))
	if got := out.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestRunFetchesHTTPSWithInsecure proves --insecure lets the
// client trust httptest.NewTLSServer's self-signed cert. The
// slice-4 integration test depends on this path.
func TestRunFetchesHTTPSWithInsecure(t *testing.T) {
	body := []byte("https-body\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	code := run([]string{"--url=" + srv.URL, "--insecure"}, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "daft-http-fetch ok status=200") {
		t.Errorf("missing ok sentinel: %q", out.String())
	}
}

// TestRunRejectsHTTPSWithoutInsecure confirms the default
// (verify-on) path rejects the test server's self-signed cert —
// otherwise --insecure would be a no-op and the integration test
// could pass by accident.
func TestRunRejectsHTTPSWithoutInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("nope"))
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	code := run([]string{"--url=" + srv.URL}, &out)
	if code != 1 {
		t.Fatalf("code = %d, want 1; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "daft-http-fetch err:") {
		t.Errorf("missing err prefix: %q", out.String())
	}
}

// TestRunRejectsMissingURL covers the --url==” branch.
func TestRunRejectsMissingURL(t *testing.T) {
	var out bytes.Buffer
	code := run(nil, &out)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "--url is required") {
		t.Errorf("missing url-required message: %q", out.String())
	}
}

// TestRunRejectsBadFlags covers the flag.Parse error branch.
func TestRunRejectsBadFlags(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--no-such-flag"}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

// TestRunReportsGetError covers the client.Get error branch: a
// URL whose host doesn't resolve fails before any bytes leave.
func TestRunReportsGetError(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--url=http://daft-fetch-no-such-host.invalid/"}, &out)
	if code != 1 {
		t.Fatalf("code = %d, want 1; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "daft-http-fetch err: get:") {
		t.Errorf("missing get-error prefix: %q", out.String())
	}
}

// TestUsagePrintsBanner exercises the fs.Usage branch the
// --help path triggers. Flag-parse on --help returns
// flag.ErrHelp; run() exits with code 2 (parse error).
func TestUsagePrintsBanner(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--help"}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "Usage of daft-http-fetch") {
		t.Errorf("missing usage banner: %q", out.String())
	}
}

// TestMainCallsRunWithOsArgs pins the trivial main() wrapper.
// Without it main() stays uncovered and drags the per-file gate.
func TestMainCallsRunWithOsArgs(t *testing.T) {
	oldExit, oldOut, oldArgs := osExit, stdout, os.Args
	defer func() { osExit, stdout, os.Args = oldExit, oldOut, oldArgs }()
	os.Args = []string{"daft-http-fetch", "--no-such-flag"}
	var buf bytes.Buffer
	stdout = &buf
	code := -1
	osExit = func(c int) { code = c }
	main()
	if code != 2 {
		t.Errorf("code = %d, want 2 (bad-flag exit)", code)
	}
}

// TestNewClientFallback covers the rare "DefaultTransport is not
// *http.Transport" branch by stubbing the package-level transport
// before calling newClient. Without this stub the assertion always
// holds and the fallback stays dark.
func TestNewClientFallback(t *testing.T) {
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	http.DefaultTransport = stubRoundTripper{}
	c := newClient(true, nil)
	if c == nil || c.Transport == nil {
		t.Fatal("newClient returned nil client/transport on fallback")
	}
	if _, ok := c.Transport.(*http.Transport); !ok {
		t.Errorf("fallback transport not *http.Transport: %T", c.Transport)
	}
}

type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("stubRoundTripper: unused")
}

// TestRunRoutesThroughProxy proves --proxy makes the request go via
// the given proxy: a stand-in proxy server records the CONNECT/req
// it receives. Here a plain-HTTP proxy returns the body itself, so a
// matching sentinel can only mean the request traversed the proxy.
func TestRunRoutesThroughProxy(t *testing.T) {
	body := []byte("via-proxy-body\n")
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Store(true)
		_, _ = w.Write(body)
	}))
	t.Cleanup(proxy.Close)

	var out bytes.Buffer
	// Target host is unroutable; the only way a 200 comes back is
	// through the proxy, which answers every request with body.
	code := run([]string{
		"--url=http://daft-fetch-no-such-host.invalid/",
		"--proxy=" + proxy.URL,
	}, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0; out=%q", code, out.String())
	}
	if !proxied.Load() {
		t.Error("proxy was not used")
	}
	wantSHA := sha256.Sum256(body)
	if !strings.Contains(out.String(), hex.EncodeToString(wantSHA[:])) {
		t.Errorf("body sha not from proxy: %q", out.String())
	}
}

// TestRunRejectsBadProxy covers the --proxy parse-error branch.
func TestRunRejectsBadProxy(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--url=http://example.invalid/", "--proxy=://no-scheme"}, &out)
	if code != 1 {
		t.Fatalf("code = %d, want 1; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "bad --proxy") {
		t.Errorf("missing bad-proxy message: %q", out.String())
	}
}
