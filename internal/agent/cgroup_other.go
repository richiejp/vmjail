//go:build !linux

package agent

import "io"

// ensureCgroupV2 is a no-op off Linux (the agent only runs as PID 1 in
// the Linux sandbox VM; this stub keeps the cross-platform build green).
func ensureCgroupV2(io.Writer) {}
