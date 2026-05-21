package orchestrator

import (
	"net/http"

	"github.com/localai-org/darkaifactory/internal/identity"
)

// peerFingerprintHeader is the internal header name that
// withPeerMiddleware stamps with the mTLS leaf-cert fingerprint
// of the connection's peer. Handlers read it via
// PeerFingerprintFromRequestHeader.
//
// The header is set by trusted server-side middleware AFTER mTLS
// has authenticated the peer, then OVERWRITES anything a
// misbehaving client might have set. So even though headers are
// client-controllable in general, this one is reliable on the
// server side.
//
// We use a request header rather than a context.Value because
// passing `r.WithContext(...)` through Connect-go's
// server-streaming handlers caused the request context to be
// cancelled almost immediately for reasons we haven't fully
// diagnosed (TestBrokerHappyPath failure: ctx.Done fires before
// the first event is pushed). Headers travel with the request
// without going through context-derivation, which sidesteps the
// issue and is also what Connect-go interceptors are designed
// to read.
const peerFingerprintHeader = "X-Daft-Peer-Fingerprint"

// PeerFingerprintFromRequestHeader extracts the authenticated
// peer's fingerprint from the request's headers. Connect-go
// handlers can reach it via `req.Header().Get(...)` directly,
// but this helper does the parse + validation in one place.
func PeerFingerprintFromRequestHeader(h http.Header) (identity.Fingerprint, bool) {
	s := h.Get(peerFingerprintHeader)
	if s == "" {
		return identity.Fingerprint{}, false
	}
	fp, err := identity.ParseFingerprint(s)
	if err != nil {
		return identity.Fingerprint{}, false
	}
	return fp, true
}

// withPeerMiddleware wraps h so that downstream handlers can call
// PeerFingerprintFromRequestHeader on the Connect request's
// headers to recover the authenticated peer's fingerprint. The
// middleware sets the header AFTER stripping any client-supplied
// value, so handlers can trust it.
//
// If the request has no TLS state or no peer cert, the header is
// removed and the request is passed through; handlers that
// require identity must reject in that case rather than
// substitute a default.
func withPeerMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(peerFingerprintHeader)
		if fp, ok := identity.PeerFingerprintFromTLS(r.TLS); ok {
			r.Header.Set(peerFingerprintHeader, fp.String())
		}
		h.ServeHTTP(w, r)
	})
}
