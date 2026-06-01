package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/observability/progress"
)

// observeFixedNow is an arbitrary fixed clock; summaries below are
// stamped relative to it so the snapshot age is deterministic.
var observeFixedNow = time.UnixMilli(1_700_000_000_000)

func mustMarshalSummary(t *testing.T, s progress.Summary) []byte {
	t.Helper()
	data, err := progress.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	return data
}

func TestRenderObserveSummaryEmpty(t *testing.T) {
	out, err := renderObserveSummary(observeFixedNow, "ws", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "workspace: ws") || !strings.Contains(out, "none yet") {
		t.Errorf("empty render = %q", out)
	}
}

func TestRenderObserveSummaryActive(t *testing.T) {
	raw := mustMarshalSummary(t, progress.Summary{
		TakenAtUnixMs: observeFixedNow.UnixMilli(),
		Opensnoop: &progress.Opensnoop{
			Total: 12345, OpenatRate: 842.0, LastEventAgeMs: 100,
			TopComms: []progress.Counted{{Key: "cc1plus", Count: 4201}},
			TopDirs:  []progress.Counted{{Key: "/nix/store/x/include", Count: 3100}},
			TopPaths: []progress.Counted{{Key: "/nix/store/x/stdio.h", Count: 12}},
		},
	})
	out, err := renderObserveSummary(observeFixedNow, "myapp", raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"opens total : 12345",
		"open rate   : 842.0/s",
		"last open   : 0.1s ago",
		"status      : active",
		"cc1plus (4201)",
		"/nix/store/x/include (3100)",
		"/nix/store/x/stdio.h (12)",
		"drill down: daft sandbox observer-tail myapp",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRenderObserveSummaryStalled(t *testing.T) {
	raw := mustMarshalSummary(t, progress.Summary{
		TakenAtUnixMs: observeFixedNow.UnixMilli(),
		Opensnoop:     &progress.Opensnoop{Total: 7, LastEventAgeMs: 6000},
	})
	out, err := renderObserveSummary(observeFixedNow, "ws", raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "possibly stalled") {
		t.Errorf("expected stall flag, got %q", out)
	}
}

func TestRenderObserveSummaryStarting(t *testing.T) {
	raw := mustMarshalSummary(t, progress.Summary{
		TakenAtUnixMs: observeFixedNow.UnixMilli(),
		Opensnoop:     &progress.Opensnoop{Total: 0, LastEventAgeMs: -1},
	})
	out, err := renderObserveSummary(observeFixedNow, "ws", raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "starting") || !strings.Contains(out, "last open   : never") {
		t.Errorf("expected starting/never, got %q", out)
	}
}

func TestRenderObserveSummaryNoOpensnoop(t *testing.T) {
	raw := mustMarshalSummary(t, progress.Summary{TakenAtUnixMs: observeFixedNow.UnixMilli()})
	out, err := renderObserveSummary(observeFixedNow, "ws", raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "open-snoop: not running") {
		t.Errorf("expected not-running note, got %q", out)
	}
}

func TestRenderObserveSummaryStale(t *testing.T) {
	// Snapshot stamped 30s before now ⇒ past observeStaleThreshold.
	raw := mustMarshalSummary(t, progress.Summary{
		TakenAtUnixMs: observeFixedNow.Add(-30 * time.Second).UnixMilli(),
		Opensnoop:     &progress.Opensnoop{Total: 1, LastEventAgeMs: 0},
	})
	out, err := renderObserveSummary(observeFixedNow, "ws", raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("expected stale note, got %q", out)
	}
}

func TestRenderObserveSummaryParseError(t *testing.T) {
	if _, err := renderObserveSummary(observeFixedNow, "ws", []byte("{not json")); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestFmtDuration(t *testing.T) {
	cases := map[time.Duration]string{
		-1 * time.Second:        "0.0s",
		100 * time.Millisecond:  "0.1s",
		3200 * time.Millisecond: "3.2s",
		64 * time.Second:        "1m4s",
	}
	for d, want := range cases {
		if got := fmtDuration(d); got != want {
			t.Errorf("fmtDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// observeReqCapture records the last request the fetch stub saw, passed
// by single pointer (Power of 10 rule 9 forbids double-pointers).
type observeReqCapture struct {
	req *workspacev1.ObserveSummaryRequest
}

func observeStubDeps(t *testing.T, summary []byte, rc *observeReqCapture) observeDeps {
	t.Helper()
	return observeDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			//nolint:nilnil // test fake: client unused by the fetchSummary stub.
			return nil, nil
		},
		fetchSummary: func(_ context.Context, _ workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserveSummaryRequest) (*workspacev1.ObserveSummaryResponse, error) {
			if rc != nil {
				rc.req = req
			}
			return &workspacev1.ObserveSummaryResponse{Summary: summary}, nil
		},
		now: func() time.Time { return observeFixedNow },
	}
}

func TestRunObserveWithWiring(t *testing.T) {
	raw := mustMarshalSummary(t, progress.Summary{
		TakenAtUnixMs: observeFixedNow.UnixMilli(),
		Opensnoop:     &progress.Opensnoop{Total: 5, LastEventAgeMs: 10},
	})
	var rc observeReqCapture
	deps := observeStubDeps(t, raw, &rc)
	var out bytes.Buffer
	opts := observeOpts{orchestrator: testOrchURL, workspace: "obs-ws"}
	if err := runObserveWith(context.Background(), &out, opts, deps); err != nil {
		t.Fatalf("runObserveWith: %v", err)
	}
	if rc.req.GetWorkspace().GetName() != "obs-ws" {
		t.Errorf("req workspace = %q, want obs-ws", rc.req.GetWorkspace().GetName())
	}
	if !strings.Contains(out.String(), "opens total : 5") {
		t.Errorf("output = %q", out.String())
	}
}

func TestRunObserveWithDefaultsWorkspace(t *testing.T) {
	var rc observeReqCapture
	deps := observeStubDeps(t, nil, &rc)
	opts := observeOpts{orchestrator: testOrchURL} // workspace omitted
	if err := runObserveWith(context.Background(), &bytes.Buffer{}, opts, deps); err != nil {
		t.Fatalf("runObserveWith: %v", err)
	}
	if rc.req.GetWorkspace().GetName() == "" {
		t.Error("workspace name should be defaulted from cwd/config, got empty")
	}
}

func TestRunObserveWithDialError(t *testing.T) {
	deps := observeDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return nil, errors.New("dial broke")
		},
		now: func() time.Time { return observeFixedNow },
	}
	opts := observeOpts{orchestrator: testOrchURL, workspace: "ws"}
	if err := runObserveWith(context.Background(), &bytes.Buffer{}, opts, deps); err == nil {
		t.Fatal("expected dial error")
	}
}

func TestRunObserveWithFetchError(t *testing.T) {
	deps := observeDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			//nolint:nilnil // test fake.
			return nil, nil
		},
		fetchSummary: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient, *workspacev1.ObserveSummaryRequest) (*workspacev1.ObserveSummaryResponse, error) {
			return nil, errors.New("fetch broke")
		},
		now: func() time.Time { return observeFixedNow },
	}
	opts := observeOpts{orchestrator: testOrchURL, workspace: "ws"}
	if err := runObserveWith(context.Background(), &bytes.Buffer{}, opts, deps); err == nil {
		t.Fatal("expected fetch error")
	}
}

