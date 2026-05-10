package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }

func TestRunSuccessPath(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"version"}, &out, &errBuf); code != 0 {
		t.Errorf("code = %d (stderr=%q), want 0", code, errBuf.String())
	}
}

func TestRunErrorPathReturns1AndPrefixesStderr(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"definitely-not-real"}, &out, &errBuf)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "daft-orchestrator:") {
		t.Errorf("expected 'daft-orchestrator:' in stderr, got %q", errBuf.String())
	}
}

func TestRunReturns2WhenStderrWriteFails(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"definitely-not-real"}, &out, failingWriter{}); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestMainPassesArgvAndCallsOsExit(t *testing.T) {
	oldArgs, oldExit := os.Args, osExit
	defer func() { os.Args, osExit = oldArgs, oldExit }()
	os.Args = []string{"daft-orchestrator", "version"}
	code := -1
	osExit = func(c int) { code = c }

	main()

	if code != 0 {
		t.Errorf("osExit called with %d, want 0", code)
	}
}
