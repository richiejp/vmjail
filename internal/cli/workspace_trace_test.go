package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/trace"
)

// TestWorkspacePullTraceShape drives serve+pull in-process with a
// shared Tracer planted on the context, then inspects the resulting
// span tree. This is the ergonomic reference point for "how do I
// debug a failing workspace pull from the trace?"
func TestWorkspacePullTraceShape(t *testing.T) {
	serverDir, clientDir := approveBoth(t)

	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))
	mustWriteFile(t, filepath.Join(src, "blob.bin"), bytes.Repeat([]byte("Z"), 4096))

	addr := freeAddr(t)
	tracer := trace.New("test")
	trace.LogOnFailure(t, tracer)
	ctx := trace.ContextWithTracer(context.Background(), tracer)

	serveCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := runWorkspaceServe(serveCtx, &syncBuf{}, workspaceServeOpts{
			workspaceFlag: "ws=" + src,
			addr:          addr,
			configDir:     serverDir,
			casDir:        filepath.Join(serverDir, "cas"),
		})
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	waitForListener(t, addr, 5*time.Second)

	dest := t.TempDir()
	pullCtx, pullCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pullCancel()
	if err := runWorkspacePull(pullCtx, &syncBuf{},
		addr, "ws", dest, clientDir, filepath.Join(clientDir, "cas"), false); err != nil {
		t.Fatalf("pull: %v", err)
	}

	snap := tracer.Snapshot()
	requireSpan(t, snap, "daft.workspace.pull", trace.SpanKindInternal)
	requireSpan(t, snap, "workspace.ApplyManifest", trace.SpanKindInternal)
	requireSpan(t, snap, "/daft.workspace.v1.WorkspaceService/GetManifest", trace.SpanKindClient)
	requireSpan(t, snap, "/daft.workspace.v1.WorkspaceService/GetManifest", trace.SpanKindServer)
	requireSpan(t, snap, "/daft.workspace.v1.WorkspaceService/GetChunk", trace.SpanKindClient)
	requireSpan(t, snap, "/daft.workspace.v1.WorkspaceService/GetChunk", trace.SpanKindServer)

	root := findSpan(snap, "daft.workspace.pull", trace.SpanKindInternal)
	if root == nil {
		t.Fatal("missing root span (already asserted but findSpan returned nil)")
	}
	for _, s := range snap {
		if s.SpanID() == root.SpanID() {
			continue
		}
		if s.TraceID() != root.TraceID() {
			t.Errorf("span %q has trace_id %s, want %s (must share the root's trace)",
				s.Name(), s.TraceID(), root.TraceID())
		}
	}

	getChunks := countSpans(snap, "/daft.workspace.v1.WorkspaceService/GetChunk", trace.SpanKindClient)
	if getChunks < 2 {
		t.Errorf("expected >=2 GetChunk client spans (alpha.txt + blob.bin), got %d", getChunks)
	}
}

func waitForListener(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{}).Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener never came up at %s", addr)
}

func findSpan(snap []*trace.Span, name string, kind trace.SpanKind) *trace.Span {
	for _, s := range snap {
		if s.Name() == name && s.Kind() == kind {
			return s
		}
	}
	return nil
}

func requireSpan(t *testing.T, snap []*trace.Span, name string, kind trace.SpanKind) {
	t.Helper()
	if findSpan(snap, name, kind) == nil {
		t.Errorf("missing span name=%q kind=%v in snapshot:\n%s",
			name, kind, dumpSnap(snap))
	}
}

func countSpans(snap []*trace.Span, name string, kind trace.SpanKind) int {
	n := 0
	for _, s := range snap {
		if s.Name() == name && s.Kind() == kind {
			n++
		}
	}
	return n
}

func dumpSnap(snap []*trace.Span) string {
	var b strings.Builder
	_ = trace.FormatText(&b, snap)
	return b.String()
}
