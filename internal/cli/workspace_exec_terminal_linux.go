//go:build linux

package cli

import "golang.org/x/sys/unix"

// On Linux, termios is read/written via TCGETS/TCSETS. macOS uses
// TIOCGETA/TIOCSETA — see workspace_exec_terminal_darwin.go.
const (
	ioctlGetTermiosReq = unix.TCGETS
	ioctlSetTermiosReq = unix.TCSETS
)
