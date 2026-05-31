package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// fakeObserverClientStream is an in-process observerClientStream:
// Receive walks the canned chunks, then Err is returned.
type fakeObserverClientStream struct {
	chunks [][]byte
	idx    int
	err    error
	closed bool
}

func (s *fakeObserverClientStream) Receive() bool {
	if s.idx < len(s.chunks) {
		s.idx++
		return true
	}
	return false
}

func (s *fakeObserverClientStream) Msg() *workspacev1.ObserverChunk {
	return &workspacev1.ObserverChunk{Data: s.chunks[s.idx-1]}
}

func (s *fakeObserverClientStream) Err() error { return s.err }

func (s *fakeObserverClientStream) Close() error { s.closed = true; return nil }

func TestPumpObserverTailWritesChunks(t *testing.T) {
	stream := &fakeObserverClientStream{chunks: [][]byte{[]byte("a\n"), {}, []byte("b\n")}}
	var out bytes.Buffer
	if err := pumpObserverTail(&out, stream); err != nil {
		t.Fatalf("pumpObserverTail: %v", err)
	}
	if out.String() != "a\nb\n" {
		t.Errorf("output = %q, want a\\nb\\n (empty chunk skipped)", out.String())
	}
	if !stream.closed {
		t.Error("stream was not closed")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("stdout broke") }

func TestPumpObserverTailWriteError(t *testing.T) {
	stream := &fakeObserverClientStream{chunks: [][]byte{[]byte("x\n")}}
	if err := pumpObserverTail(errWriter{}, stream); err == nil {
		t.Fatal("expected a stdout write error to surface")
	}
}

func TestPumpObserverTailStreamError(t *testing.T) {
	stream := &fakeObserverClientStream{err: errors.New("stream broke")}
	if err := pumpObserverTail(&bytes.Buffer{}, stream); err == nil {
		t.Fatal("expected the stream error to surface")
	}
}

func TestRunObserverTailWithWiring(t *testing.T) {
	stream := &fakeObserverClientStream{chunks: [][]byte{[]byte("sample\n")}}
	var gotReq *workspacev1.ObserverTailRequest
	deps := observerTailDeps{
		dialOrchestrator: func(_, orch string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			if orch != "https://orch.invalid:8443" {
				t.Errorf("orchestrator = %q, want the flag value", orch)
			}
			//nolint:nilnil // test fake: the client value is unused by the openStream stub below.
			return nil, nil
		},
		openStream: func(_ context.Context, _ workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserverTailRequest) (observerClientStream, error) {
			gotReq = req
			return stream, nil
		},
	}
	var out bytes.Buffer
	opts := observerTailOpts{orchestrator: "https://orch.invalid:8443", workspace: "ws1", follow: true}
	if err := runObserverTailWith(context.Background(), &out, opts, deps); err != nil {
		t.Fatalf("runObserverTailWith: %v", err)
	}
	if out.String() != "sample\n" {
		t.Errorf("output = %q, want sample\\n", out.String())
	}
	if gotReq.GetWorkspace().GetName() != "ws1" || !gotReq.GetFollow() {
		t.Errorf("req = %+v, want workspace=ws1 follow=true", gotReq)
	}
}

func TestRunObserverTailWithDialError(t *testing.T) {
	deps := observerTailDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return nil, errors.New("dial broke")
		},
	}
	opts := observerTailOpts{orchestrator: testOrchURL, workspace: "ws"}
	if err := runObserverTailWith(context.Background(), &bytes.Buffer{}, opts, deps); err == nil {
		t.Fatal("expected dial error")
	}
}

// TestRunObserverTailWithDefaultsWorkspace covers the
// empty-workspace branch: the name is resolved from cwd/config.
func TestRunObserverTailWithDefaultsWorkspace(t *testing.T) {
	stream := &fakeObserverClientStream{chunks: [][]byte{[]byte("x\n")}}
	var gotReq *workspacev1.ObserverTailRequest
	deps := observerTailDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			//nolint:nilnil // test fake: client unused by openStream stub.
			return nil, nil
		},
		openStream: func(_ context.Context, _ workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserverTailRequest) (observerClientStream, error) {
			gotReq = req
			return stream, nil
		},
	}
	// workspace omitted; orchestrator set so resolveOrchestrator
	// short-circuits to the flag without touching client config.
	opts := observerTailOpts{orchestrator: testOrchURL}
	if err := runObserverTailWith(context.Background(), &bytes.Buffer{}, opts, deps); err != nil {
		t.Fatalf("runObserverTailWith: %v", err)
	}
	if gotReq.GetWorkspace().GetName() == "" {
		t.Error("workspace name should be defaulted from cwd/config, got empty")
	}
}

// TestSandboxObserverTailCmdExecutes drives the cobra RunE end to end.
// With no client config it fails at the dial, which still exercises
// the command wiring + default deps.
func TestSandboxObserverTailCmdExecutes(t *testing.T) {
	cmd := newSandboxObserverTailCmd()
	cmd.SetArgs([]string{"--orchestrator", "https://127.0.0.1:1", "--config-dir", t.TempDir(), "ws"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	// A real dial against a closed port (or missing identity) errors;
	// we only need RunE → runObserverTail → defaultObserverTailDeps
	// executed.
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error dialing an unreachable orchestrator")
	}
}

func TestRunObserverTailWithNilStream(t *testing.T) {
	deps := observerTailDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			//nolint:nilnil // test fake.
			return nil, nil
		},
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient, *workspacev1.ObserverTailRequest) (observerClientStream, error) {
			//nolint:nilnil // deliberately exercises the nil-stream guard.
			return nil, nil
		},
	}
	opts := observerTailOpts{orchestrator: testOrchURL, workspace: "ws"}
	if err := runObserverTailWith(context.Background(), &bytes.Buffer{}, opts, deps); err == nil {
		t.Fatal("expected an error for a nil stream")
	}
}

func TestRunObserverTailWithOpenStreamError(t *testing.T) {
	deps := observerTailDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			//nolint:nilnil // test fake.
			return nil, nil
		},
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient, *workspacev1.ObserverTailRequest) (observerClientStream, error) {
			return nil, errors.New("open broke")
		},
	}
	opts := observerTailOpts{orchestrator: testOrchURL, workspace: "ws"}
	if err := runObserverTailWith(context.Background(), &bytes.Buffer{}, opts, deps); err == nil {
		t.Fatal("expected the openStream error")
	}
}

func TestNewSandboxObserverTailCmdShape(t *testing.T) {
	cmd := newSandboxObserverTailCmd()
	if cmd.Use != "observer-tail [workspace]" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if f := cmd.Flags().Lookup("follow"); f == nil || f.DefValue != "true" {
		t.Errorf("--follow flag missing or not defaulted true: %+v", f)
	}
}
