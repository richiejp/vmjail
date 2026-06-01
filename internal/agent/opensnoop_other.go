//go:build !linux

package agent

// ensureTracefs is a no-op off Linux: the gadget only loads on Linux
// (opensnoop.Run returns ErrUnsupported elsewhere), and this stub keeps
// the cross-platform build green.
func ensureTracefs() error { return nil }
