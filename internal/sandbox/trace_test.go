package sandbox

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/richiejp/vmjail/internal/agent"
	"github.com/richiejp/vmjail/internal/trace"
)

// traceCtx returns a context with a fresh Tracer attached and
// registers a Cleanup that dumps the snapshot via tb.Logf on
// failure. Used by both unit and integration tests so a failing
// boot surfaces the per-stage timing breakdown automatically.
func traceCtx(tb testing.TB) context.Context {
	tb.Helper()
	tr := trace.New("test")
	trace.LogOnFailure(tb, tr)
	if t, ok := tb.(*testing.T); ok {
		return trace.ContextWithTracer(t.Context(), tr)
	}
	if b, ok := tb.(*testing.B); ok {
		return trace.ContextWithTracer(b.Context(), tr)
	}
	return trace.ContextWithTracer(context.Background(), tr)
}

// TestHelloEmitsExpectedSpanTree drives helloWith with fakes and a
// captured Tracer to verify the boot path produces the trace shape
// PLAN.md advertises: one vmjail.sandbox.hello root with three child
// spans for the boot pipeline stages.
func TestHelloEmitsExpectedSpanTree(t *testing.T) {
	ctx := traceCtx(t)
	tr := trace.TracerFromContext(ctx)

	r := &fakeRunner{output: "boot line\n" + agent.Banner + "\n"}
	cfg := validCfg()
	cfg.Virtiofsd = testFakeVirtiofsd
	cfg.Shares = []*ShareConfig{{HostDir: t.TempDir(), Tag: "t", Mountpoint: "/share"}}
	if err := helloWith(ctx, &bytes.Buffer{}, cfg, r,
		&fakeShareSidecar{sockets: []string{t.TempDir() + "/sock"}}); err != nil {
		t.Fatalf("helloWith: %v", err)
	}

	snap := tr.Snapshot()
	want := []string{
		"sandbox.Hello",
		"sandbox.start_virtiofsd",
		"sandbox.run_cloud_hypervisor",
		"sandbox.scan_for_banner",
	}
	got := map[string]*trace.Span{}
	for _, s := range snap {
		got[s.Name()] = s
	}
	for _, name := range want {
		if got[name] == nil {
			t.Errorf("missing span %q in snapshot:\n%s", name, dumpSnap(snap))
		}
	}
	if len(got) > 0 {
		root := got["sandbox.Hello"]
		for _, s := range snap {
			if s.SpanID() == root.SpanID() {
				continue
			}
			if s.TraceID() != root.TraceID() {
				t.Errorf("span %q has trace_id %s, want %s",
					s.Name(), s.TraceID(), root.TraceID())
			}
		}
	}
}

func TestHelloSkipsStartVirtiofsdSpanWhenNoShares(t *testing.T) {
	ctx := traceCtx(t)
	tr := trace.TracerFromContext(ctx)

	r := &fakeRunner{output: agent.Banner + "\n"}
	if err := helloWith(ctx, &bytes.Buffer{}, validCfg(), r, noShareSidecar{}); err != nil {
		t.Fatalf("helloWith: %v", err)
	}
	for _, s := range tr.Snapshot() {
		if s.Name() == "sandbox.start_virtiofsd" {
			t.Errorf("start_virtiofsd span emitted when no shares configured")
		}
	}
}

func TestHelloRecordsValidationErrorOnSpan(t *testing.T) {
	ctx := traceCtx(t)
	tr := trace.TracerFromContext(ctx)

	if err := helloWith(ctx, &bytes.Buffer{}, Config{}, &fakeRunner{}, noShareSidecar{}); err == nil {
		t.Fatal("expected validation error")
	}
	snap := tr.Snapshot()
	var root *trace.Span
	for _, s := range snap {
		if s.Name() == "sandbox.Hello" {
			root = s
			break
		}
	}
	if root == nil {
		t.Fatal("missing sandbox.Hello span")
	}
	if root.TraceID() == "" {
		t.Error("root span missing trace_id")
	}
}

func TestHelloRecordsRunErrorOnSpan(t *testing.T) {
	ctx := traceCtx(t)
	tr := trace.TracerFromContext(ctx)

	r := &fakeRunner{startErr: errors.New("synthetic")}
	if err := helloWith(ctx, &bytes.Buffer{}, validCfg(), r, noShareSidecar{}); err == nil {
		t.Fatal("expected run error")
	}
	if !sawError(tr, "sandbox.run_cloud_hypervisor") {
		t.Errorf("expected error event on run_cloud_hypervisor span:\n%s",
			dumpSnap(tr.Snapshot()))
	}
}

func sawError(tr *trace.Tracer, spanName string) bool {
	for _, s := range tr.Snapshot() {
		if s.Name() == spanName {
			// status is private; check via the formatter, which
			// renders ERR for status=Error.
			var b strings.Builder
			_ = trace.FormatText(&b, []*trace.Span{s})
			if strings.Contains(b.String(), "ERR") {
				return true
			}
		}
	}
	return false
}

func dumpSnap(snap []*trace.Span) string {
	var b strings.Builder
	_ = trace.FormatText(&b, snap)
	return b.String()
}
