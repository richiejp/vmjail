package main

import (
	"context"
	"io"
	"os"
	"syscall"
	"testing"
)

func TestMainWiresAgentMainAndOsExit(t *testing.T) {
	oldExit, oldAgent := osExit, agentMain
	defer func() { osExit, agentMain = oldExit, oldAgent }()

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
