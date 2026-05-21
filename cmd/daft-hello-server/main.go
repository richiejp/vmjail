// daft-hello-server is the D6b.4 dev-server proof workload: a
// tiny HTTP listener that responds 200 OK with a known body on
// every request. The integration test bakes it into the nix-
// shell-image, starts it via `daft workspace exec`, then drives
// `daft workspace forward` and HTTP-GETs the local forwarder to
// assert the body surfaces back out of the container.
//
// Flags:
//
//	--addr=:8080    listen address (default :8080)
//	--body=hello    response body (default "hello from daft-hello-server")
//
// On startup the server prints `daft-hello-server listening on
// <addr>` to stdout — same readiness-sentinel convention as
// daft-hello-container's banner, so the test can wait on a known
// string before driving the forward.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/localai-org/darkaifactory/internal/banner"
)

const (
	defaultAddr = ":8080"
	defaultBody = "hello from daft-hello-server\n"
	// readyPrefix is the line the test waits on before opening
	// a forward. The full line includes the bound addr (useful
	// when --addr=:0 picks an ephemeral port).
	readyPrefix = "daft-hello-server listening on "
)

//nolint:gochecknoglobals // test seams.
var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
	serve            = http.Serve
)

func main() {
	osExit(run(os.Args[1:], stdout))
}

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("daft-hello-server", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = fmt.Fprint(out, banner.String)
		_, _ = fmt.Fprintln(out, "\nUsage of daft-hello-server:")
		fs.PrintDefaults()
	}
	addr := fs.String("addr", defaultAddr, "listen address (host:port)")
	body := fs.String("body", defaultBody, "response body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintf(out, "daft-hello-server: listen %s: %v\n", *addr, err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "%s%s\n", readyPrefix, ln.Addr())
	body0 := *body
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body0)
	})
	if err := serve(ln, handler); err != nil {
		_, _ = fmt.Fprintf(out, "daft-hello-server: serve: %v\n", err)
		return 1
	}
	return 0
}
