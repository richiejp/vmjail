//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunXFSgrowTraced(t *testing.T) {
	newTrace := func(t *testing.T) *kernelTrace {
		t.Helper()
		root := fakeTracefs(t, xfsCommitEvent)
		trace, err := startKernelTraceAt(root, kernelTraceInstance, []string{xfsCommitEvent}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(trace.root, "trace"), []byte("xfs evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		return trace
	}
	runErr := errors.New("grow failed")
	tests := []struct {
		name  string
		ctx   func() context.Context
		start func() (*kernelTrace, error)
		run   func(context.Context, string, ...string) error
		want  string
	}{
		{name: "success", start: func() (*kernelTrace, error) { return newTrace(t), nil }, run: func(context.Context, string, ...string) error { return nil }},
		{name: "trace unavailable success", start: func() (*kernelTrace, error) { return nil, errors.New("tracefs") }, run: func(context.Context, string, ...string) error { return nil }},
		{name: "trace unavailable failure", start: func() (*kernelTrace, error) { return nil, errors.New("tracefs") }, run: func(context.Context, string, ...string) error { return runErr }, want: "kernel trace unavailable"},
		{name: "failure with evidence", start: func() (*kernelTrace, error) { return newTrace(t), nil }, run: func(context.Context, string, ...string) error { return runErr }, want: "xfs evidence"},
		{name: "timeout", ctx: func() context.Context { ctx, cancel := context.WithCancel(t.Context()); cancel(); return ctx }, start: func() (*kernelTrace, error) { return newTrace(t), nil }, run: func(ctx context.Context, _ string, _ ...string) error { <-ctx.Done(); return ctx.Err() }, want: "context canceled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			err := runXFSgrowTracedWith(ctx, tc.run, "/workspace", tc.start)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunXFSgrowTracedEntryPoint(t *testing.T) {
	if err := runXFSgrowTraced(t.Context(), func(context.Context, string, ...string) error { return nil }, "/workspace"); err != nil {
		t.Fatal(err)
	}
}

func fakeTracefs(t *testing.T, events ...string) string {
	t.Helper()
	root := t.TempDir()
	instance := filepath.Join(root, "instances", kernelTraceInstance)
	for _, name := range append(events, "") {
		dir := instance
		if name != "" {
			dir = filepath.Join(instance, "events", name)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"buffer_size_kb", "trace", "tracing_on"} {
		if err := os.WriteFile(filepath.Join(instance, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range events {
		if err := os.WriteFile(filepath.Join(instance, "events", event, "enable"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestKernelTraceLifecycle(t *testing.T) {
	root := fakeTracefs(t, xfsCommitEvent)
	trace, err := startKernelTraceAt(root, kernelTraceInstance, []string{"missing/event", xfsCommitEvent}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	instance := filepath.Join(root, "instances", kernelTraceInstance)
	if err := os.WriteFile(filepath.Join(instance, "trace"), []byte("xfs_trans_commit sector=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := trace.stop()
	if err != nil || !strings.Contains(report, "sector=1") {
		t.Fatalf("stop = %q, %v", report, err)
	}
}

func TestKernelTraceFailures(t *testing.T) {
	root := fakeTracefs(t)
	if _, err := startKernelTraceAt(root, kernelTraceInstance, nil, func() error { return errors.New("mount") }); err == nil {
		t.Fatal("expected mount error")
	}
	if _, err := startKernelTraceAt(root, kernelTraceInstance, nil, func() error { return nil }); err == nil {
		t.Fatal("expected no-events error")
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := startKernelTraceAt(blocker, kernelTraceInstance, nil, func() error { return nil }); err == nil {
		t.Fatal("expected instance creation error")
	}
	root = fakeTracefs(t, xfsCommitEvent)
	bufferPath := filepath.Join(root, "instances", kernelTraceInstance, "buffer_size_kb")
	if err := os.Remove(bufferPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bufferPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := startKernelTraceAt(root, kernelTraceInstance, []string{xfsCommitEvent}, func() error { return nil }); err == nil {
		t.Fatal("expected buffer-size error")
	}
}

func TestKernelTraceStopFailures(t *testing.T) {
	root := fakeTracefs(t, xfsCommitEvent)
	trace := &kernelTrace{root: filepath.Join(root, "instances", kernelTraceInstance)}
	tracingPath := filepath.Join(trace.root, "tracing_on")
	if err := os.Remove(tracingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tracingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.stop(); err == nil {
		t.Fatal("expected disable error")
	}
	if err := os.Remove(tracingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracingPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(trace.root, "trace")); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.stop(); err == nil {
		t.Fatal("expected open error")
	}
}

func TestKernelTraceBounded(t *testing.T) {
	root := fakeTracefs(t, xfsCommitEvent)
	trace, err := startKernelTraceAt(root, kernelTraceInstance, []string{xfsCommitEvent}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trace.root, "trace"), []byte(strings.Repeat("x", kernelTraceLimit+100)), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := trace.stop()
	if err != nil || !strings.Contains(report, "[trace truncated]") {
		t.Fatalf("bounded stop = %d bytes, %v", len(report), err)
	}
}
