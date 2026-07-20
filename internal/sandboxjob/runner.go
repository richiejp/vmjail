// Package sandboxjob adapts the guest's non-PTY job protocol to nodejob.Runner.
package sandboxjob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/richiejp/darkaifactory/internal/agent"
	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

// Dialer resolves a live Sandbox UID to its VM's guest job listener.
type Dialer func(ctx context.Context, sandboxUID string, port uint32) (net.Conn, error)

type Runner struct {
	dial Dialer
}

func NewRunner(dial Dialer) (*Runner, error) {
	if dial == nil {
		return nil, errors.New("sandboxjob: nil dialer")
	}
	return &Runner{dial: dial}, nil
}

func (r *Runner) Start(ctx context.Context, req nodejob.StartRequest, stdout, stderr io.Writer) (nodejob.Process, error) {
	conn, err := r.dial(ctx, req.SandboxUID, agent.DefaultJobVsockPort)
	if err != nil {
		return nil, fmt.Errorf("sandboxjob: dial guest: %w", err)
	}
	if err := agent.WriteJobHello(conn, &agentv1.JobHello{Command: req.Command}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sandboxjob: send hello: %w", err)
	}
	p := &process{conn: conn, stdout: stdout, stderr: stderr, done: make(chan struct{})}
	go p.receive()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Cancel()
		case <-p.done:
		}
	}()
	return p, nil
}

type process struct {
	conn   net.Conn
	stdout io.Writer
	stderr io.Writer
	done   chan struct{}

	canceled  atomic.Bool
	closeOnce sync.Once
	mu        sync.Mutex
	result    nodejob.Result
	err       error
}

func (p *process) receive() {
	defer close(p.done)
	defer func() { _ = p.conn.Close() }()
	//daft:lint:disable=forloopbound reason="bounded by terminal JobExit, connection close, or malformed frame"
	for {
		frame, err := agent.ReadJobFrame(p.conn)
		if err != nil {
			if p.canceled.Load() && (errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF)) {
				p.setResult(nodejob.Result{}, nil)
				return
			}
			p.setResult(nodejob.Result{Reason: "guest job stream ended before exit"}, err)
			return
		}
		switch body := frame.GetBody().(type) {
		case *agentv1.JobFrame_Stdout:
			if _, err := p.stdout.Write(body.Stdout); err != nil {
				p.setResult(nodejob.Result{Reason: "write stdout spool: " + err.Error()}, err)
				return
			}
		case *agentv1.JobFrame_Stderr:
			if _, err := p.stderr.Write(body.Stderr); err != nil {
				p.setResult(nodejob.Result{Reason: "write stderr spool: " + err.Error()}, err)
				return
			}
		case *agentv1.JobFrame_Exit:
			if body.Exit == nil {
				p.setResult(nodejob.Result{Reason: "guest sent nil job exit"}, errors.New("sandboxjob: nil exit"))
				return
			}
			p.setResult(nodejob.Result{ExitCode: body.Exit.GetCode(), Reason: body.Exit.GetError()}, nil)
			return
		default:
			p.setResult(nodejob.Result{Reason: "guest sent empty job frame"}, errors.New("sandboxjob: empty frame"))
			return
		}
	}
}

func (p *process) setResult(result nodejob.Result, err error) {
	p.mu.Lock()
	p.result = result
	p.err = err
	p.mu.Unlock()
}

func (p *process) Wait() (nodejob.Result, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result, p.err
}

func (p *process) Cancel() error {
	p.canceled.Store(true)
	var err error
	p.closeOnce.Do(func() { err = p.conn.Close() })
	return err
}

var (
	_ nodejob.Runner  = (*Runner)(nil)
	_ nodejob.Process = (*process)(nil)
)
