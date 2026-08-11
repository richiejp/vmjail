package main

import (
	"context"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestMainWiresAgentMainAndOsExit(t *testing.T) {
	oldExit, oldAgent, oldArgs := osExit, agentMain, os.Args
	defer func() { osExit, agentMain, os.Args = oldExit, oldAgent, oldArgs }()
	os.Args = []string{"daft-agent"}

	gotCode := -1
	var (
		gotStdout io.Writer
		gotStderr io.Writer
		gotSigs   []os.Signal
		gotCtxOK  bool
	)
	agentMain = func(ctx context.Context, stdout, stderr io.Writer, sigs ...os.Signal) int {
		gotCtxOK = ctx != nil
		gotStdout = stdout
		gotStderr = stderr
		gotSigs = sigs
		return 7
	}
	osExit = func(c int) { gotCode = c }

	main()

	if !gotCtxOK {
		t.Error("agentMain received nil context")
	}
	if gotStdout != os.Stdout {
		t.Errorf("stdout = %v, want os.Stdout", gotStdout)
	}
	if gotStderr != os.Stderr {
		t.Errorf("stderr = %v, want os.Stderr", gotStderr)
	}
	wantSigs := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	if len(gotSigs) != len(wantSigs) {
		t.Fatalf("sigs = %v, want %v", gotSigs, wantSigs)
	}
	for i, s := range wantSigs {
		if gotSigs[i] != s {
			t.Errorf("sigs[%d] = %v, want %v", i, gotSigs[i], s)
		}
	}
	if gotCode != 7 {
		t.Errorf("osExit called with %d, want 7 (agentMain return)", gotCode)
	}
}

func TestMainContainerKeepaliveMode(t *testing.T) {
	oldExit, oldAgent, oldKeepalive, oldArgs := osExit, agentMain, containerKeepalive, os.Args
	defer func() {
		osExit, agentMain, containerKeepalive, os.Args = oldExit, oldAgent, oldKeepalive, oldArgs
	}()
	os.Args = []string{"daft-agent", containerKeepaliveFlag}
	called := false
	containerKeepalive = func() { called = true }
	osExit = func(int) { t.Error("keepalive called osExit") }
	agentMain = func(context.Context, io.Writer, io.Writer, ...os.Signal) int {
		t.Fatal("keepalive called agentMain")
		return 1
	}
	main()
	if !called {
		t.Error("container keepalive was not called")
	}
}

func TestRunContainerKeepaliveRepeats(t *testing.T) {
	wantPanic := struct{}{}
	calls := 0
	defer func() {
		if got := recover(); got != wantPanic {
			t.Errorf("panic = %v, want sentinel", got)
		}
		if calls != 2 {
			t.Errorf("sleep calls = %d, want 2", calls)
		}
	}()
	runContainerKeepalive(func(d time.Duration) {
		if d != time.Hour {
			t.Errorf("sleep duration = %v, want 1h", d)
		}
		calls++
		if calls == 2 {
			panic(wantPanic)
		}
	})
}

func TestDefaultContainerKeepaliveWiresSleep(t *testing.T) {
	oldSleep := containerSleep
	defer func() { containerSleep = oldSleep }()
	wantPanic := struct{}{}
	containerSleep = func(time.Duration) { panic(wantPanic) }
	defer func() {
		if got := recover(); got != wantPanic {
			t.Errorf("panic = %v, want sentinel", got)
		}
	}()
	containerKeepalive()
}
