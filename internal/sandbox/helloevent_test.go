package sandbox

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/observability"
)

// bootEventRunner is a fake VMM whose console stays open (like a real
// CH's stdout) until its ctx is cancelled — mirroring CommandContext, so
// waitForBootComplete's "cancel the VM, then drain" path works.
type bootEventRunner struct{ output string }

func (b bootEventRunner) run(ctx context.Context, _ Config, _ []string, _ string, _ func() *observability.NetSample) (io.ReadCloser, func(), error) {
	pr, pw := io.Pipe()
	go func() { _, _ = io.WriteString(pw, b.output) }()
	go func() { <-ctx.Done(); _ = pw.Close() }()
	return pr, func() { _ = pw.Close() }, nil
}

// TestHelloWithWaitsForBootCompleteEvent exercises helloWith's event
// path: with a vsock device present it waits for the agent's
// BOOT_COMPLETE over the control channel (not the console banner), while
// still mirroring the console into the buffer.
func TestHelloWithWaitsForBootCompleteEvent(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: filepath.Join(dir, "v.sock"), GuestCID: 3}

	// Stand in for the in-VM agent: once the config socket exists,
	// handshake, read the config, then report boot-complete.
	go func() {
		sock := configSockPath(cfg)
		var conn net.Conn
		//daft:lint:disable=forloopbound reason="bounded retry until the server's listener is up"
		for range 200 {
			c, err := net.Dial("unix", sock)
			if err == nil {
				conn = c
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if conn == nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = agent.WriteGuestConfigRequest(conn, &agentv1.GuestConfigRequest{AgentVersion: "1"})
		_, _ = agent.ReadGuestConfig(conn)
		_ = agent.WriteAgentEvent(conn, &agentv1.AgentEvent{Kind: agentv1.AgentEvent_BOOT_COMPLETE})
		// Hold the conn open briefly so the host reads the event before EOF.
		time.Sleep(50 * time.Millisecond)
	}()

	var buf bytes.Buffer
	err := helloWith(context.Background(), &buf, cfg, bootEventRunner{output: agent.Banner + "\n"}, &fakeShareSidecar{})
	if err != nil {
		t.Fatalf("helloWith: %v", err)
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Errorf("console not mirrored into buffer: %q", buf.String())
	}
}
