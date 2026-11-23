package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/richiejp/vmjail/internal/version"
)

type failedVersionOutput struct{}

func (failedVersionOutput) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestVersionCommand(t *testing.T) {
	var output bytes.Buffer
	cmd := NewVersionCommand("vmjail")
	cmd.SetArgs([]string{})
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil || output.String() != version.Version+"\n" {
		t.Fatalf("version output = %q, error = %v", output.String(), err)
	}

	cmd.SetArgs([]string{"extra"})
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("version accepted an extra argument")
	}

	cmd.SetArgs([]string{})
	cmd.SetOut(failedVersionOutput{})
	if err := cmd.Execute(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("version write error = %v", err)
	}

	saved := version.Version
	t.Cleanup(func() { version.Version = saved })
	version.Version = ""
	if err := cmd.Execute(); err == nil || errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("missing build version error = %v", err)
	}
}
