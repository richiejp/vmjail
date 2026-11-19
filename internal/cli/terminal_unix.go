//go:build linux || darwin

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// StdinTerminal wires the real termios + SIGWINCH backend
// against os.Stdin.
func StdinTerminal() *Terminal {
	return NewTerminal(int(os.Stdin.Fd()))
}

// NewTerminal probes fd for a termios and constructs the
// local terminal backend. Split out so tests can hand it a real
// PTY slave fd rather than relying on os.Stdin being a tty.
func NewTerminal(fd int) *Terminal {
	return &Terminal{fd: fd, isTTY: term.IsTerminal(fd)}
}

// Terminal is the local terminal backend. Winsize and
// SIGWINCH stay on x/sys/unix because x/term doesn't expose either.
type Terminal struct {
	fd    int
	isTTY bool
}

func (t *Terminal) IsTTY() bool { return t.isTTY }

func (t *Terminal) Winsize() (cols, rows uint32, err error) {
	w, h, err := term.GetSize(t.fd)
	if err != nil {
		return 0, 0, fmt.Errorf("term.GetSize: %w", err)
	}
	//nolint:gosec // G115: x/term returns int(uint16) — never negative or oversized.
	return uint32(w), uint32(h), nil
}

func (t *Terminal) MakeRaw() (func() error, error) {
	if !t.isTTY {
		return func() error { return nil }, nil
	}
	state, err := term.MakeRaw(t.fd)
	if err != nil {
		return nil, fmt.Errorf("term.MakeRaw: %w", err)
	}
	return func() error {
		if rerr := term.Restore(t.fd, state); rerr != nil {
			return fmt.Errorf("term.Restore: %w", rerr)
		}
		return nil
	}, nil
}

// Resize wires a SIGWINCH watcher and returns a chan that fires
// once per signal. Cancels its signal subscription on ctx done.
// The channel is closed when the subscription tears down so the
// pump goroutine exits via the `!ok` branch.
func (t *Terminal) Resize(ctx context.Context) <-chan struct{} {
	if !t.isTTY {
		return nil
	}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, unix.SIGWINCH)
	out := make(chan struct{}, 4)
	go func() {
		defer signal.Stop(sigCh)
		defer close(out)
		//vmjail:lint:disable=forloopbound reason="bounded by ctx cancellation / sigCh close on shutdown"
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
