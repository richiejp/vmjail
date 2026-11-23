package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/agent"
	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/pty"
	"golang.org/x/sys/unix"
)

func TestRelay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int32
		message string
	}{
		{name: "success"},
		{name: "workload exit", code: 17},
		{name: "setup failure", code: 125, message: "setup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			done := make(chan error, 1)
			go func() {
				if _, err := agent.ReadAttachHello(server); err != nil {
					done <- err
					return
				}
				input, err := io.ReadAll(&agent.CommandInput{Conn: server})
				if err != nil {
					done <- err
					return
				}
				if string(input) != "stdin\x00" {
					done <- errors.New("stdin corruption")
					return
				}
				for _, f := range []*agentv1.JobFrame{{Body: &agentv1.JobFrame_Stdout{Stdout: []byte("out\x00")}}, {Body: &agentv1.JobFrame_Stderr{Stderr: []byte("err")}}, {Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{Code: tc.code, Error: tc.message}}}} {
					if err := agent.WriteJobFrame(server, f); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			var out, errout bytes.Buffer
			err := RelayCommand(context.Background(), client, strings.NewReader("stdin\x00"), &out, &errout)
			switch {
			case tc.message != "":
				if err == nil || err.Error() != tc.message {
					t.Fatalf("setup failure: %v", err)
				}
			case tc.code != 0:
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != int(tc.code) {
					t.Fatalf("workload exit: %v", err)
				}
			case err != nil:
				t.Fatal(err)
			}
			if out.String() != "out\x00" || errout.String() != "err" {
				t.Fatal(out.String(), errout.String())
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInputCancellation(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }() // Test endpoint cleanup.
	defer func() { _ = w.Close() }() // Test endpoint cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = pumpInput(ctx, io.Discard, r); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stdin cancellation blocked")
	}
}

func TestFileInputPump(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }() // Test pipe cleanup.
	var framed bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = pumpInput(ctx, &framed, r); close(done) }()
	// Exercise poll timeout while stdin remains open and idle.
	time.Sleep(150 * time.Millisecond)
	if _, err := w.WriteString("pipe input"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("input pump did not send EOF")
	}
	data, err := io.ReadAll(&agent.CommandInput{Conn: &framed})
	if err != nil || string(data) != "pipe input" {
		t.Fatal(string(data), err)
	}
	_ = pumpInput(context.Background(), failedOutput{}, strings.NewReader("data"))
}

// Exit must release the previous VM's input pump before the next VM or
// listener cleanup uses that input.
func TestRelayExitWithIdleInput(t *testing.T) {
	t.Run("rerun file", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
		runRelayWithIdleInput(t, reader, writer)
		if _, err := writer.WriteString("next command"); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != "next command" {
			t.Fatalf("next input = %q, %v", got, err)
		}
	})
	t.Run("listener connection", func(t *testing.T) {
		input, peer := net.Pipe()
		t.Cleanup(func() { _ = input.Close(); _ = peer.Close() })
		runRelayWithIdleInput(t, input, peer)
	})
}

func runRelayWithIdleInput(t *testing.T, input io.Reader, inputWriter io.Writer) {
	t.Helper()
	client, guest := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
	done := make(chan error, 1)
	const firstInput = "first command"
	inputSent := make(chan error, 1)
	go func() {
		_, err := io.WriteString(inputWriter, firstInput)
		inputSent <- err
	}()

	go func() {
		if _, err := agent.ReadAttachHello(guest); err != nil {
			done <- err
			return
		}
		input := &agent.CommandInput{Conn: guest}
		data := make([]byte, len(firstInput))
		if _, err := io.ReadFull(input, data); err != nil {
			done <- err
			return
		}
		if string(data) != firstInput {
			done <- errors.New("guest received incorrect input")
			return
		}

		done <- agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{}}})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := RelayCommand(ctx, client, input, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-inputSent; err != nil {
		t.Fatal(err)
	}
}

func TestRelayInputFailure(t *testing.T) {
	client, guest := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
	wantErr := errors.New("read failed")
	hello := make(chan error, 1)
	go func() {
		_, err := agent.ReadAttachHello(guest)
		hello <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := RelayCommand(ctx, client, failingInput{wantErr}, io.Discard, io.Discard)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected input failure, got %v", err)
	}
	if err := <-hello; err != nil {
		t.Fatal(err)
	}
}

type failingInput struct{ err error }

func (r failingInput) Read([]byte) (int, error) { return 0, r.err }

func TestRelayTerminal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guestError string
	}{
		{name: "success"},
		{name: "guest error", guestError: "terminal command failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })

			dims := unix.Winsize{Col: 96, Row: 37}
			if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &dims); err != nil {
				t.Fatal(err)
			}
			fd := int(slave.Fd())
			original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if original.Lflag&unix.ICANON == 0 {
				t.Fatal("fresh PTY should be in canonical mode")
			}

			client, guest := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- RelayCommand(ctx, client, slave, io.Discard, io.Discard) }()

			hello, err := agent.ReadAttachHello(guest)
			if err != nil {
				t.Fatal(err)
			}
			if !hello.GetTty() || hello.GetCols() != uint32(dims.Col) || hello.GetRows() != uint32(dims.Row) {
				t.Fatalf("TTY hello = %v", hello)
			}
			raw, err := unix.IoctlGetTermios(fd, unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if raw.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 {
				t.Fatalf("terminal flags during relay = %#x", raw.Lflag)
			}

			exit := &agentv1.JobExit{Error: tc.guestError}
			if err := agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: exit}}); err != nil {
				t.Fatal(err)
			}
			err = <-done
			switch {
			case tc.guestError == "" && err != nil:
				t.Fatal(err)
			case tc.guestError != "" && (err == nil || err.Error() != tc.guestError):
				t.Fatalf("guest error = %v, want %q", err, tc.guestError)
			}

			restored, err := unix.IoctlGetTermios(fd, unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			if *restored != *original {
				t.Errorf("terminal restored to %+v, want %+v", restored, original)
			}
		})
	}
}

func TestExitError(t *testing.T) {
	if got := (&ExitError{7}).Error(); got != "workload exited with status 7" {
		t.Fatalf("exit error: %s", got)
	}
}

func TestRelayTransportFailure(t *testing.T) {
	const cancellation = "cancellation"
	for _, phase := range []string{"handshake", "output", cancellation} {
		t.Run(phase, func(t *testing.T) {
			client, guest := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = guest.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() {
				if phase == "handshake" {
					done <- guest.Close()
					return
				}
				if _, err := agent.ReadAttachHello(guest); err != nil {
					done <- err
					return
				}
				if phase == cancellation {
					cancel()
					done <- nil
					return
				}
				done <- guest.Close() // Drop the connection before any exit frame.
			}()

			err := RelayCommand(ctx, client, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil {
				t.Fatal("broken transport reported successful command completion")
			}
			if phase == cancellation && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
