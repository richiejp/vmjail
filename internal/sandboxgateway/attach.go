package sandboxgateway

import (
	"context"
	"errors"
	"net"
)

// ErrAttachNoRoute is returned by an AttachBridger when no
// session can be opened for the requested sandbox_uid. Same
// shape as ErrNoRoute on the port side.
var ErrAttachNoRoute = errors.New("sandboxgateway: no attach route for sandbox")

// AttachRequest is the dispatch payload carried from the
// orchestrator's AttachRequest event through to the sandboxd's
// AttachBridger.Attach call. Mirrors nodectrlv1.AttachRequest
// but is wire-free so this package stays transport-agnostic.
type AttachRequest struct {
	SessionID  string
	SandboxUID string
	Command    []string
	TTY        bool
	Cols, Rows uint32
}

// AttachSession is a live attach: a bidi byte stream into the
// running sandbox plus a side-channel for window-resize events.
// Bytes flow via net.Conn; resize is out-of-band so a partial
// data frame in flight is not corrupted by an interleaved
// window-size change.
type AttachSession interface {
	net.Conn
	// Resize forwards a window-resize control event to the
	// session's downstream PTY. A non-nil return tears the
	// session down (the caller must Close the net.Conn).
	Resize(cols, rows uint32) error
}

// AttachBridger opens an attach session into a running Sandbox
// on the local Node. Implementations live in the sandboxd
// (production: vsock to daft-agent — Slice D2b) and tests (fake
// in-process PTY). The interface is transport-free so this
// package never depends on the agent or the wire format.
type AttachBridger interface {
	Attach(ctx context.Context, req AttachRequest) (AttachSession, error)
}
