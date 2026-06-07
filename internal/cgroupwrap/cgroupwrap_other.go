//go:build !linux

package cgroupwrap

import "io"

// EnsureDelegated is a no-op on non-Linux platforms: cgroup v2
// delegation is a Linux-only mechanism. Returning nil keeps the call
// site in main() portable; daft-sandboxd doesn't build on non-Linux
// today, but the stub avoids a build break if the rest of the binary
// picks up Darwin support for local dev.
func EnsureDelegated(_ io.Writer) error {
	return nil
}
