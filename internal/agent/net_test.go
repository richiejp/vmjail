package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/sandboxnet/sandboxnettest"
)

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

func TestStartNetBridgeDisabled(t *testing.T) {
	var w bytes.Buffer
	d := runDeps{dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
		t.Fatal("dial should not have been called")
		return nil, errors.New("unreachable")
	}}
	if got := startNetBridge(t.Context(), &w, d); got != nil {
		t.Fatal("expected nil bridge when net is not enabled")
	}
	if w.Len() != 0 {
		t.Fatalf("unexpected output: %q", w.String())
	}
}

func TestStartNetBridgeDialError(t *testing.T) {
	var w bytes.Buffer
	d := runDeps{
		cfg: &agentv1.GuestConfig{NetEnabled: true},
		dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
			return nil, errors.New("no host listener")
		},
	}
	if got := startNetBridge(t.Context(), &w, d); got != nil {
		t.Fatal("expected nil bridge on dial error")
	}
	if !strings.Contains(w.String(), "no host listener") {
		t.Fatalf("output %q missing dial-error report", w.String())
	}
}

func TestNetBridgeCloseWithBothEndsSet(t *testing.T) {
	// NetBridge.Close has a tap-close branch that only runs when
	// startNetBridge's tap path was wired. The
	// integration test exercises the conn-close branch via
	// pumpFrames teardown but never calls bridge.Close directly,
	// so we cover the both-set path here in isolation.
	connHost, guestSide := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = connHost.Close()
		_ = tapHost.Close()
	})
	b := &NetBridge{conn: guestSide, tap: tapAgent}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if b.conn != nil || b.tap != nil {
		t.Fatalf("Close left state behind: conn=%v tap=%v", b.conn, b.tap)
	}
	// Idempotent.
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStartNetBridgeWithTap(t *testing.T) {
	// Cross-platform tap-path test: fake the tap deps with a
	// net.Pipe pair so the unit test never needs /dev/net/tun or
	// CAP_NET_ADMIN. Exercises attachTapToBridge + pumpFrames glue
	// end-to-end: framed bytes on the vsock side land raw on the
	// tap side after the bridge is up.
	connHost, guestSide := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = connHost.Close()
		_ = tapHost.Close()
	})

	var configured struct {
		name    string
		addr    string
		gateway string
		mtu     int
	}
	d := runDeps{
		cfg: &agentv1.GuestConfig{NetEnabled: true},
		dialVsock: func(_ context.Context, port uint32) (net.Conn, error) {
			if port != DefaultNetVsockPort {
				t.Fatalf("dial port=%d, want %d", port, DefaultNetVsockPort)
			}
			return guestSide, nil
		},
		tap: tapDeps{
			open: func(name string) (io.ReadWriteCloser, string, error) {
				if name != DefaultTapName {
					t.Fatalf("open(%q), want %q", name, DefaultTapName)
				}
				return tapAgent, name, nil
			},
			configure: func(name, addr, gw string, mtu int) error {
				configured.name = name
				configured.addr = addr
				configured.gateway = gw
				configured.mtu = mtu
				return nil
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var w bytes.Buffer
	b := startNetBridge(ctx, &w, d)
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}
	if !strings.Contains(w.String(), fmt.Sprintf("connected port=%d", DefaultNetVsockPort)) ||
		!strings.Contains(w.String(), "tap up name="+DefaultTapName) {
		t.Fatalf("output %q missing connected + tap-up reports", w.String())
	}
	if configured.name != DefaultTapName ||
		configured.addr != DefaultGuestIPNet ||
		configured.gateway != DefaultGuestGw ||
		configured.mtu != DefaultGuestMTU {
		t.Fatalf("configure args = %+v", configured)
	}

	// Drive a frame conn → tap and verify the pump is running.
	frame := []byte("ethernet-payload")
	sandboxnettest.WriteFrame(t, connHost, frame)
	if err := tapHost.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(tapHost, got); err != nil {
		t.Fatalf("read tap: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("tap got %q, want %q", got, frame)
	}

	// Cancel and confirm pumpFrames closes both halves. The close races
	// this goroutine: SetReadDeadline may itself fail once the pipe is
	// closed (io: read/write on closed pipe), which already proves closure;
	// otherwise the deadline bounds the Read so it returns a (non-nil)
	// error rather than blocking. Either way the Read below must error.
	cancel()
	_ = connHost.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := connHost.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected conn close after cancel")
	}
}

func TestStartNetBridgeTapOpenError(t *testing.T) {
	_, guestSide := net.Pipe()
	t.Cleanup(func() { _ = guestSide.Close() })

	var w bytes.Buffer
	d := runDeps{
		cfg: &agentv1.GuestConfig{NetEnabled: true},
		dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
			return guestSide, nil
		},
		tap: tapDeps{
			open: func(string) (io.ReadWriteCloser, string, error) {
				return nil, "", errors.New("no /dev/net/tun")
			},
			configure: func(string, string, string, int) error {
				t.Fatal("configure should not be called after open failure")
				return nil
			},
		},
	}
	if got := startNetBridge(t.Context(), &w, d); got != nil {
		t.Fatal("expected nil bridge on tap open failure")
	}
	if !strings.Contains(w.String(), "no /dev/net/tun") {
		t.Fatalf("output %q missing open-error report", w.String())
	}
}

