//go:build linux || darwin

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// defaultExecTerminal wires the real termios + SIGWINCH backend
// against os.Stdin.
func defaultExecTerminal() execTerminal {
	return newUnixExecTerminal(int(os.Stdin.Fd()))
}

// newUnixExecTerminal probes fd for a termios and constructs the
// production execTerminal. Split out so tests can hand it a real
// PTY slave fd rather than relying on os.Stdin being a tty.
func newUnixExecTerminal(fd int) *unixExecTerminal {
	_, err := unix.IoctlGetTermios(fd, ioctlGetTermiosReq)
	return &unixExecTerminal{fd: fd, isTTY: err == nil}
}

// unixExecTerminal is the production execTerminal — IoctlGetTermios
// for raw-mode toggling, IoctlGetWinsize for the initial + per-
// SIGWINCH dimensions, signal.Notify(SIGWINCH) for the resize
// channel.
type unixExecTerminal struct {
	fd    int
	isTTY bool
}

func (t *unixExecTerminal) IsTTY() bool { return t.isTTY }

func (t *unixExecTerminal) Winsize() (cols, rows uint32, err error) {
	ws, werr := unix.IoctlGetWinsize(t.fd, unix.TIOCGWINSZ)
	if werr != nil {
		return 0, 0, fmt.Errorf("ioctl TIOCGWINSZ: %w", werr)
	}
	return uint32(ws.Col), uint32(ws.Row), nil
}

// MakeRaw saves the current termios, applies the standard raw-mode
// mask, and returns a restore func that tcsetattrs the original
// back. Mirrors golang.org/x/term.MakeRaw — the project deliberately
// avoids the extra dependency.
func (t *unixExecTerminal) MakeRaw() (func() error, error) {
	if !t.isTTY {
		return func() error { return nil }, nil
	}
	original, err := unix.IoctlGetTermios(t.fd, ioctlGetTermiosReq)
	if err != nil {
		return nil, fmt.Errorf("ioctl get termios: %w", err)
	}
	raw := *original
	// Match the standard raw-mode flags from POSIX cfmakeraw(3).
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(t.fd, ioctlSetTermiosReq, &raw); err != nil {
		return nil, fmt.Errorf("ioctl set termios: %w", err)
	}
	saved := *original
	return func() error {
		if err := unix.IoctlSetTermios(t.fd, ioctlSetTermiosReq, &saved); err != nil {
			return fmt.Errorf("ioctl restore termios: %w", err)
		}
		return nil
	}, nil
}

// Resize wires a SIGWINCH watcher and returns a chan that fires
// once per signal. Cancels its signal subscription on ctx done.
// The channel is closed when the subscription tears down so the
// pump goroutine exits via the `!ok` branch.
func (t *unixExecTerminal) Resize(ctx context.Context) <-chan struct{} {
	if !t.isTTY {
		return nil
	}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, unix.SIGWINCH)
	out := make(chan struct{}, 4)
	go func() {
		defer signal.Stop(sigCh)
		defer close(out)
		//daft:lint:disable=forloopbound reason="bounded by ctx cancellation / sigCh close on shutdown"
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sigCh:
				if !ok {
					return
				}
				// Non-blocking send: a slow reader on a flurry of
				// resizes shouldn't pin this goroutine.
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}
