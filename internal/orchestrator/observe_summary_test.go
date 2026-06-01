package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

func summaryReq(uuid string) *workspacev1.ObserveSummaryRequest {
	return &workspacev1.ObserveSummaryRequest{
		Workspace: &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}},
	}
}

// TestObserveSummaryReturnsSnapshot exercises the public connect wrapper
// end to end (header → fingerprint → resolve → bridge → ReadAll) and
// pins that the bridge is opened with the SUMMARY source, one-shot
// (follow=false), and the reader is closed.
func TestObserveSummaryReturnsSnapshot(t *testing.T) {
	rc := newStringReadCloser("{\"taken_at_unix_ms\":1}\n")
	br := &fakeObserverBridger{rc: rc}
	h, uid, owner := newObserverTestHandlerWithWorkspace(t, br)

	req := connect.NewRequest(summaryReq(uid))
	req.Header().Set(peerFingerprintHeader, owner.String())
	resp, err := h.ObserveSummary(context.Background(), req)
	if err != nil {
		t.Fatalf("ObserveSummary: %v", err)
	}
	if string(resp.Msg.GetSummary()) != "{\"taken_at_unix_ms\":1}\n" {
		t.Errorf("summary = %q, want the snapshot bytes", resp.Msg.GetSummary())
	}
	if br.lastUID != testObserverSandboxUID {
		t.Errorf("bridger uid = %q, want %q", br.lastUID, testObserverSandboxUID)
	}
	if br.lastSource != nodectrlv1.ObserverSource_OBSERVER_SOURCE_SUMMARY {
		t.Errorf("bridger source = %v, want SUMMARY", br.lastSource)
	}
	if br.lastFollow {
		t.Error("summary read must be one-shot (follow=false)")
	}
	if !rc.closed {
		t.Error("reader was not closed")
	}
}

// TestObserveSummaryUnauthenticated covers the missing-peer branch of
// the connect wrapper: no fingerprint header ⇒ Unauthenticated.
func TestObserveSummaryUnauthenticated(t *testing.T) {
	h, uid, _ := newObserverTestHandlerWithWorkspace(t, &fakeObserverBridger{})
	_, err := h.ObserveSummary(context.Background(), connect.NewRequest(summaryReq(uid)))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("err code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestReadObserveSummaryNoBridger(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	h, _ := NewWorkspaceCatalogHandler(cat, nil)
	_, err := h.readObserveSummary(context.Background(), identity.Fingerprint{}, summaryReq("x"))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("err code = %v, want Unimplemented", connect.CodeOf(err))
	}
}

func TestReadObserveSummaryMissingHandle(t *testing.T) {
	h, _, owner := newObserverTestHandlerWithWorkspace(t, &fakeObserverBridger{})
	_, err := h.readObserveSummary(context.Background(), owner, &workspacev1.ObserveSummaryRequest{})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("err code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestReadObserveSummaryNotRunning(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	h, _ := NewWorkspaceCatalogHandler(cat, nil)
	h.SetObserverBridger(&fakeObserverBridger{})
	owner := identity.Fingerprint{0xaa}
	uid := "ws-idle-summary"
	if _, err := cat.Create(workspacecatalog.CreateInput{UUID: uid, Name: "idle-sum", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := h.readObserveSummary(context.Background(), owner, summaryReq(uid))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("err code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestReadObserveSummaryBridgerError(t *testing.T) {
	br := &fakeObserverBridger{err: errors.New("synthetic summary bridge fail")}
	h, uid, owner := newObserverTestHandlerWithWorkspace(t, br)
	if _, err := h.readObserveSummary(context.Background(), owner, summaryReq(uid)); err == nil {
		t.Fatal("expected bridger error")
	}
}

func TestReadObserveSummaryUnknownWorkspace(t *testing.T) {
	h, _, owner := newObserverTestHandlerWithWorkspace(t, &fakeObserverBridger{})
	if _, err := h.readObserveSummary(context.Background(), owner, summaryReq("no-such-uuid")); err == nil {
		t.Fatal("expected an error resolving an unknown workspace")
	}
}

// chunkReader yields data once, then returns endErr alongside zero bytes
// on the next Read — modelling the node bridge handing back the snapshot
// and then closing the ObserverStream.
type chunkReader struct {
	data   []byte
	endErr error
	done   bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, r.endErr
}

// TestDrainObserveSummaryWrappedEOF is the regression test for the
// "unknown: EOF" bug: the bidi ObserverStream returns a *wrapped* io.EOF
// on half-close, which io.ReadAll (identity compare) would surface as an
// error. drainObserveSummary must treat it — and expected pump-exits — as
// a clean end and return the bytes.
func TestDrainObserveSummaryWrappedEOF(t *testing.T) {
	for _, end := range []error{
		io.EOF,
		fmt.Errorf("nodectrl: ObserverStream receive: %w", io.EOF), // connect-style wrap
		fmt.Errorf("closed: %w", net.ErrClosed),
	} {
		got, err := drainObserveSummary(&chunkReader{data: []byte("{\"total\":3}"), endErr: end})
		if err != nil {
			t.Errorf("end=%v: drain returned error: %v", end, err)
		}
		if string(got) != "{\"total\":3}" {
			t.Errorf("end=%v: data = %q, want the snapshot", end, got)
		}
	}
}

func TestDrainObserveSummaryRealError(t *testing.T) {
	if _, err := drainObserveSummary(&chunkReader{data: []byte("partial"), endErr: errors.New("boom")}); err == nil {
		t.Fatal("a non-EOF stream error must surface, not be swallowed")
	}
}

func TestDrainObserveSummaryCap(t *testing.T) {
	// bytes.Reader streams in chunk-sized Reads, so the running total
	// crosses the cap mid-stream (chunkReader delivers only one chunk).
	big := bytes.Repeat([]byte("x"), maxObserveSummaryBytes+1)
	_, err := drainObserveSummary(bytes.NewReader(big))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("oversized snapshot err = %v, want ResourceExhausted", connect.CodeOf(err))
	}
}