func TestStartNetBridgeTapConfigureError(t *testing.T) {
	_, guestSide := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = guestSide.Close()
		_ = tapHost.Close()
	})

	var w bytes.Buffer
	d := runDeps{
		cfg: &agentv1.GuestConfig{NetEnabled: true},
		dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
			return guestSide, nil
		},
		tap: tapDeps{
			open: func(name string) (io.ReadWriteCloser, string, error) {
				return tapAgent, name, nil
			},
			configure: func(string, string, string, int) error {
				return errors.New("netlink: addr in use")
			},
		},
	}
	if got := startNetBridge(t.Context(), &w, d); got != nil {
		t.Fatal("expected nil bridge on configure failure")
	}
	if !strings.Contains(w.String(), "netlink: addr in use") {
		t.Fatalf("output %q missing configure-error report", w.String())
	}
}

func TestWriteContainerResolvConfCreatesFile(t *testing.T) {
	bundle := t.TempDir()
	var w bytes.Buffer
	WriteContainerResolvConf(&w, bundle)

	path := filepath.Join(bundle, "rootfs", "etc", "resolv.conf")
	got, err := os.ReadFile(path) //nolint:gosec // test reads back a path the test itself just wrote
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	wantContent := "nameserver " + DefaultGuestGw + "\n"
	if string(got) != wantContent {
		t.Fatalf("resolv.conf = %q, want %q", got, wantContent)
	}
	if !strings.Contains(w.String(), "resolv.conf nameserver="+DefaultGuestGw) {
		t.Fatalf("missing success report: %q", w.String())
	}
}

func TestWriteContainerResolvConfIsIdempotent(t *testing.T) {
	bundle := t.TempDir()
	var w bytes.Buffer
	WriteContainerResolvConf(&w, bundle)
	WriteContainerResolvConf(&w, bundle)

	path := filepath.Join(bundle, "rootfs", "etc", "resolv.conf")
	got, err := os.ReadFile(path) //nolint:gosec // test reads back a path the test itself just wrote
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if string(got) != "nameserver "+DefaultGuestGw+"\n" {
		t.Fatalf("resolv.conf got corrupted across writes: %q", got)
	}
}

