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

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
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
	go runJobListener(ctx, w, DefaultJobVsockPort, d.job, proc)
}

func runJobListener(ctx context.Context, w io.Writer, port uint32, d jobDeps, proc containerProcess) {
	_, span := trace.Start(ctx, "agent.job.listener.start",
		trace.WithAttributes(trace.KV{Key: "vsock_port", Value: strconv.FormatUint(uint64(port), 10)}))
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
	//daft:lint:disable=forloopbound reason="bounded by listener Close on ctx cancellation or an Accept error"
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
		_ = WriteJobFrame(conn, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{
			Exit: &agentv1.JobExit{Code: 1, Error: "agent: read job hello: " + err.Error()},
		}})
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	process, err := executor.Start(jobCtx, JobExecRequest{
		Command: hello.GetCommand(), Env: container.Env, User: container.User,
	})
	if err != nil {
		writeReport(w, jobReportPrefix, "exec start: "+err.Error())
		_ = WriteJobFrame(conn, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{
			Exit: &agentv1.JobExit{Code: 1, Error: "agent: job exec start: " + err.Error()},
		}})
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

	var writeMu sync.Mutex
	var pumps sync.WaitGroup
	pumps.Add(2)
	go pumpJobOutput(process.Stdout(), true, conn, &writeMu, process, &pumps)
	go pumpJobOutput(process.Stderr(), false, conn, &writeMu, process, &pumps)
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
	writeMu.Lock()
	_ = WriteJobFrame(conn, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: exit}})
	writeMu.Unlock()
}

func pumpJobOutput(src io.Reader, stdout bool, conn net.Conn, writeMu *sync.Mutex, process JobProcess, done *sync.WaitGroup) {
	defer done.Done()
	buf := make([]byte, jobCopyBufSize)
	//daft:lint:disable=forloopbound reason="bounded by process pipe EOF or cancellation closing the pipe"
	for {
		n, err := src.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			frame := &agentv1.JobFrame{Body: &agentv1.JobFrame_Stderr{Stderr: data}}
			if stdout {
				frame.Body = &agentv1.JobFrame_Stdout{Stdout: data}
			}
			writeMu.Lock()
			writeErr := WriteJobFrame(conn, frame)
			writeMu.Unlock()
			if writeErr != nil {
				_ = process.Cancel()
				return
			}
		}
		if err != nil {
			return
		}
	}
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
