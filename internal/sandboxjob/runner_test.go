package sandboxjob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/agent"
	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/nodejob"
	"github.com/richiejp/vmjail/internal/sandboxgateway"
)

func TestRunnerRoundTripSeparatedOutput(t *testing.T) {
	host, guest := net.Pipe()
	runner, err := NewRunner(func(_ context.Context, sandboxUID string, port uint32) (net.Conn, error) {
		if sandboxUID != "sb" || port != agent.DefaultJobVsockPort {
			t.Fatalf("dial sandbox=%q port=%d", sandboxUID, port)
		}
		return host, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	guestDone := make(chan struct{})
	go func() {
		defer close(guestDone)
		defer func() { _ = guest.Close() }()
		hello, readErr := agent.ReadJobHello(guest)
		if readErr != nil || len(hello.GetCommand()) != 2 {
			return
		}
		_ = agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Stdout{Stdout: []byte("out")}})
		_ = agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Stderr{Stderr: []byte("err")}})
		_ = agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{Code: 4}}})
	}()
	var stdout, stderr bytes.Buffer
	p, err := runner.Start(t.Context(), nodejob.StartRequest{SandboxUID: "sb", Command: []string{"agent", "run"}}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Wait()
	if err != nil || result.ExitCode != 4 || result.Reason != "" || stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("result=%+v err=%v stdout=%q stderr=%q", result, err, stdout.String(), stderr.String())
	}
	<-guestDone
}

func TestRunnerCancelClosesGuestConnection(t *testing.T) {
	host, guest := net.Pipe()
	runner, _ := NewRunner(func(context.Context, string, uint32) (net.Conn, error) { return host, nil })
	guestClosed := make(chan error, 1)
	go func() {
		_, err := agent.ReadJobHello(guest)
		if err == nil {
			var b [1]byte
			_, err = guest.Read(b[:])
		}
		guestClosed <- err
	}()
	p, err := runner.Start(t.Context(), nodejob.StartRequest{SandboxUID: "sb", Command: []string{"sleep"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Cancel(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-guestClosed:
	case <-time.After(time.Second):
		t.Fatal("guest did not observe cancellation close")
	}
	result, err := p.Wait()
	if err != nil || result != (nodejob.Result{}) {
		t.Fatalf("Wait after cancel = %+v, %v", result, err)
	}
	_ = guest.Close()
}

func TestRunnerStreamFailuresAndInfrastructureExit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serve     func(net.Conn)
		wantError bool
		wantWhy   string
	}{
		{name: "eof before exit", serve: func(c net.Conn) { _, _ = agent.ReadJobHello(c); _ = c.Close() }, wantError: true, wantWhy: "before exit"},
		{name: "empty frame", serve: func(c net.Conn) {
			_, _ = agent.ReadJobHello(c)
			_ = agent.WriteJobFrame(c, &agentv1.JobFrame{})
			_ = c.Close()
		}, wantError: true, wantWhy: "empty job frame"},
		{name: "guest infrastructure exit", serve: func(c net.Conn) {
			_, _ = agent.ReadJobHello(c)
			_ = agent.WriteJobFrame(c, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{Code: 1, Error: "crun missing"}}})
			_ = c.Close()
		}, wantWhy: "crun missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			runner, _ := NewRunner(func(context.Context, string, uint32) (net.Conn, error) { return host, nil })
			go tc.serve(guest)
			p, err := runner.Start(t.Context(), nodejob.StartRequest{SandboxUID: "sb", Command: []string{"x"}}, io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			result, waitErr := p.Wait()
			if (waitErr != nil) != tc.wantError || !bytes.Contains([]byte(result.Reason), []byte(tc.wantWhy)) {
				t.Fatalf("result=%+v err=%v", result, waitErr)
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunnerSpoolWriteFailure(t *testing.T) {
	host, guest := net.Pipe()
	runner, _ := NewRunner(func(context.Context, string, uint32) (net.Conn, error) { return host, nil })
	want := errors.New("spool failed")
	go func() {
		_, _ = agent.ReadJobHello(guest)
		_ = agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Stdout{Stdout: []byte("x")}})
		_ = guest.Close()
	}()
	p, err := runner.Start(t.Context(), nodejob.StartRequest{SandboxUID: "sb", Command: []string{"x"}}, failingWriter{want}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Wait()
	if !errors.Is(err, want) || !bytes.Contains([]byte(result.Reason), []byte("stdout spool")) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunnerConstructionAndDialFailure(t *testing.T) {
	if _, err := NewRunner(nil); err == nil {
		t.Fatal("nil dialer succeeded")
	}
	want := errors.New("dial failed")
	runner, _ := NewRunner(func(context.Context, string, uint32) (net.Conn, error) { return nil, want })
	if _, err := runner.Start(t.Context(), nodejob.StartRequest{}, io.Discard, io.Discard); !errors.Is(err, want) {
		t.Fatalf("Start error = %v", err)
	}
}

func TestRunnerWaitsForSandboxRoute(t *testing.T) {
	host, guest := net.Pipe()
	dials := 0
	runner, _ := NewRunner(func(context.Context, string, uint32) (net.Conn, error) {
		dials++
		if dials == 1 {
			return nil, sandboxgateway.ErrJobNoRoute
		}
		if dials == 2 {
			return nil, fmt.Errorf("socket pending: %w", os.ErrNotExist)
		}
		return host, nil
	})
	runner.retryWait = time.Millisecond
	go func() {
		defer func() { _ = guest.Close() }()
		_, _ = agent.ReadJobHello(guest)
		_ = agent.WriteJobFrame(guest, &agentv1.JobFrame{Body: &agentv1.JobFrame_Exit{Exit: &agentv1.JobExit{}}})
	}()
	process, err := runner.Start(t.Context(), nodejob.StartRequest{SandboxUID: "starting", Command: []string{"true"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err != nil || dials != 3 {
		t.Fatalf("Wait = %v, dials = %d", err, dials)
	}
}

func TestRunnerBoundsSandboxRouteWait(t *testing.T) {
	runner, _ := NewRunner(func(context.Context, string, uint32) (net.Conn, error) {
		return nil, sandboxgateway.ErrJobNoRoute
	})
	runner.routeWait = time.Millisecond
	runner.retryWait = time.Hour
	if _, err := runner.Start(t.Context(), nodejob.StartRequest{}, io.Discard, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start error = %v, want deadline exceeded", err)
	}
}

func TestRoutePendingClassification(t *testing.T) {
	for _, err := range []error{sandboxgateway.ErrJobNoRoute, os.ErrNotExist, syscall.ECONNREFUSED, io.EOF} {
		if !routePending(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("routePending(%v) = false", err)
		}
	}
	if routePending(errors.New("permission denied")) {
		t.Error("arbitrary dial error was classified as pending")
	}
}
