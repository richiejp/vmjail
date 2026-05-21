// daft-http-fetch is a tiny static HTTPS-GET workload for the
// integration tests: it dials a URL, hashes the body, and prints
// a single readiness-sentinel line the test waits on. Baked into
// the nix-shell-image so the test can `daft workspace exec` it
// against an httptest.NewTLSServer on the host's loopback,
// reached through sandboxnet's DefaultHostLoopbackIP NAT.
//
// Flags:
//
//	--url=...       URL to GET (required)
//	--insecure      skip TLS verification (for self-signed test certs)
//
// On success prints
//
//	daft-http-fetch ok status=<code> bytes=<n> sha256=<hex>
//
// On any error prints
//
//	daft-http-fetch err: <message>
//
// and exits 1. The sentinel format keeps the test from having to
// parse HTTP bodies; sha256 lets the test cross-check that the
// bytes the container received match the bytes the host wrote.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/localai-org/darkaifactory/internal/banner"
)

const readyPrefix = "daft-http-fetch ok"

//nolint:gochecknoglobals // test seams.
var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
)

func main() {
	osExit(run(os.Args[1:], stdout))
}

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("daft-http-fetch", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = fmt.Fprint(out, banner.String)
		_, _ = fmt.Fprintln(out, "\nUsage of daft-http-fetch:")
		fs.PrintDefaults()
	}
	url := fs.String("url", "", "URL to GET (required)")
	insecure := fs.Bool("insecure", false,
		"skip TLS certificate verification (for self-signed test certs)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		_, _ = fmt.Fprintln(out, "daft-http-fetch err: --url is required")
		return 1
	}
	client := newClient(*insecure)
	//nolint:noctx // a one-shot CLI tool; no parent context.
	resp, err := client.Get(*url)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-http-fetch err: get: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-http-fetch err: read body: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "%s status=%d bytes=%d sha256=%s\n",
		readyPrefix, resp.StatusCode, n, hex.EncodeToString(h.Sum(nil)))
	return 0
}

// newClient builds the http.Client used by run. When insecure is
// false the cloned transport keeps Go's default TLSClientConfig
// (system CAs, hostname verification); only --insecure swaps in
// an InsecureSkipVerify config — leaving the verify-on path
// untouched so a future CA-bundle wiring doesn't get silently
// undone by an unconditional override here.
//
//nolint:gosec // G402: InsecureSkipVerify is gated on the --insecure flag.
func newClient(insecure bool) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	tr := base.Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Transport: tr}
}
