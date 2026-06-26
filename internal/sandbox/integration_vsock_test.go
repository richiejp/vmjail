//go:build linux && integration

package sandbox

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// TestHelloIntegrationShipsAgentSpansOverVsock is the e2e proof of
// the vsock trace channel: boot a real VM with a hybrid-vsock
// device, the in-VM agent ships its boot span over AF_VSOCK to
// the sandbox-host, and we assert the span lands in the host
// Tracer.
//
// This is the e2e half of the agent_test.go fake-pipe test +
// vsock_test.go uds round-trip test. Together they cover the full
// agent → vsock → host wire path on real KVM.
func TestHelloIntegrationShipsAgentSpansOverVsock(t *testing.T) {
	cfg := resolveArtifacts(t)
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(t.TempDir(), "v.sock"),
		GuestCID:   3,
		Trace:      true,
	}

	hostTracer := trace.New("test-host")
	trace.LogOnFailure(t, hostTracer)
	ctx := trace.ContextWithTracer(t.Context(), hostTracer)

	var buf bytes.Buffer
	if err := Hello(ctx, &buf, cfg); err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Fatalf("banner missing from console:\n%s", buf.String())
	}

	// The host listener accepts the agent's batch asynchronously
	// — give it a generous window. Hello already waited for the
	// banner, so the agent has called Shutdown by now; the
	// listener just needs to finish reading the framed payload.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if found, _ := findAgentBootSpan(hostTracer); found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, snap := findAgentBootSpan(hostTracer)
	t.Fatalf("agent.boot span never arrived over vsock; host snapshot=%v\n--- console ---\n%s", snap, buf.String())
}

// findAgentBootSpan reports whether the host Tracer received a
// span named "agent.boot" produced by the daft-agent component.
// Returns the snapshot regardless so a failing test can dump
// what *did* arrive.
func findAgentBootSpan(tr *trace.Tracer) (bool, []string) {
	snap := tr.Snapshot()
	names := make([]string, 0, len(snap))
	found := false
	for _, s := range snap {
		names = append(names, s.Name()+"@"+s.Component())
		if s.Name() == "agent.boot" && s.Component() == "daft-agent" {
			found = true
		}
	}
	return found, names
}
