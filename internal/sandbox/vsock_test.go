package sandbox

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// Fixed absolute path used by validate-only tests that don't open
// the socket — validate() only requires absoluteness.
const testHostSocketPath = "/tmp/v.sock"

// TestBuildCloudHypervisorArgsEmitsVsockFlag confirms the --vsock
// flag is emitted with the expected cid/socket pair when a
// VsockConfig is attached.
func TestBuildCloudHypervisorArgsEmitsVsockFlag(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		TracePort:  8000,
	}
	args := buildCloudHypervisorArgs(cfg, nil)

	idx := -1
	for i, a := range args {
		if a == "--vsock" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("--vsock not present: %v", args)
	}
	if idx+1 >= len(args) {
		t.Fatalf("--vsock at end with no value: %v", args)
	}
	want := "cid=3,socket=/tmp/v.sock"
	if args[idx+1] != want {
		t.Errorf("--vsock value = %q, want %q", args[idx+1], want)
	}
}

// TestBuildCloudHypervisorArgsOmitsVsockWhenAbsent makes sure we
// don't accidentally emit --vsock with a zero-value VsockConfig.
func TestBuildCloudHypervisorArgsOmitsVsockWhenAbsent(t *testing.T) {
	cfg := validCfg().withDefaults()
	args := buildCloudHypervisorArgs(cfg, nil)
	for _, a := range args {
		if a == "--vsock" {
			t.Errorf("unexpected --vsock in args: %v", args)
		}
	}
}

// TestBuildKernelCmdlineEmitsVsockTracePort confirms the agent
// will see daft.vsock.trace_port on the cmdline when TracePort is
// set.
func TestBuildKernelCmdlineEmitsVsockTracePort(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		TracePort:  8000,
	}
	got := buildKernelCmdline(cfg)
	if !strings.Contains(got, "daft.vsock.trace_port=8000") {
		t.Errorf("cmdline %q missing trace port", got)
	}
}

// TestBuildKernelCmdlineEmitsNetPort confirms the agent sees
// daft.net_port on the cmdline when NetPort is set.
func TestBuildKernelCmdlineEmitsNetPort(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		NetPort:    9100,
	}
	got := buildKernelCmdline(cfg)
	if !strings.Contains(got, "daft.net_port=9100") {
		t.Errorf("cmdline %q missing net_port", got)
	}
}

// TestBuildKernelCmdlineSkipsNetPortWhenZero confirms a VsockConfig
// with NetPort=0 leaves the cmdline clean.
func TestBuildKernelCmdlineSkipsNetPortWhenZero(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		NetPort:    0,
	}
	got := buildKernelCmdline(cfg)
	if strings.Contains(got, "daft.net_port") {
		t.Errorf("cmdline %q has net_port despite zero", got)
	}
}

// TestBuildKernelCmdlineSkipsVsockTracePortWhenZero confirms a
// VsockConfig with TracePort=0 (channel exists but no trace
// shipping wanted) doesn't pollute the cmdline.
func TestBuildKernelCmdlineSkipsVsockTracePortWhenZero(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		TracePort:  0,
	}
	got := buildKernelCmdline(cfg)
	if strings.Contains(got, "daft.vsock.trace_port") {
		t.Errorf("cmdline %q has trace_port despite zero", got)
	}
}

// TestVsockConfigValidateRequiresAbsolutePath catches the
// most common host-config typo: relative socket path.
func TestVsockConfigValidateRequiresAbsolutePath(t *testing.T) {
	v := &VsockConfig{HostSocket: "relative.sock", GuestCID: 3}
	err := v.validate()
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want mention of absolute", err)
	}
}

// TestVsockConfigValidateRejectsReservedCIDs rejects the three
// reserved AF_VSOCK CIDs (0=reserved, 1=local/hypervisor, 2=host)
// per the kernel ABI.
func TestVsockConfigValidateRejectsReservedCIDs(t *testing.T) {
	for _, cid := range []uint32{0, 1, 2} {
		v := &VsockConfig{HostSocket: testHostSocketPath, GuestCID: cid}
		if err := v.validate(); err == nil {
			t.Errorf("CID=%d accepted; want reserved-CID rejection", cid)
		}
	}
}

