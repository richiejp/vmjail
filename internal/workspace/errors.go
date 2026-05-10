package workspace

import (
	"errors"
	"io/fs"

	"connectrpc.com/connect"
)

// mapStoreError translates an internal/cas error into the right
// Connect-RPC code. Missing chunks become CodeNotFound; everything
// else is CodeInternal — server-side problems that the client
// shouldn't try to recover from.
func mapStoreError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}
