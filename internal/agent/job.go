package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/oops"
	"github.com/richiejp/vmjail/internal/trace"
)

// DefaultJobVsockPort is dedicated to non-PTY detached command sessions.
// It is separate from attach so protocol confusion cannot accidentally turn
// an interactive stream into a long-lived background job.
const DefaultJobVsockPort uint32 = 9100

const (
	jobReportPrefix = "job"
	jobCopyBufSize  = 32 * 1024
)

type JobExecRequest struct {
	Command []string
	Env     []string
	User    string
	Cwd     string
}

type JobProcess interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (int32, error)
	Cancel() error
}

type ExecJob interface {
	Start(ctx context.Context, req JobExecRequest) (JobProcess, error)
}

type jobDeps struct {
	listenVsock func(port uint32) (net.Listener, error)
	exec        ExecJob
}

var ErrJobExecUnavailable = errors.New("agent: job exec unsupported off Linux")

type unavailableJobExec struct{}

func (unavailableJobExec) Start(context.Context, JobExecRequest) (JobProcess, error) {
	return nil, ErrJobExecUnavailable
}

func startJobListener(ctx context.Context, w io.Writer, d runDeps) {
	if !d.cfg.GetAttachEnabled() || d.job.listenVsock == nil || d.job.exec == nil {
		return
	}
	proc := readContainerProcess(w, d.cfg.GetBundle())
	go func() {
		if !awaitContainerRunning(ctx, w, jobReportPrefix,
			"container did not reach running state; detached jobs unavailable") {
			return
		}
		runJobListener(ctx, w, DefaultJobVsockPort, d.job, proc)
	}()
}

func runJobListener(ctx context.Context, w io.Writer, port uint32, d jobDeps, proc containerProcess) {
	_, span := trace.Start(ctx, "agent.job.listener.start",
		trace.WithAttributes(trace.KV{Key: traceAttrVsockPort, Value: strconv.FormatUint(uint64(port), 10)}))
	defer span.End()
	ln, err := d.listenVsock(port)
	if err != nil {
		writeReport(w, jobReportPrefix, fmt.Sprintf("listen vsock %d: %v", port, err))
		return
	}
	writeReport(w, jobReportPrefix, fmt.Sprintf("ready vsock %d", port))
	go func() {
		<-ctx.Done()
		oops.Skip("agent: close job listener", ln.Close(), net.ErrClosed)
	}()
	//vmjail:lint:disable=forloopbound reason="bounded by listener Close on ctx cancellation or an Accept error"
	for {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			if errors.Is(acceptErr, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			writeReport(w, jobReportPrefix, "accept: "+acceptErr.Error())
			return
		}
		go handleJob(ctx, w, conn, d.exec, proc)
	}
}

func handleJob(ctx context.Context, w io.Writer, conn net.Conn, executor ExecJob, container containerProcess) {
	defer func() { oops.Skip("agent: close job conn", conn.Close(), net.ErrClosed) }()
	hello, err := ReadJobHello(conn)
	if err != nil {
		writeReport(w, jobReportPrefix, "read hello: "+err.Error())
		_ = writeJobExit(conn, &agentv1.JobExit{Code: 1, Error: "agent: read job hello: " + err.Error()})
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	process, err := executor.Start(jobCtx, JobExecRequest{
		Command: hello.GetCommand(), Env: container.Env, User: container.User, Cwd: container.Cwd,
	})
	if err != nil {
		writeReport(w, jobReportPrefix, "exec start: "+err.Error())
		_ = writeJobExit(conn, &agentv1.JobExit{Code: 1, Error: "agent: job exec start: " + err.Error()})
		return
	}
	defer func() { oops.Skip("agent: cancel job process", process.Cancel(), net.ErrClosed, os.ErrProcessDone) }()

	// The protocol is output-only after Hello. A host-side close is therefore
	// an unambiguous cancellation signal; no command input can be consumed.
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancel()
		_ = process.Cancel()
	}()

	streams := jobOutput{conn: conn}
	var pumps sync.WaitGroup
	pumps.Add(2)
	go pumpJobOutput(process.Stdout(), streams.stdout(), process, &pumps)
	go pumpJobOutput(process.Stderr(), streams.stderr(), process, &pumps)
	// StdoutPipe and StderrPipe require their readers to reach EOF before Wait:
	// exec.Cmd.Wait closes both pipes and can otherwise discard a short-lived
	// command's final output. Process exit closes the writers, so both pumps can
	// finish before the process is reaped.
	pumps.Wait()
	code, waitErr := process.Wait()
	exit := &agentv1.JobExit{Code: code}
	if waitErr != nil {
		exit.Code = 1
		exit.Error = "agent: job exec wait: " + waitErr.Error()
		writeReport(w, jobReportPrefix, "exec wait: "+waitErr.Error())
	}
	_ = writeJobExit(conn, exit)
}