// TestVsockConfigValidateRequiresHostSocket — empty string is the
// "not configured" sentinel and should fail loudly.
func TestVsockConfigValidateRequiresHostSocket(t *testing.T) {
	v := &VsockConfig{GuestCID: 3}
	if err := v.validate(); err == nil {
		t.Fatal("expected error for empty HostSocket")
	}
}

// TestStartVsockTraceReceiverNilWhenDisabled returns a no-op stop
// when Vsock is nil or TracePort is zero. Caller can defer stop
// unconditionally.
//
//nolint:dupl // structurally parallel to NetPort by design.
func TestStartVsockTraceReceiverNilWhenDisabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil vsock", validCfg()},
		{
			name: "zero trace port",
			cfg: Config{
				CloudHypervisor: "/c",
				Kernel:          "/k",
				Initramfs:       "/i",
				Vsock: &VsockConfig{
					HostSocket: testHostSocketPath,
					GuestCID:   3,
					TracePort:  0,
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stop, err := startVsockTraceReceiver(context.Background(), tc.cfg)
			if err != nil {
				t.Fatalf("startVsockTraceReceiver: %v", err)
			}
			stop() // must not panic
		})
	}
}

// TestStartVsockTraceReceiverRoundTrip exercises both sides of
// the framing protocol against a real UDS — the production
// StreamExporter feeds the production receiver, no VM involved.
func TestStartVsockTraceReceiverRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		TracePort:  8000,
	}

	hostTracer := trace.New("test-host")
	ctx := trace.ContextWithTracer(context.Background(), hostTracer)
	stop, err := startVsockTraceReceiver(ctx, cfg)
	if err != nil {
		t.Fatalf("startVsockTraceReceiver: %v", err)
	}
	t.Cleanup(stop)

	// Use the production exporter so the test exercises both sides
	// of the framing protocol together.
	addr := cfg.Vsock.HostSocket + "_8000"
	exporter := trace.NewStreamExporter(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", addr)
	})
	traceID := strings.Repeat("a", 32)
	if err := exporter.Export(context.Background(), []*tracev1.Span{
		{
			TraceId:   traceID,
			SpanId:    strings.Repeat("b", 16),
			Name:      "agent.boot",
			Component: "daft-agent",
		},
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := hostTracer.Snapshot()
		if len(snap) == 1 && snap[0].TraceID() == traceID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("span never landed in host tracer; snapshot=%v", hostTracer.Snapshot())
}

// TestStartVsockTraceReceiverWithoutTracerInContext exercises the
// "no Tracer in ctx" path — the listener still binds (so the
// agent's Flush doesn't dial-error) but spans are silently dropped.
func TestStartVsockTraceReceiverWithoutTracerInContext(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		TracePort:  8000,
	}
	stop, err := startVsockTraceReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockTraceReceiver: %v", err)
	}
	t.Cleanup(stop)

	conn, err := net.Dial("unix", cfg.Vsock.HostSocket+"_8000")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = conn.Close()
}

// TestStartVsockTraceReceiverPreCleansStaleSocket simulates a
// previous run crashing while a stale socket file remained — the
// new listener should remove it and bind successfully.
//
//nolint:dupl // structurally parallel to NetPort by design.
func TestStartVsockTraceReceiverPreCleansStaleSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		TracePort:  8000,
	}

	stalePath := cfg.Vsock.HostSocket + "_8000"
	stale, err := net.Listen("unix", stalePath)
	if err != nil {
		t.Fatalf("preseed listener: %v", err)
	}
	_ = stale.Close() // leaves the socket file behind

	stop, err := startVsockTraceReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockTraceReceiver against stale socket: %v", err)
	}
	stop()
}

// TestStartVsockTraceReceiverSurfacesListenError exercises the
// path where the listen syscall itself fails (e.g. parent dir
// missing). The stop func returned in that case must be safe to
// call.
func TestStartVsockTraceReceiverSurfacesListenError(t *testing.T) {
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: "/no/such/directory/v.sock",
		GuestCID:   3,
		TracePort:  8000,
	}
	stop, err := startVsockTraceReceiver(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from Listen on bad path")
	}
	stop() // must not panic
}
