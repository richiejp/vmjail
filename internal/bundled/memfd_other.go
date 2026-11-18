//go:build !linux

package bundled

import "errors"

func loadAssets(_, _, _, _ []byte) (Paths, error) {
	return Paths{}, errors.New("bundled: sealed memfd assets require Linux")
}
