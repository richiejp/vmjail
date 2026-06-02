//go:build !linux

package agent

// ensureTracefs is a no-op off Linux: the gadget only loads on Linux
// (opensnoop.Run returns ErrUnsupported elsewhere), and this stub keeps
// the cross-platform build green.
func ensureTracefs() error { return nil }

// readSelfPIDNS is unsupported off Linux. The gadget never loads here, so
// the namespace filter has no meaning; returning 0 keeps the cross-platform
// build green and would disable the filter were it ever consulted.
func readSelfPIDNS() (uint32, error) { return 0, nil }
