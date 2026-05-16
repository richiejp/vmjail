//go:build !linux

package agent

import (
	"context"
	"errors"
	"net"
)

// DialHostVsock on non-Linux always errors — AF_VSOCK is a Linux
// kernel feature. The agent binary ships only for Linux (see
// flake.nix); this stub exists so the package compiles on macOS for
// editor tooling and cross-platform `go vet`.
func DialHostVsock(_ context.Context, _ uint32) (net.Conn, error) {
	return nil, errors.New("agent: vsock unsupported off Linux")
}

// vsockListenGuest on non-Linux always errors for the same reason
// as DialHostVsock — there's no AF_VSOCK off Linux.
func vsockListenGuest(_ uint32) (net.Listener, error) {
	return nil, errors.New("agent: vsock listen unsupported off Linux")
}
