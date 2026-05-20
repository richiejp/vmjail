//go:build !linux

package agent

import (
	"errors"
	"os"
)

// errTapUnsupported keeps the agent package compiling on non-linux
// dev hosts. The daft-agent binary only runs inside the guest VM
// (Linux), so these stubs are never reached at runtime.
var errTapUnsupported = errors.New("agent: tap device unsupported on this platform")

func openTapLinux(_ string) (*os.File, string, error) {
	return nil, "", errTapUnsupported
}

func configureTapLinux(_, _, _ string, _ int) error {
	return errTapUnsupported
}
