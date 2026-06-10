//go:build !linux

package agent

import "io"

// raiseGuestRlimits is a no-op off Linux (the agent only runs in a Linux
// microVM; this keeps the package building on dev hosts).
func raiseGuestRlimits(io.Writer) {}
