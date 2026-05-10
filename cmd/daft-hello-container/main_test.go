package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/agent"
)

func TestRunPrintsBannerAndReturnsZero(t *testing.T) {
	var buf bytes.Buffer
	if code := run(&buf); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), agent.ContainerBanner) {
		t.Errorf("expected banner %q in output, got %q", agent.ContainerBanner, buf.String())
	}
}

// failingWriter forces fmt.Fprintln's error branch.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }

func TestRunReturnsOneOnWriteError(t *testing.T) {
	if code := run(failingWriter{}); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}

func TestMainCallsRunAndOsExit(t *testing.T) {
	oldExit, oldOut := osExit, stdout
	defer func() { osExit, stdout = oldExit, oldOut }()
	var buf bytes.Buffer
	stdout = &buf
	code := -1
	osExit = func(c int) { code = c }

	main()

	if code != 0 {
		t.Errorf("osExit called with %d, want 0", code)
	}
	if !strings.Contains(buf.String(), agent.ContainerBanner) {
		t.Errorf("expected banner in stdout, got %q", buf.String())
	}
}
