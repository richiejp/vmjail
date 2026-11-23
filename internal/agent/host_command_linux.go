//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
)

// DefaultCommandVsockPort carries an attach hello, framed stdin, EOF and output.
const DefaultCommandVsockPort uint32 = 9600

func runHostCommand(ctx context.Context, bundle string, dial func(context.Context, uint32) (net.Conn, error)) error {
	conn, err := dial(ctx, DefaultCommandVsockPort)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }() // Release the command stream on exit.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	hello, err := ReadAttachHello(conn)
	if err != nil {
		return err
	}

	streams := jobOutput{conn: conn}
	cmd := containerRunCommand(ctx, bundle)
	code, runErr := runCommandStreams(cmd, hello, &CommandInput{Conn: conn}, streams.stdout(), streams.stderr())

	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	return writeJobExit(conn, &agentv1.JobExit{Code: code, Error: message})
}

func runCommandStreams(cmd *exec.Cmd, hello *agentv1.AgentAttachHello, input io.Reader, stdout, stderr io.Writer) (int32, error) {
	if hello.GetTty() {
		proc, err := startPTY(cmd, hello.GetCols(), hello.GetRows())
		if err != nil {
			return 125, err
		}
		defer func() { _ = proc.Close() }()         // Reclaim the PTY after command completion.
		go func() { _, _ = io.Copy(proc, input) }() // Connection closure ends this pump at teardown.

		_, copyErr := io.Copy(stdout, proc)
		if errors.Is(copyErr, syscall.EIO) {
			copyErr = nil // Linux PTYs return EIO when the last slave closes.
		}
		if copyErr != nil {
			copyErr = errors.Join(copyErr, proc.Close())
		}
		code, err := proc.Wait()
		return code, errors.Join(copyErr, err)
	}

	in, inputWriter, err := os.Pipe()
	if err != nil {
		return 125, err
	}
	defer func() { _ = in.Close(); _ = inputWriter.Close() }()

	// An actual pipe FD keeps os/exec.Wait independent of the network reader.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, stdout, stderr
	go func() {
		// Close stdin on input EOF. The host reports transport errors.
		_, _ = io.Copy(inputWriter, input)
		_ = inputWriter.Close()
	}()

	return commandExitCode(cmd.Run(), 125)
}

// CommandInput decodes stdin frames. An exit frame means stdin EOF, without
// half-closing vsock (which would also discard command output).
type CommandInput struct {
	Conn    io.Reader
	pending bytes.Buffer
	ended   bool
}

func (r *CommandInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.pending.Len() == 0 {
		if r.ended {
			return 0, io.EOF
		}
		f, err := ReadJobFrame(r.Conn)
		if err != nil {
			return 0, err
		}
		switch b := f.GetBody().(type) {
		case *agentv1.JobFrame_Stdout:
			r.pending.Write(b.Stdout)
		case *agentv1.JobFrame_Exit:
			r.ended = true
		default:
			return 0, fmt.Errorf("unexpected command input frame")
		}
	}
	return r.pending.Read(p)
}

// WriteCommandInput sends stdin; EOF is an explicit frame, keeping output open.
func WriteCommandInput(w io.Writer, data []byte) error {
	return WriteJobFrame(w, &agentv1.JobFrame{Body: &agentv1.JobFrame_Stdout{Stdout: data}})
}

// EndCommandInput sends stdin EOF without closing the output stream.
func EndCommandInput(w io.Writer) error {
	return writeJobExit(w, &agentv1.JobExit{})
}
