//go:build !linux

package agent

import (
	"errors"
	"os"
)

var errTapUnsupported = errors.New("agent: tap device unsupported on this platform")

func openTapLinux(_ string) (*os.File, string, error) {
	return nil, "", errTapUnsupported
}

func configureTapLinux(_, _, _ string, _ int) error {
	return errTapUnsupported
}
