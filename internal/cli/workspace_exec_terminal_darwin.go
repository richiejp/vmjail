//go:build darwin

package cli

import "golang.org/x/sys/unix"

// macOS reads/writes termios via TIOCGETA/TIOCSETA. Linux uses
// TCGETS/TCSETS — see workspace_exec_terminal_linux.go.
const (
	ioctlGetTermiosReq = unix.TIOCGETA
	ioctlSetTermiosReq = unix.TIOCSETA
)
