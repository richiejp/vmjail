//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/richiejp/vmjail/internal/agent"
	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"golang.org/x/sys/unix"
)

// RelayCommand relays an initial guest command and owns conn. Callers supplying
// input other than a file or connection must ensure any blocked Read can return
// after the command exits.
func RelayCommand(ctx context.Context, conn net.Conn, in io.Reader, out, errout io.Writer) (resultErr error) {
	defer func() { _ = conn.Close() }()                         // Also close on handshake or terminal setup failure.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // Wake reads and writes on cancellation.
	defer stop()

	hello := &agentv1.AgentAttachHello{}
	if file, ok := in.(*os.File); ok {
		terminal := NewTerminal(int(file.Fd()))
		if terminal.IsTTY() {
			cols, rows, err := terminal.Winsize()
			if err != nil {
				return err
			}
			restore, err := terminal.MakeRaw()
			if err != nil {
				return err
			}
			defer func() {
				if err := restore(); err != nil {
					resultErr = errors.Join(resultErr, err)
				}
			}()
			hello.Tty, hello.Cols, hello.Rows = true, cols, rows
		}
	}

	if err := agent.WriteAttachHello(conn, hello); err != nil {
		return err
	}

	inputCtx, cancelInput := context.WithCancel(ctx)
	inputDone := make(chan struct{})
	inputErr := make(chan error, 1)
	go func() {
		defer close(inputDone)
		if err := pumpInput(inputCtx, conn, in); err != nil {
			inputErr <- err
			_ = conn.Close() // Unblock the output reader after a local input failure.
		}
	}()
	defer func() {
		cancelInput()
		_ = conn.Close() // Unblock a stdin write when the workload exits without reading it.
		waitForInput(in, inputDone)
	}()

	exit, err := agent.ReadJobOutput(conn, out, errout)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case inputFailure := <-inputErr:
		return inputFailure
	default:
	}
	if err != nil {
		return err
	}

	if exit.GetError() != "" {
		return errors.New(exit.GetError())
	}
	if exit.GetCode() != 0 {
		return &ExitError{int(exit.GetCode())}
	}
	return nil
}

// waitForInput joins pumps whose Read can be interrupted. A generic
// io.Reader's pump exits once its caller releases the pending Read.
func waitForInput(in io.Reader, done <-chan struct{}) {
	switch input := in.(type) {
	case net.Conn:
		if err := input.SetReadDeadline(time.Now()); err != nil {
			_ = input.Close() // The listener owns this client for a single command.
		}
		<-done
	case *os.File:
		<-done // Poll checks cancellation every 100 ms.
	}
}

func pumpInput(ctx context.Context, conn io.Writer, in io.Reader) error {
	buf := make([]byte, 32*1024)
	file, isFile := in.(*os.File)
	var polls [1]unix.PollFd
	if isFile {
		// #nosec G115 -- Linux file descriptors are signed 32-bit integers.
		polls[0] = unix.PollFd{Fd: int32(file.Fd()), Events: unix.POLLIN}
	}

	for ctx.Err() == nil {
		if isFile {
			n, err := unix.Poll(polls[:], 100)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return fmt.Errorf("poll command stdin: %w", err)
			}
			if n == 0 {
				continue
			}
		}

		n, err := in.Read(buf)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // Session teardown supersedes the pending read error.
		}
		if n > 0 && agent.WriteCommandInput(conn, buf[:n]) != nil {
			// The output reader reports guest disconnects. An exit
			// frame may already be queued when the write fails.
			return nil //nolint:nilerr // The output stream owns the result after a write failure.
		}

		if errors.Is(err, io.EOF) {
			// As with data writes, the output side reports disconnection.
			_ = agent.EndCommandInput(conn)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read command stdin: %w", err)
		}
	}
	return nil
}