func pumpJobOutput(src io.Reader, dst io.Writer, process JobProcess, done *sync.WaitGroup) {
	defer done.Done()
	if _, err := io.CopyBuffer(dst, src, make([]byte, jobCopyBufSize)); err != nil {
		oops.Skip("agent: cancel job process after output failure", process.Cancel(), net.ErrClosed, os.ErrProcessDone)
	}
}

// jobOutput serializes frames from concurrent stdout and stderr pumps.
type jobOutput struct {
	conn io.Writer
	mu   sync.Mutex
}

func (o *jobOutput) stdout() jobOutputWriter { return jobOutputWriter{output: o} }
func (o *jobOutput) stderr() jobOutputWriter { return jobOutputWriter{output: o, stderr: true} }

type jobOutputWriter struct {
	output *jobOutput
	stderr bool
}

func (w jobOutputWriter) Write(data []byte) (int, error) {
	w.output.mu.Lock()
	defer w.output.mu.Unlock()

	written := 0
	for len(data) > 0 {
		chunk := data[:min(len(data), jobCopyBufSize)]
		frame := &agentv1.JobFrame{Body: &agentv1.JobFrame_Stdout{Stdout: chunk}}
		if w.stderr {
			frame.Body = &agentv1.JobFrame_Stderr{Stderr: chunk}
		}

		if err := WriteJobFrame(w.output.conn, frame); err != nil {
			return written, err
		}
		written += len(chunk)
		data = data[len(chunk):]
	}
	return written, nil
}

func writeJobExit(w io.Writer, exit *agentv1.JobExit) error {
	return WriteJobFrame(w, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: exit}})
}

// ReadJobOutput drains both output streams through the exit frame. Guest
// command failures are reported by the exit frame; err describes the stream.
func ReadJobOutput(r io.Reader, stdout, stderr io.Writer) (*agentv1.JobExit, error) {
	//vmjail:lint:disable=forloopbound reason="bounded by a terminal exit frame or a stream error"
	for {
		frame, err := ReadJobFrame(r)
		if err != nil {
			return nil, fmt.Errorf("guest job stream ended before exit: %w", err)
		}
		switch body := frame.GetBody().(type) {
		case *agentv1.JobFrame_Stdout:
			if err := writeStreamBytes(stdout, body.Stdout); err != nil {
				return nil, fmt.Errorf("write stdout spool: %w", err)
			}
		case *agentv1.JobFrame_Stderr:
			if err := writeStreamBytes(stderr, body.Stderr); err != nil {
				return nil, fmt.Errorf("write stderr spool: %w", err)
			}
		case *agentv1.JobFrame_Exit:
			if body.Exit == nil {
				return nil, errors.New("guest sent nil job exit")
			}
			return body.Exit, nil
		default:
			return nil, errors.New("guest sent empty job frame")
		}
	}
}

func writeStreamBytes(dst io.Writer, data []byte) error {
	n, err := dst.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func WriteJobHello(w io.Writer, hello *agentv1.JobHello) error {
	payload, err := proto.Marshal(hello)
	if err != nil {
		return fmt.Errorf("agent: marshal JobHello: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

func ReadJobHello(r io.Reader) (*agentv1.JobHello, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var hello agentv1.JobHello
	if err := proto.Unmarshal(payload, &hello); err != nil {
		return nil, fmt.Errorf("agent: unmarshal JobHello: %w", err)
	}
	return &hello, nil
}

func WriteJobFrame(w io.Writer, frame *agentv1.JobFrame) error {
	payload, err := proto.Marshal(frame)
	if err != nil {
		return fmt.Errorf("agent: marshal JobFrame: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

func ReadJobFrame(r io.Reader) (*agentv1.JobFrame, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var frame agentv1.JobFrame
	if err := proto.Unmarshal(payload, &frame); err != nil {
		return nil, fmt.Errorf("agent: unmarshal JobFrame: %w", err)
	}
	return &frame, nil
}
