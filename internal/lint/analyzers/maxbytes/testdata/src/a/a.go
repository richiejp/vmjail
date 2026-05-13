// Package a is the fixture for maxbytes' analysistest.
package a

import (
	"io"
	"net/http"

	"other"
)

// bad reads r.Body without a size cap; analyzer must fire.
func bad(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body) // want `MaxBytesReader`
}

// good wraps r.Body before reading; analyzer must not fire.
func good(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	_, _ = io.ReadAll(r.Body)
}

// goodFreshVar binds the wrapped body to a new variable; the
// MaxBytesReader call still appears in the function, so analyzer
// must not fire.
func goodFreshVar(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	_, _ = io.ReadAll(body)
}

// responseFine reads an http.Response.Body — that's a server
// response we already trust enough to call. Analyzer must not fire.
func responseFine() {
	resp, _ := http.Get("https://example.com")
	if resp == nil {
		return
	}
	_, _ = io.ReadAll(resp.Body)
}

// suppressed pins the directive silences the diagnostic.
func suppressed(_ http.ResponseWriter, r *http.Request) {
	//daft:lint:disable=maxbytes reason="fixture exercises suppression"
	_, _ = io.ReadAll(r.Body)
}

// otherReadAll uses io.ReadAll on something unrelated; analyzer
// must not fire.
func otherReadAll(rd io.Reader) {
	_, _ = io.ReadAll(rd)
}

// otherPackageReadAll calls a different package's ReadAll with
// r.Body; analyzer must not fire because the package-path check
// rejects it.
func otherPackageReadAll(_ http.ResponseWriter, r *http.Request) {
	_, _ = other.ReadAll(r.Body)
}

// nonSelectorCall demonstrates that a call whose Fun is not a
// SelectorExpr (a plain identifier) is rejected by calleeFunc;
// analyzer must not fire.
func nonSelectorCall() {
	doNothing()
}

func doNothing() {}

// readAllOfNonBody calls io.ReadAll on a field that happens to be
// named Body but on a non-http.Request type; analyzer must not
// fire because argIsRequestBody checks the receiver type.
type wrapper struct {
	Body io.Reader
}

func readAllOfNonBody(w wrapper) {
	_, _ = io.ReadAll(w.Body)
}
