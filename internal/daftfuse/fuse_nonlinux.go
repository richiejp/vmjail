//go:build !linux

package daftfuse

import (
	"context"
	"errors"
)

// Server is the non-linux stub of the linux Server type. Wait /
// Unmount return immediately so a caller can dial-through the
// same shape without build tags.
type Server struct{}

// Wait is a no-op on non-linux.
func (*Server) Wait() {}

// Unmount returns an "unsupported" sentinel.
func (*Server) Unmount() error { return errUnsupported }

var errUnsupported = errors.New("daftfuse: FUSE only supported on linux")

// Serve always errors on non-linux. daft-sandboxd is linux-only in
// production; this stub exists so the package compiles on darwin
// and other platforms for IDE + cross-platform tooling.
func (*Mount) Serve(_ context.Context, _ string) (*Server, error) {
	return nil, errUnsupported
}
