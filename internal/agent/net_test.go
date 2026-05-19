package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFormatNetPortCmdlineRoundTrips(t *testing.T) {
	for _, port := range []uint32{1, 8000, 9000, 65535} {
		got := FormatNetPortCmdline(port)
		want := "daft.net_port=" + strings.TrimSpace(strings.SplitN(got, "=", 2)[1])
		if got != want {
			t.Fatalf("format = %q, want %q", got, want)
		}
		parsed, ok, err := ParseNetPortCmdline(got)
		if err != nil || !ok || parsed != port {
			t.Fatalf("parse(%q) = (%d, %v, %v), want (%d, true, nil)", got, parsed, ok, err, port)
		}
	}
}

func TestParseNetPortCmdline(t *testing.T) {
	const (
		errEmpty   = "empty"
		errNonNum  = "abc"
		errNonZero = "non-zero"
	)
	cases := []struct {
		name    string
		cmdline string
		port    uint32
		ok      bool
		wantErr string
	}{
		{"absent_token", "console=ttyS0", 0, false, ""},
		{"present_token", "console=ttyS0 daft.net_port=9100", 9100, true, ""},
		{"blank_value", "daft.net_port=", 0, false, errEmpty},
		{"alpha_value", "daft.net_port=abc", 0, false, errNonNum},
		{"zero_value", "daft.net_port=0", 0, false, errNonZero},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, ok, err := ParseNetPortCmdline(tc.cmdline)
			if port != tc.port || ok != tc.ok {
				t.Fatalf("port=%d ok=%v, want %d/%v", port, ok, tc.port, tc.ok)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestStartNetBridgeHappyPath(t *testing.T) {
	hostSide, guestSide := net.Pipe()
	t.Cleanup(func() { _ = hostSide.Close(); _ = guestSide.Close() })
	dial := func(_ context.Context, _ uint32) (net.Conn, error) {
		return guestSide, nil
	}
	b, err := StartNetBridge(t.Context(), dial, 9100)
	if err != nil {
		t.Fatalf("StartNetBridge: %v", err)
	}
	if b.Conn() == nil {
		t.Fatal("Conn() = nil after dial")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := b.Close(); err != nil {
		t.Fatalf("Close (second): %v", err)
	}
}

func TestStartNetBridgeWrapsDialError(t *testing.T) {
	want := errors.New("boom")
	dial := func(_ context.Context, _ uint32) (net.Conn, error) {
		return nil, want
	}
	_, err := StartNetBridge(t.Context(), dial, 9100)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrapping %v", err, want)
	}
}

func TestStartNetBridgeFromCmdlineAbsent(t *testing.T) {
	var w bytes.Buffer
	d := runDeps{dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
		t.Fatal("dial should not have been called")
		return nil, errors.New("unreachable")
	}}
	if got := startNetBridgeFromCmdline(t.Context(), &w, d, "console=ttyS0"); got != nil {
		t.Fatal("expected nil bridge when cmdline lacks key")
	}
	if w.Len() != 0 {
		t.Fatalf("unexpected output: %q", w.String())
	}
}

func TestStartNetBridgeFromCmdlineMalformed(t *testing.T) {
	var w bytes.Buffer
	d := runDeps{dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
		t.Fatal("dial should not have been called")
		return nil, errors.New("unreachable")
	}}
	if got := startNetBridgeFromCmdline(t.Context(), &w, d, "daft.net_port=abc"); got != nil {
		t.Fatal("expected nil bridge on malformed value")
	}
	if !strings.Contains(w.String(), NetReportPrefix) ||
		!strings.Contains(w.String(), "cmdline:") {
		t.Fatalf("output %q missing cmdline-error report", w.String())
	}
}

func TestStartNetBridgeFromCmdlineDialError(t *testing.T) {
	var w bytes.Buffer
	d := runDeps{dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
		return nil, errors.New("no host listener")
	}}
	if got := startNetBridgeFromCmdline(t.Context(), &w, d, "daft.net_port=9100"); got != nil {
		t.Fatal("expected nil bridge on dial error")
	}
	if !strings.Contains(w.String(), "no host listener") {
		t.Fatalf("output %q missing dial-error report", w.String())
	}
}

func TestStartNetBridgeFromCmdlineHappyPath(t *testing.T) {
	hostSide, guestSide := net.Pipe()
	t.Cleanup(func() { _ = hostSide.Close(); _ = guestSide.Close() })

	var w bytes.Buffer
	d := runDeps{dialVsock: func(_ context.Context, port uint32) (net.Conn, error) {
		if port != 9100 {
			t.Fatalf("dial port=%d, want 9100", port)
		}
		return guestSide, nil
	}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := startNetBridgeFromCmdline(ctx, &w, d, "daft.net_port=9100")
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}
	if !strings.Contains(w.String(), "connected port=9100") {
		t.Fatalf("output %q missing connected report", w.String())
	}

	// Cancel must trigger the cleanup goroutine that Closes the
	// bridge. Wait for the conn read on the host side to observe
	// the close; without the cleanup goroutine the read would block
	// forever (net.Pipe has no timeout).
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	if err := hostSide.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 1)
	_, err := hostSide.Read(buf)
	if err == nil {
		t.Fatal("expected read error after bridge closed")
	}
}
