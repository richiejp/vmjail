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

	"github.com/richiejp/darkaifactory/internal/agent"
	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/observability"
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

// replyBootComplete stands in for the in-VM agent: once the config
// socket exists it handshakes, reads the config, then reports
// BOOT_COMPLETE over the control channel.
func replyBootComplete(cfg Config) {
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
}

// TestHelloWithWaitsForBootCompleteEvent exercises helloWith's event
// path: with a vsock device present it waits for the agent's
// BOOT_COMPLETE over the control channel (not the console banner), while
// still mirroring the console into the buffer.
func TestHelloWithWaitsForBootCompleteEvent(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: filepath.Join(dir, "v.sock"), GuestCID: 3}

	go replyBootComplete(cfg)

	var buf bytes.Buffer
	err := helloWith(context.Background(), &buf, cfg, bootEventRunner{output: agent.Banner + "\n"}, &fakeShareSidecar{})
	if err != nil {
		t.Fatalf("helloWith: %v", err)
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Errorf("console not mirrored into buffer: %q", buf.String())
	}
}

// TestRunToCompletionZeroTimeoutWaitsOnContext proves the one-shot build
// runner's path: with timeout == 0 (no HelloTimeout cap) it still returns
// the moment the agent reports BOOT_COMPLETE — the signal the in-sandbox
// builder relies on to know the build container has exited.
func TestRunToCompletionZeroTimeoutWaitsOnContext(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{HostSocket: filepath.Join(dir, "v.sock"), GuestCID: 3}

	go replyBootComplete(cfg)

	var buf bytes.Buffer
	err := helloWithTimeout(context.Background(), &buf, cfg,
		bootEventRunner{output: agent.Banner + "\n"}, &fakeShareSidecar{}, 0)
	if err != nil {
		t.Fatalf("helloWithTimeout(0): %v", err)
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Errorf("console not mirrored into buffer: %q", buf.String())
	}
}

func TestRunToCompletionRejectsInvalidConfig(t *testing.T) {
	if err := RunToCompletion(t.Context(), io.Discard, Config{}); err == nil {
		t.Fatal("RunToCompletion accepted invalid config")
	}
}

// TestContextWithOptionalTimeout covers both arms: a zero timeout yields a
// ctx bounded only by its parent (RunToCompletion's long-build case),
// while a positive timeout installs one of its own (Hello's 30s cap).
func TestContextWithOptionalTimeout(t *testing.T) {
	t.Run("zero inherits parent only", func(t *testing.T) {
		ctx, cancel := contextWithOptionalTimeout(context.Background(), 0)
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("zero timeout must not install a deadline")
		}
	})
	t.Run("positive installs a deadline", func(t *testing.T) {
		ctx, cancel := contextWithOptionalTimeout(context.Background(), time.Hour)
		defer cancel()
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("positive timeout must install a deadline")
		}
	})
}
