//go:build linux

// Linux-only because the PTY allocation flow used in the tests
// (TIOCSPTLCK + TIOCGPTN against /dev/ptmx) is Linux-specific.
// macOS uses posix_openpt(3) + grantpt(3); rather than adding a
// macOS PTY helper just for tests, the production code path is
// covered on Linux only — both halves of darwin (newUnixExecTerminal
// + its methods) still compile and ship for editor tooling.

package cli

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openTestPTY opens a /dev/ptmx + /dev/pts/<n> pair and returns
// them. Same shape as the agent's openPtyPair, redeclared here
// so the cli test doesn't import the agent package just to reach
// a private helper.
func openTestPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open /dev/ptmx: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("ptsname: %v", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open slave: %v", err)
	}
	slave = os.NewFile(uintptr(sfd), slavePath)
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// TestUnixExecTerminalIsTTYProbesFd pins the constructor: an fd
// pointing at a real PTY reports IsTTY=true; an fd pointing at a
// regular file (e.g. an open /dev/null) reports IsTTY=false.
func TestUnixExecTerminalIsTTYProbesFd(t *testing.T) {
	_, slave := openTestPTY(t)
	if term := newUnixExecTerminal(int(slave.Fd())); !term.IsTTY() {
		t.Error("PTY slave should report IsTTY=true")
	}

	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if term := newUnixExecTerminal(int(f.Fd())); term.IsTTY() {
		t.Error("/dev/null should report IsTTY=false")
	}
}

// TestUnixExecTerminalWinsizeFromMaster sets a winsize via
// TIOCSWINSZ on the master and verifies the terminal reads it
// back through Winsize().
func TestUnixExecTerminalWinsizeFromMaster(t *testing.T) {
	master, slave := openTestPTY(t)
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 132, Row: 50}); err != nil {
		t.Fatalf("TIOCSWINSZ: %v", err)
	}
	term := newUnixExecTerminal(int(slave.Fd()))
	cols, rows, err := term.Winsize()
	if err != nil {
		t.Fatalf("Winsize: %v", err)
	}
	if cols != 132 || rows != 50 {
		t.Errorf("Winsize = (%d, %d), want (132, 50)", cols, rows)
	}
}

// TestUnixExecTerminalWinsizeOnRegularFileErrors pins the error
// path: ioctl on a non-tty fd surfaces a wrapped error.
func TestUnixExecTerminalWinsizeOnRegularFileErrors(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	term := newUnixExecTerminal(int(f.Fd()))
	if _, _, werr := term.Winsize(); werr == nil {
		t.Error("expected Winsize on non-tty to error")
	}
}

// TestUnixExecTerminalMakeRawRoundtrip puts the slave into raw
// mode, asserts ICANON is cleared, then restores and asserts the
// original flags came back.
func TestUnixExecTerminalMakeRawRoundtrip(t *testing.T) {
	_, slave := openTestPTY(t)
	fd := int(slave.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios: %v", err)
	}

	term := newUnixExecTerminal(fd)
	restore, err := term.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	raw, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios after MakeRaw: %v", err)
	}
	if raw.Lflag&unix.ICANON != 0 {
		t.Error("ICANON not cleared after MakeRaw")
	}
	if raw.Iflag&unix.ICRNL != 0 {
		t.Error("ICRNL not cleared after MakeRaw")
	}

	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios after restore: %v", err)
	}
	if restored.Lflag != original.Lflag || restored.Iflag != original.Iflag {
		t.Errorf("restored flags differ from original: orig.Lflag=%x got=%x", original.Lflag, restored.Lflag)
	}
}

// TestUnixExecTerminalMakeRawOnNonTTYNoOp pins the IsTTY=false
// branch: MakeRaw returns a no-op restore func and no error.
func TestUnixExecTerminalMakeRawOnNonTTYNoOp(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	term := newUnixExecTerminal(int(f.Fd()))
	restore, err := term.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if rerr := restore(); rerr != nil {
		t.Errorf("no-op restore returned err: %v", rerr)
	}
}

// TestUnixExecTerminalResizeNilForNonTTY pins the IsTTY=false
// branch: Resize returns nil so the pump goroutine never starts.
func TestUnixExecTerminalResizeNilForNonTTY(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	term := newUnixExecTerminal(int(f.Fd()))
	if ch := term.Resize(context.Background()); ch != nil {
		t.Errorf("Resize on non-tty = %v, want nil", ch)
	}
}

// TestUnixExecTerminalResizeFiresOnSIGWINCH delivers a SIGWINCH
// to the test process and asserts the chan fires.
func TestUnixExecTerminalResizeFiresOnSIGWINCH(t *testing.T) {
	_, slave := openTestPTY(t)
	term := newUnixExecTerminal(int(slave.Fd()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := term.Resize(ctx)
	if ch == nil {
		t.Fatal("Resize returned nil for a tty fd")
	}

	// Deliver SIGWINCH to ourselves. The terminal's signal.Notify
	// subscription is process-global; this is the canonical way
	// to drive a test through the same path.
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := p.Signal(unix.SIGWINCH); err != nil {
		t.Fatalf("Signal SIGWINCH: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGWINCH never observed on resize channel")
	}
}

// TestDefaultExecTerminalConstructs pins that defaultExecTerminal
// returns a non-nil concrete *unixExecTerminal (regardless of
// whether stdin is a tty under `go test`).
func TestDefaultExecTerminalConstructs(t *testing.T) {
	term := defaultExecTerminal()
	if _, ok := term.(*unixExecTerminal); !ok {
		t.Fatalf("defaultExecTerminal returned %T, want *unixExecTerminal", term)
	}
}