func TestWriteContainerResolvConfOverwritesExisting(t *testing.T) {
	// nix-built images may not ship /etc/resolv.conf at all; some
	// others bake in a public resolver. We overwrite because the
	// sandboxnet gateway is the only route the container actually
	// has, so a baked-in 8.8.8.8 would just time out.
	bundle := t.TempDir()
	etc := filepath.Join(bundle, "rootfs", "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil { //nolint:gosec // test fixture rootfs needs the same mode prod uses
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatalf("seed resolv.conf: %v", err)
	}

	var w bytes.Buffer
	WriteContainerResolvConf(&w, bundle)

	got, err := os.ReadFile(filepath.Join(etc, "resolv.conf")) //nolint:gosec // test reads back a path the test itself just wrote
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if !strings.Contains(string(got), DefaultGuestGw) || strings.Contains(string(got), "8.8.8.8") {
		t.Fatalf("resolv.conf = %q, want nameserver=%s", got, DefaultGuestGw)
	}
}

// TestRunWithWritesResolvConfWhenNetAndBundleAreSet pins the
// agent-side composition: when the GuestConfig enables the net
// bridge AND points at an OCI bundle, runWith drops a resolv.conf
// into the bundle's rootfs/etc before launching the container. The
// container shares the VM's netns, so this file is the only DNS
// plumbing it sees.
func TestRunWithWritesResolvConfWhenNetAndBundleAreSet(t *testing.T) {
	bundle := t.TempDir()
	_, guestSide := net.Pipe()
	t.Cleanup(func() { _ = guestSide.Close() })

	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{NetEnabled: true, Bundle: bundle}}
	deps := runDeps{
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
		share:       noopShareDeps(),
		dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
			return guestSide, nil
		},
		container: ContainerDeps{
			Exec: func(_ context.Context, _ io.Writer, _ string) error { return nil },
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	got, err := os.ReadFile(filepath.Join(bundle, "rootfs", "etc", "resolv.conf")) //nolint:gosec // test reads back a path the test itself just wrote
	if err != nil {
		t.Fatalf("expected resolv.conf at <bundle>/rootfs/etc; got %v\nagent output:\n%s", err, buf.String())
	}
	if !strings.Contains(string(got), DefaultGuestGw) {
		t.Fatalf("resolv.conf = %q, want nameserver %s", got, DefaultGuestGw)
	}
}

// TestRunWithSkipsResolvConfWithoutBundle pins the negative branch:
// when no bundle is configured we don't fabricate a rootfs to write
// into.
func TestRunWithSkipsResolvConfWithoutBundle(t *testing.T) {
	_, guestSide := net.Pipe()
	t.Cleanup(func() { _ = guestSide.Close() })

	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{NetEnabled: true}}
	deps := runDeps{
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
		share:       noopShareDeps(),
		dialVsock: func(_ context.Context, _ uint32) (net.Conn, error) {
			return guestSide, nil
		},
		container: ContainerDeps{
			Exec: func(_ context.Context, _ io.Writer, _ string) error { return nil },
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	// No bundle = no rootfs/etc directory should have been created.
	if strings.Contains(buf.String(), "resolv.conf") {
		t.Fatalf("unexpected resolv.conf report when no bundle is set: %q", buf.String())
	}
}

func TestWriteContainerResolvConfReportsMkdirFailure(t *testing.T) {
	// Point the "bundle" at an existing non-dir so MkdirAll fails
	// with ENOTDIR. Catches the error-report branch without root.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte{}, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	var w bytes.Buffer
	WriteContainerResolvConf(&w, f)
	if !strings.Contains(w.String(), "resolv.conf mkdir") {
		t.Fatalf("missing mkdir-error report: %q", w.String())
	}
}

func TestStartNetBridgeWiringHappyPath(t *testing.T) {
	hostSide, guestSide := net.Pipe()
	t.Cleanup(func() { _ = hostSide.Close(); _ = guestSide.Close() })

	var w bytes.Buffer
	d := runDeps{
		cfg: &agentv1.GuestConfig{NetEnabled: true},
		dialVsock: func(_ context.Context, port uint32) (net.Conn, error) {
			if port != DefaultNetVsockPort {
				t.Fatalf("dial port=%d, want %d", port, DefaultNetVsockPort)
			}
			return guestSide, nil
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := startNetBridge(ctx, &w, d)
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}
	if !strings.Contains(w.String(), fmt.Sprintf("connected port=%d", DefaultNetVsockPort)) {
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
