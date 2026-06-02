//go:build !linux

package opensnoop

import (
	"context"
	"errors"
	"io"
)

// ErrUnsupported is returned by Run off Linux. The gadget loads a BPF
// program against the guest kernel; there is nothing to run on other
// platforms. daft-agent — the only caller — is Linux-only, so this stub
// exists purely so the package still compiles cross-platform (gopls,
// `GOOS=darwin go build`).
var ErrUnsupported = errors.New("opensnoop: eBPF gadgets are only supported on Linux")

func Run(_ context.Context, _ io.Writer, _ uint32) error {
	return ErrUnsupported
}

// RunAggregating mirrors Run off Linux: there is no gadget to drain into the
// aggregator, so it reports the same unsupported error.
func RunAggregating(_ context.Context, _ io.Writer, _ *Aggregator, _ uint32) error {
	return ErrUnsupported
}