// TestRunObserveFollowOneIteration covers the --follow loop: a
// pre-cancelled context renders exactly once (the render precedes the
// select) and then exits cleanly.
func TestRunObserveFollowOneIteration(t *testing.T) {
	raw := mustMarshalSummary(t, progress.Summary{
		TakenAtUnixMs: observeFixedNow.UnixMilli(),
		Opensnoop:     &progress.Opensnoop{Total: 1, LastEventAgeMs: 0},
	})
	calls := 0
	deps := observeStubDeps(t, raw, nil)
	inner := deps.fetchSummary
	deps.fetchSummary = func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserveSummaryRequest) (*workspacev1.ObserveSummaryResponse, error) {
		calls++
		return inner(ctx, c, req)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx.Done() ready ⇒ the select exits after the first render
	opts := observeOpts{orchestrator: testOrchURL, workspace: "ws", follow: true, interval: time.Hour}
	var out bytes.Buffer
	if err := runObserveWith(ctx, &out, opts, deps); err != nil {
		t.Fatalf("runObserveWith follow: %v", err)
	}
	if calls != 1 {
		t.Errorf("fetch calls = %d, want 1", calls)
	}
	if !strings.Contains(out.String(), "opens total : 1") {
		t.Errorf("follow output = %q", out.String())
	}
}

// TestSandboxObserveCmdExecutes drives the cobra RunE end to end. With
// no reachable orchestrator it fails at the dial, exercising the command
// wiring + default deps.
func TestSandboxObserveCmdExecutes(t *testing.T) {
	cmd := newSandboxObserveCmd()
	cmd.SetArgs([]string{"--orchestrator", "https://127.0.0.1:1", "--config-dir", t.TempDir(), "ws"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error dialing an unreachable orchestrator")
	}
}
