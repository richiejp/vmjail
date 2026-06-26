package nodectrlclient

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
)

// fakeObserverStream is the in-process observerStreamRPC counterpart
// to fakeAttachStream, typed for ObserverStreamMessage.
type fakeObserverStream struct {
	mu       sync.Mutex
	incoming chan *nodectrlv1.ObserverStreamMessage
	outgoing []*nodectrlv1.ObserverStreamMessage
	closeRsp bool
	recvErr  error
}

func newFakeObserverStream() *fakeObserverStream {
	return &fakeObserverStream{incoming: make(chan *nodectrlv1.ObserverStreamMessage, 8)}
}

func (s *fakeObserverStream) Send(m *nodectrlv1.ObserverStreamMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeObserverStream) Receive() (*nodectrlv1.ObserverStreamMessage, error) {
	s.mu.Lock()
	err := s.recvErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m, ok := <-s.incoming
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (s *fakeObserverStream) CloseRequest() error { return nil }

func (s *fakeObserverStream) CloseResponse() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeRsp {
		return nil
	}
	s.closeRsp = true
	close(s.incoming)
	return nil
}

func (s *fakeObserverStream) sentData() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, m := range s.outgoing {
		b.Write(m.GetData())
	}
	return b.String()
}

func (s *fakeObserverStream) sentHello() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.outgoing {
		if h := m.GetHello(); h != nil {
			return h.GetSessionId()
		}
	}
	return ""
}

type fakeResolver struct {
	path string
	err  error
}

func (f fakeResolver) Resolve(string) (string, error) { return f.path, f.err }

// TestRunObserverHelloResolveAndTail covers the happy path: the
// handler sends a Hello echoing session_id, resolves the path, and
// streams the tail bytes as data frames.
func TestRunObserverHelloResolveAndTail(t *testing.T) {
	stream := newFakeObserverStream()
	req := &nodectrlv1.ObserverRequest{SessionId: "sess-1", SandboxUid: "sb-1", Follow: false}
	tail := func(_ context.Context, path string, follow bool, w io.Writer) error {
		if path != "/state/sb-1/observer.jsonl" || follow {
			t.Errorf("tail args path=%q follow=%v", path, follow)
		}
		_, _ = io.WriteString(w, "sample-line\n")
		return nil
	}
	err := runObserver(context.Background(), req, stream,
		fakeResolver{path: "/state/sb-1/observer.jsonl"}, tail)
	if err != nil {
		t.Fatalf("runObserver: %v", err)
	}
	if got := stream.sentHello(); got != "sess-1" {
		t.Errorf("hello session = %q, want sess-1", got)
	}
	if got := stream.sentData(); got != "sample-line\n" {
		t.Errorf("streamed data = %q, want the tail bytes", got)
	}
}

// TestRunObserverResolveError surfaces a resolver miss as an error
// without streaming anything.
func TestRunObserverResolveError(t *testing.T) {
	stream := newFakeObserverStream()
	req := &nodectrlv1.ObserverRequest{SessionId: "s", SandboxUid: "ghost"}
	called := false
	tail := func(context.Context, string, bool, io.Writer) error { called = true; return nil }
	err := runObserver(context.Background(), req, stream,
		fakeResolver{err: errors.New("no route")}, tail)
	if err == nil {
		t.Fatal("expected resolve error")
	}
	if called {
		t.Error("tail must not run when resolve fails")
	}
}

// TestRunObserverCancelOnOrchestratorClose proves that when the
// orchestrator closes its return direction (Receive errors), the
// tail's context is cancelled so a follow tail stops.
func TestRunObserverCancelOnOrchestratorClose(t *testing.T) {
	stream := newFakeObserverStream()
	req := &nodectrlv1.ObserverRequest{SessionId: "s", SandboxUid: "sb", Follow: true}
	tailCtxDone := make(chan struct{})
	tail := func(ctx context.Context, _ string, _ bool, _ io.Writer) error {
		<-ctx.Done() // a real follow tail blocks until cancelled
		close(tailCtxDone)
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- runObserver(context.Background(), req, stream, fakeResolver{path: "/p"}, tail)
	}()
	// Simulate the orchestrator closing the stream → Receive returns EOF.
	_ = stream.CloseResponse()
	select {
	case <-tailCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("tail ctx not cancelled after orchestrator close")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runObserver = %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runObserver did not return after cancel")
	}
}

