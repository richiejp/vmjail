package sandboxexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

const testImageRef = "alpine:3"

func TestNewRejectsNilStdout(t *testing.T) {
	_, err := New(sandbox.Config{}, nil, func(context.Context, io.Writer, sandbox.Config) error { return nil })
	if err == nil {
		t.Fatal("expected error for nil stdout")
	}
}

func TestNewDefaultsRunnerToHello(t *testing.T) {
	// Production callers pass runner = nil; the constructor must
	// substitute sandbox.Hello so HandleDispatch doesn't NPE.
	e, err := New(sandbox.Config{}, io.Discard, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.run == nil {
		t.Fatal("runner not defaulted")
	}
}

func TestHandleDispatchInvokesRunnerWithBaseConfig(t *testing.T) {
	base := sandbox.Config{
		CloudHypervisor: "/path/to/ch",
		Kernel:          "/path/to/kernel",
		Initramfs:       "/path/to/initramfs",
		Memory:          "512M",
		CPUs:            2,
	}
	var seen sandbox.Config
	called := 0
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		called++
		return nil
	}
	e, err := New(base, io.Discard, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &sandboxctrlv1.Dispatch{
		DispatchId: "d-42",
		Spec: &sandboxctrlv1.SandboxSpec{
			ImageRef:      testImageRef,
			WorkspaceUuid: "ws-1",
		},
	}
	if err := e.HandleDispatch(t.Context(), d); err != nil {
		t.Fatalf("HandleDispatch: %v", err)
	}
	if called != 1 {
		t.Errorf("runner called %d times, want 1", called)
	}
	if seen.CloudHypervisor != base.CloudHypervisor ||
		seen.Kernel != base.Kernel ||
		seen.Initramfs != base.Initramfs ||
		seen.Memory != base.Memory ||
		seen.CPUs != base.CPUs {
		t.Errorf("runner saw cfg %+v, want baseline %+v", seen, base)
	}
}

func TestHandleDispatchLogsDispatchToStdout(t *testing.T) {
	var buf bytes.Buffer
	e, err := New(sandbox.Config{}, &buf, func(context.Context, io.Writer, sandbox.Config) error { return nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &sandboxctrlv1.Dispatch{
		DispatchId: "d-99",
		Spec: &sandboxctrlv1.SandboxSpec{
			ImageRef:      testImageRef,
			WorkspaceUuid: "ws-9",
		},
	}
	if err := e.HandleDispatch(t.Context(), d); err != nil {
		t.Fatalf("HandleDispatch: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"d-99", "ws-9", testImageRef} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestHandleDispatchSurfacesRunnerError(t *testing.T) {
	want := errors.New("synthetic boot failure")
	e, err := New(sandbox.Config{}, io.Discard, func(context.Context, io.Writer, sandbox.Config) error {
		return want
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &sandboxctrlv1.Dispatch{DispatchId: "d-1", Spec: &sandboxctrlv1.SandboxSpec{}}
	got := e.HandleDispatch(t.Context(), d)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if !strings.Contains(got.Error(), "d-1") {
		t.Errorf("err message %q should mention dispatch id", got.Error())
	}
}
