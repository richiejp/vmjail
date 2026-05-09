// Package sandbox boots and supervises sandbox VMs by driving the
// configured VMM (Cloud Hypervisor in v1) and ferrying their console
// output back to the caller. The host-side `daft sandbox` commands in
// internal/cli call into this package.
//
// The public surface is deliberately VMM-agnostic so we can swap the
// backend behind the same interface (Firecracker, QEMU q35) — see PLAN.md
// "MicroVM stack".
package sandbox

import (
	"context"
	"errors"
	"io"
)

// ErrNotWired marks a sandbox operation that has not yet been implemented.
// It lets the CLI surface a clear, stable error while the microVM stack is
// being built up slice by slice; integration tests assert against it so a
// premature implementation can't silently pass.
var ErrNotWired = errors.New("sandbox: not yet wired")

// Hello boots a minimal sandbox VM, captures its console output, and
// reports success once the in-VM agent prints its hello banner.
//
// Currently a stub that returns ErrNotWired — the VMM plumbing lands in a
// later slice, by which point this signature should be stable.
func Hello(_ context.Context, _ io.Writer) error {
	return ErrNotWired
}