// TestObserverFrameWriter covers the io.Writer adapter: empty writes
// are a no-op, normal writes become data frames, and a Send error
// surfaces.
func TestObserverFrameWriter(t *testing.T) {
	stream := newFakeObserverStream()
	w := observerFrameWriter{stream: stream}
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Errorf("Write(nil) = (%d,%v), want (0,nil)", n, err)
	}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Errorf("Write(abc) = (%d,%v), want (3,nil)", n, err)
	}
	if got := stream.sentData(); got != "abc" {
		t.Errorf("sent = %q, want abc", got)
	}

	bad := &failOnSend{}
	wb := observerFrameWriter{stream: bad}
	if _, err := wb.Write([]byte("x")); err == nil {
		t.Error("expected Send error to surface from Write")
	}
}

// failOnSend is an observerStreamRPC whose Send always errors.
type failOnSend struct{ fakeObserverStream }

func (f *failOnSend) Send(*nodectrlv1.ObserverStreamMessage) error {
	return errors.New("send broke")
}

// TestRunObserverTailError surfaces a non-expected tail error.
func TestRunObserverTailError(t *testing.T) {
	stream := newFakeObserverStream()
	req := &nodectrlv1.ObserverRequest{SessionId: "s", SandboxUid: "sb"}
	tail := func(context.Context, string, bool, io.Writer) error {
		return errors.New("tail blew up")
	}
	err := runObserver(context.Background(), req, stream, fakeResolver{path: "/p"}, tail)
	if err == nil {
		t.Fatal("expected the tail error to surface")
	}
}

// TestHandleObserverNilResolver pins the misconfiguration guard (the
// default LOG source with no ObserverResolver wired).
func TestHandleObserverNilResolver(t *testing.T) {
	c := &Client{cfg: Config{}}
	if err := c.handleObserver(context.Background(), &nodectrlv1.ObserverRequest{}); err == nil {
		t.Fatal("expected error when ObserverResolver is unset")
	}
}

// TestObserverResolverFor pins the source → resolver routing: LOG (and
// unspecified, for back-compat) select ObserverResolver; SUMMARY selects
// SummaryResolver. Distinct resolver paths make the selection observable.
func TestObserverResolverFor(t *testing.T) {
	const obsPath, sumPath = "/obs-route", "/sum-route"
	c := &Client{cfg: Config{
		ObserverResolver: fakeResolver{path: obsPath},
		SummaryResolver:  fakeResolver{path: sumPath},
	}}
	cases := []struct {
		source nodectrlv1.ObserverSource
		want   string
	}{
		{nodectrlv1.ObserverSource_OBSERVER_SOURCE_UNSPECIFIED, obsPath},
		{nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, obsPath},
		{nodectrlv1.ObserverSource_OBSERVER_SOURCE_SUMMARY, sumPath},
	}
	for _, tc := range cases {
		r, err := c.observerResolverFor(tc.source)
		if err != nil {
			t.Fatalf("source=%v: %v", tc.source, err)
		}
		if got, _ := r.Resolve("uid"); got != tc.want {
			t.Errorf("source=%v resolver path = %q, want %q", tc.source, got, tc.want)
		}
	}
}

// TestObserverResolverForMissingSummary pins the SUMMARY misconfiguration
// guard: a summary request with no SummaryResolver wired errors rather
// than silently falling through to the observer log.
func TestObserverResolverForMissingSummary(t *testing.T) {
	c := &Client{cfg: Config{ObserverResolver: fakeResolver{path: "/obs-only"}}}
	if _, err := c.observerResolverFor(nodectrlv1.ObserverSource_OBSERVER_SOURCE_SUMMARY); err == nil {
		t.Fatal("expected error when SummaryResolver is unset for source=SUMMARY")
	}
}
