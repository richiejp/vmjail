package sandboxexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

const (
	testImageRef  = "alpine:3"
	testSandboxID = "sb-1"
)

func noopRunner() Runner {
	return func(context.Context, io.Writer, sandbox.Config) error { return nil }
}

// addedEvent wraps a SandboxSpec in a freshly-minted SandboxWatchEvent
// of type ADDED for tests that exercise the executor's ADDED path.
func addedEvent(uid string, spec *sandboxv1.SandboxSpec) *nodectrlv1.SandboxWatchEvent {
	return &nodectrlv1.SandboxWatchEvent{
		Type: sandboxv1.EventType_EVENT_TYPE_ADDED,
		Sandbox: &sandboxv1.Sandbox{
			Metadata: &sandboxv1.SandboxMetadata{Uid: uid},
			Spec:     spec,
		},
	}
}

func TestNewRejectsNilStdout(t *testing.T) {
	_, err := New(Config{Runner: noopRunner()})
	if err == nil {
		t.Fatal("expected error for nil stdout")
	}
}

func TestNewDefaultsRunnerToHello(t *testing.T) {
	// Production callers pass Runner = nil; the constructor must
	// substitute sandbox.Hello so HandleSandboxEvent doesn't NPE.
	e, err := New(Config{Stdout: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.run == nil {
		t.Fatal("runner not defaulted")
	}
}

// TestHandleSandboxEventBareHelloPath covers the empty-image_ref branch:
// the runner sees the executor's baseline config, no assembler is
// consulted.
func TestHandleSandboxEventBareHelloPath(t *testing.T) {
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
	evt := addedEvent("sb-42", &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-1"})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
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

// TestHandleSandboxEventAssemblerPath covers the image_ref-set branch:
// the assembler is consulted, the runner sees the bundled config,
// and the unlock returned by the assembler runs after the boot.
func TestHandleSandboxEventAssemblerPath(t *testing.T) {
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
	evt := addedEvent("sb-7", &sandboxv1.SandboxSpec{ImageRef: testImageRef})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
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

// TestHandleSandboxEventAssemblerError pins the error path when the
// assembler fails — runner must not be invoked and the dispatch
// id must appear in the wrapped error.
func TestHandleSandboxEventAssemblerError(t *testing.T) {
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
	evt := addedEvent("sb-bad", &sandboxv1.SandboxSpec{ImageRef: testImageRef})
	got := e.HandleSandboxEvent(t.Context(), evt)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if runCalls != 0 {
		t.Error("runner was invoked despite assembler failure")
	}
	if !strings.Contains(got.Error(), "sb-bad") {
		t.Errorf("err message %q should mention sandbox uid", got.Error())
	}
}

// TestHandleSandboxEventNoAssemblerFallback covers the production
// safety net: image_ref is set but no Assembler is configured.
// Expected behaviour is to fall back to the baseline config so
// the dispatch still produces something observable; A4b/A5a
// production wiring always sets an Assembler.
func TestHandleSandboxEventNoAssemblerFallback(t *testing.T) {
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
	evt := addedEvent(testSandboxID, &sandboxv1.SandboxSpec{ImageRef: testImageRef})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Kernel != base.Kernel {
		t.Errorf("runner saw cfg %+v, want baseline (assembler absent)", seen)
	}
}

func TestHandleSandboxEventLogsToStdout(t *testing.T) {
	var buf bytes.Buffer
	e, err := New(Config{Stdout: &buf, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-99", &sandboxv1.SandboxSpec{
		ImageRef:      testImageRef,
		WorkspaceUuid: "ws-9",
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"sb-99", "ws-9", testImageRef} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestHandleSandboxEventPanicsOnNilEvent(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("HandleSandboxEvent did not panic on nil event")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "nil event") {
			t.Errorf("panic = %q, want substring 'nil event'", msg)
		}
	}()
	_ = e.HandleSandboxEvent(t.Context(), nil)
}

func TestHandleSandboxEventPanicsWithoutSpec(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("HandleSandboxEvent did not panic when Spec is nil")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "without a Spec") {
			t.Errorf("panic = %q, want substring 'without a Spec'", msg)
		}
	}()
	_ = e.HandleSandboxEvent(t.Context(), &nodectrlv1.SandboxWatchEvent{
		Type:    sandboxv1.EventType_EVENT_TYPE_ADDED,
		Sandbox: &sandboxv1.Sandbox{Metadata: &sandboxv1.SandboxMetadata{Uid: testSandboxID}},
	})
}

func TestHandleSandboxEventSurfacesRunnerError(t *testing.T) {
	want := errors.New("synthetic boot failure")
	runner := func(context.Context, io.Writer, sandbox.Config) error { return want }
	e, err := New(Config{Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent(testSandboxID, &sandboxv1.SandboxSpec{})
	got := e.HandleSandboxEvent(t.Context(), evt)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if !strings.Contains(got.Error(), testSandboxID) {
		t.Errorf("err message %q should mention sandbox uid", got.Error())
	}
}

// TestLiveSnapshotTracksLifecycle pins the B4d heartbeat-side
// reporting: after a successful run the snapshot reports the
// sandbox with phase=Exited; on a runner error it reports
// phase=Killed. A second snapshot drops the terminal entry so
// future heartbeats don't keep re-reporting it.
func TestLiveSnapshotTracksLifecycle(t *testing.T) {
	okRunner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
	e, err := New(Config{Stdout: io.Discard, Runner: okRunner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(),
		addedEvent("sb-live-1", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	snap := e.LiveSnapshot()
	if len(snap) != 1 || snap[0].GetUid() != "sb-live-1" || snap[0].GetPhase() != "Exited" {
		t.Fatalf("snapshot[0] = %+v, want sb-live-1 Exited", snap[0])
	}
	// Terminal entries drop on the next snapshot — the
	// orchestrator infers "the sandbox is gone" from absence
	// going forward.
	if got := e.LiveSnapshot(); len(got) != 0 {
		t.Errorf("second snapshot = %+v, want empty", got)
	}
}

func TestLiveSnapshotReportsKilledOnRunnerError(t *testing.T) {
	want := errors.New("synthetic boot failure")
	failRunner := func(context.Context, io.Writer, sandbox.Config) error { return want }
	e, err := New(Config{Stdout: io.Discard, Runner: failRunner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = e.HandleSandboxEvent(t.Context(),
		addedEvent("sb-live-bad", &sandboxv1.SandboxSpec{}))
	snap := e.LiveSnapshot()
	if len(snap) != 1 || snap[0].GetPhase() != "Killed" {
		t.Fatalf("snapshot = %+v, want one Killed entry", snap)
	}
}

// TestLiveSnapshotIsSortedByUID pins the deterministic ordering
// the orchestrator relies on for stable trace and golden-file
// output.
func TestLiveSnapshotIsSortedByUID(t *testing.T) {
	okRunner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
	e, err := New(Config{Stdout: io.Discard, Runner: okRunner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, uid := range []string{"sb-c", "sb-a", "sb-b"} {
		e.markPhase(uid, "Running", 0)
	}
	snap := e.LiveSnapshot()
	if len(snap) != 3 || snap[0].GetUid() != "sb-a" || snap[1].GetUid() != "sb-b" || snap[2].GetUid() != "sb-c" {
		t.Errorf("snapshot order = %+v, want sb-a, sb-b, sb-c", snap)
	}
}
