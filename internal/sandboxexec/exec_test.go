package sandboxexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

const (
	testImageRef   = "alpine:3"
	testDispatchID = "d-1"
)

func noopRunner() Runner {
	return func(context.Context, io.Writer, sandbox.Config) error { return nil }
}

func TestNewRejectsNilStdout(t *testing.T) {
	_, err := New(Config{Runner: noopRunner()})
	if err == nil {
		t.Fatal("expected error for nil stdout")
	}
}

func TestNewDefaultsRunnerToHello(t *testing.T) {
	// Production callers pass Runner = nil; the constructor must
	// substitute sandbox.Hello so HandleDispatch doesn't NPE.
	e, err := New(Config{Stdout: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.run == nil {
		t.Fatal("runner not defaulted")
	}
}

// TestHandleDispatchBareHelloPath covers the empty-image_ref branch:
// the runner sees the executor's baseline config, no assembler is
// consulted.
func TestHandleDispatchBareHelloPath(t *testing.T) {
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
	assemblerCalled := false
	asm := func(context.Context, string) (sandbox.Config, func(), error) {
		assemblerCalled = true
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{Base: base, Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &nodectrlv1.Dispatch{
		DispatchId: "d-42",
		Spec:       &nodectrlv1.SandboxSpec{WorkspaceUuid: "ws-1"},
	}
	if err := e.HandleDispatch(t.Context(), d); err != nil {
		t.Fatalf("HandleDispatch: %v", err)
	}
	if called != 1 {
		t.Errorf("runner called %d times, want 1", called)
	}
	if assemblerCalled {
		t.Error("assembler called for empty image_ref")
	}
	if seen.CloudHypervisor != base.CloudHypervisor ||
		seen.Kernel != base.Kernel ||
		seen.Initramfs != base.Initramfs {
		t.Errorf("runner saw cfg %+v, want baseline %+v", seen, base)
	}
}

// TestHandleDispatchAssemblerPath covers the image_ref-set branch:
// the assembler is consulted, the runner sees the bundled config,
// and the unlock returned by the assembler runs after the boot.
func TestHandleDispatchAssemblerPath(t *testing.T) {
	bundled := sandbox.Config{
		CloudHypervisor: "/from/assembler/ch",
		Kernel:          "/from/assembler/kernel",
		Initramfs:       "/from/assembler/initramfs",
		Bundle:          "/bundles/abc",
	}
	var seenRef string
	unlockCalls := 0
	asm := func(_ context.Context, ref string) (sandbox.Config, func(), error) {
		seenRef = ref
		return bundled, func() { unlockCalls++ }, nil
	}
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &nodectrlv1.Dispatch{
		DispatchId: "d-7",
		Spec:       &nodectrlv1.SandboxSpec{ImageRef: testImageRef},
	}
	if err := e.HandleDispatch(t.Context(), d); err != nil {
		t.Fatalf("HandleDispatch: %v", err)
	}
	if seenRef != testImageRef {
		t.Errorf("assembler saw ref %q, want %q", seenRef, testImageRef)
	}
	if seen.Bundle != bundled.Bundle {
		t.Errorf("runner saw bundle %q, want %q", seen.Bundle, bundled.Bundle)
	}
	if unlockCalls != 1 {
		t.Errorf("unlock called %d times, want 1", unlockCalls)
	}
}

// TestHandleDispatchAssemblerError pins the error path when the
// assembler fails — runner must not be invoked and the dispatch
// id must appear in the wrapped error.
func TestHandleDispatchAssemblerError(t *testing.T) {
	want := errors.New("synthetic pull failure")
	asm := func(context.Context, string) (sandbox.Config, func(), error) {
		return sandbox.Config{}, func() {}, want
	}
	runCalls := 0
	runner := func(context.Context, io.Writer, sandbox.Config) error {
		runCalls++
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &nodectrlv1.Dispatch{DispatchId: "d-bad", Spec: &nodectrlv1.SandboxSpec{ImageRef: testImageRef}}
	got := e.HandleDispatch(t.Context(), d)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if runCalls != 0 {
		t.Error("runner was invoked despite assembler failure")
	}
	if !strings.Contains(got.Error(), "d-bad") {
		t.Errorf("err message %q should mention dispatch id", got.Error())
	}
}

// TestHandleDispatchNoAssemblerFallback covers the production
// safety net: image_ref is set but no Assembler is configured.
// Expected behaviour is to fall back to the baseline config so
// the dispatch still produces something observable; A4b/A5a
// production wiring always sets an Assembler.
func TestHandleDispatchNoAssemblerFallback(t *testing.T) {
	base := sandbox.Config{Kernel: "/baseline/kernel"}
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Base: base, Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &nodectrlv1.Dispatch{
		DispatchId: testDispatchID,
		Spec:       &nodectrlv1.SandboxSpec{ImageRef: testImageRef},
	}
	if err := e.HandleDispatch(t.Context(), d); err != nil {
		t.Fatalf("HandleDispatch: %v", err)
	}
	if seen.Kernel != base.Kernel {
		t.Errorf("runner saw cfg %+v, want baseline (assembler absent)", seen)
	}
}

func TestHandleDispatchLogsDispatchToStdout(t *testing.T) {
	var buf bytes.Buffer
	e, err := New(Config{Stdout: &buf, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &nodectrlv1.Dispatch{
		DispatchId: "d-99",
		Spec: &nodectrlv1.SandboxSpec{
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

func TestHandleDispatchPanicsOnNilDispatch(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("HandleDispatch did not panic on nil Dispatch")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "nil Dispatch") {
			t.Errorf("panic = %q, want substring 'nil Dispatch'", msg)
		}
	}()
	_ = e.HandleDispatch(t.Context(), nil)
}

func TestHandleDispatchPanicsOnDispatchWithoutSpec(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("HandleDispatch did not panic when Spec is nil")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "without a Spec") {
			t.Errorf("panic = %q, want substring 'without a Spec'", msg)
		}
	}()
	_ = e.HandleDispatch(t.Context(), &nodectrlv1.Dispatch{DispatchId: testDispatchID})
}

func TestHandleDispatchSurfacesRunnerError(t *testing.T) {
	want := errors.New("synthetic boot failure")
	runner := func(context.Context, io.Writer, sandbox.Config) error { return want }
	e, err := New(Config{Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := &nodectrlv1.Dispatch{DispatchId: testDispatchID, Spec: &nodectrlv1.SandboxSpec{}}
	got := e.HandleDispatch(t.Context(), d)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if !strings.Contains(got.Error(), testDispatchID) {
		t.Errorf("err message %q should mention dispatch id", got.Error())
	}
}
