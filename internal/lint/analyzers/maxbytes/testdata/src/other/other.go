// Package other defines a same-named ReadAll for fixture coverage
// of maxbytes' "wrong package" rejection branch.
package other

import "io"

// ReadAll is intentionally not io.ReadAll. A call site that uses
// this won't be flagged by maxbytes even when handed a
// *http.Request.Body — the package-path check rejects it.
func ReadAll(_ io.Reader) ([]byte, error) { return nil, nil }
