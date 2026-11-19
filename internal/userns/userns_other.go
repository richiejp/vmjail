//go:build !linux

package userns

import "io"

// Reexec is a no-op on non-Linux platforms: user namespaces are
// a Linux-only mechanism. Returning nil keeps the call site in
// main() portable; vmjail-sandboxd doesn't actually build on
// non-Linux today, but the stub avoids a build break the moment
// the rest of the binary picks up Darwin support for local dev.
func Reexec(_ io.Writer) error {
	return nil
}
