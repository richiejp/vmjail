// daft-mitm-proxy is the standalone entry point for the HTTPS-MITM
// forward proxy implemented in internal/mitmproxy. Its production use
// is in-process inside sandboxd (served on the per-VM netstack's
// gateway); this binary keeps the proxy reachable as a one-off
// investigation/recording tool, listening on a host TCP port and
// writing its CA cert out for a client to trust.
//
// Wire shape (standalone):
//
//	client --https_proxy--> daft-mitm-proxy on host
//	                          ↓ MITM (terminate + re-encrypt)
//	                        real cache.nixos.org / etc.
//
// Each request is logged as one line:
//
//	<ts> <method> <url> -> <status> bytes=<n> elapsed=<d>
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/localai-org/darkaifactory/internal/banner"
	"github.com/localai-org/darkaifactory/internal/httpcache"
	"github.com/localai-org/darkaifactory/internal/mitmproxy"
)

const (
	readyPrefix = "daft-mitm-proxy listening on "
	defaultAddr = "127.0.0.1:0"
)

//nolint:gochecknoglobals // test seams.
var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
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
	cacheMaxBytes := fs.Uint64("cache-max-bytes", 0,
		"cap the cache's total stored body size in bytes; over-cap "+
			"writes evict least-recently-used entries (0 = unbounded)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	certs, err := mitmproxy.LoadOrCreateCertCache(*caDir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: ca: %v\n", err)
		return 1
	}
	if *caCertOut != "" {
		if werr := os.WriteFile(*caCertOut, certs.CACertPEM(), 0o644); werr != nil { //nolint:gosec // CA cert is public.
			_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: write %s: %v\n", *caCertOut, werr)
			return 1
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: listen %s: %v\n", *addr, err)
		return 1
	}

	p := mitmproxy.New(certs, *insecureUpstream, out)
	if *cacheDir != "" {
		cache, cerr := httpcache.Open(*cacheDir, *offline, *cacheMaxBytes)
		if cerr != nil {
			_, _ = fmt.Fprintf(out, "daft-mitm-proxy err: cache: %v\n", cerr)
			return 1
		}
		p.SetCache(cache)
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
