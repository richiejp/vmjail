package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

// TestGuestConfigFramingRoundTrip covers the marshal/length-prefix/
// unmarshal path for both the request and the response over a buffer,
// including the empty (zero-valued) config which marshals to no bytes.
func TestGuestConfigFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := &agentv1.GuestConfigRequest{AgentVersion: "1"}
	if err := WriteGuestConfigRequest(&buf, req); err != nil {
		t.Fatalf("WriteGuestConfigRequest: %v", err)
	}
	gotReq, err := ReadGuestConfigRequest(&buf)
	if err != nil {
		t.Fatalf("ReadGuestConfigRequest: %v", err)
	}
	if gotReq.GetAgentVersion() != "1" {
		t.Errorf("agent_version = %q, want 1", gotReq.GetAgentVersion())
	}

	cfg := &agentv1.GuestConfig{
		Bundle: testBundlePath,
		Shares: []*agentv1.GuestConfig_Share{{Tag: testTag, Mountpoint: testMountpoint}},
	}
	buf.Reset()
	if err := WriteGuestConfig(&buf, cfg); err != nil {
		t.Fatalf("WriteGuestConfig: %v", err)
	}
	got, err := ReadGuestConfig(&buf)
	if err != nil {
		t.Fatalf("ReadGuestConfig: %v", err)
	}
	if got.GetBundle() != testBundlePath || len(got.GetShares()) != 1 {
		t.Errorf("round-trip = %v, want bundle + 1 share", got)
	}

	// Empty config: zero bytes on the wire, decodes back to zero value.
	buf.Reset()
	if err := WriteGuestConfig(&buf, &agentv1.GuestConfig{}); err != nil {
		t.Fatalf("WriteGuestConfig empty: %v", err)
	}
	empty, err := ReadGuestConfig(&buf)
	if err != nil {
		t.Fatalf("ReadGuestConfig empty: %v", err)
	}
	if empty.GetBundle() != "" || len(empty.GetShares()) != 0 {
		t.Errorf("empty round-trip = %v, want zero value", empty)
	}
}

// TestOpenControlChannelRoundTripAndEvent drives the full control
// channel against an in-process server over net.Pipe: dial → handshake →
// config response → boot-complete event read back by the host side.
func TestOpenControlChannelRoundTripAndEvent(t *testing.T) {
	client, server := net.Pipe()
	want := &agentv1.GuestConfig{
		Bundle: testBundlePath,
		Shares: []*agentv1.GuestConfig_Share{{Tag: testTag, Mountpoint: testMountpoint}},
	}
	srvErr := make(chan error, 1)
	gotEvent := make(chan *agentv1.AgentEvent, 1)
	go func() {
		defer func() { _ = server.Close() }()
		if _, err := ReadGuestConfigRequest(server); err != nil {
			srvErr <- err
			return
		}
		if err := WriteGuestConfig(server, want); err != nil {
			srvErr <- err
			return
		}
		ev, err := ReadAgentEvent(server)
		if err != nil {
			srvErr <- err
			return
		}
		gotEvent <- ev
		srvErr <- nil
	}()

	var dialedPort uint32
	dial := func(_ context.Context, port uint32) (net.Conn, error) {
		dialedPort = port
		return client, nil
	}
	cc, err := OpenControlChannel(context.Background(), dial)
	if err != nil {
		t.Fatalf("OpenControlChannel: %v", err)
	}
	if dialedPort != DefaultConfigVsockPort {
		t.Errorf("dialed port %d, want %d", dialedPort, DefaultConfigVsockPort)
	}
	if cc.Config().GetBundle() != testBundlePath || len(cc.Config().GetShares()) != 1 {
		t.Errorf("config = %v, want bundle + 1 share", cc.Config())
	}
	if err := cc.SendEvent(agentv1.AgentEvent_BOOT_COMPLETE); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	if ev := <-gotEvent; ev.GetKind() != agentv1.AgentEvent_BOOT_COMPLETE {
		t.Errorf("event kind = %v, want BOOT_COMPLETE", ev.GetKind())
	}
	if err := cc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// TestOpenControlChannelDialError surfaces a dial failure (the no-vsock /
// seccomp-denied case) as an error so the caller degrades.
func TestOpenControlChannelDialError(t *testing.T) {
	dial := func(context.Context, uint32) (net.Conn, error) {
		return nil, errors.New("no vsock device")
	}
	if _, err := OpenControlChannel(context.Background(), dial); err == nil {
		t.Fatal("OpenControlChannel with failing dial returned nil error")
	}
}

// TestOpenControlChannelReadError covers a server that accepts + reads
// the handshake but closes before replying — the read must surface an
// error (and the conn must be closed, not leaked).
func TestOpenControlChannelReadError(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		_, _ = ReadGuestConfigRequest(server)
		_ = server.Close()
	}()
	dial := func(context.Context, uint32) (net.Conn, error) { return client, nil }
	if _, err := OpenControlChannel(context.Background(), dial); err == nil {
		t.Fatal("OpenControlChannel with closed server returned nil error")
	}
}

// TestAgentEventFramingRoundTrip covers the event marshal/frame/unmarshal
// path plus the malformed-read error branch.
func TestAgentEventFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAgentEvent(&buf, &agentv1.AgentEvent{Kind: agentv1.AgentEvent_BOOT_COMPLETE}); err != nil {
		t.Fatalf("WriteAgentEvent: %v", err)
	}
	got, err := ReadAgentEvent(&buf)
	if err != nil {
		t.Fatalf("ReadAgentEvent: %v", err)
	}
	if got.GetKind() != agentv1.AgentEvent_BOOT_COMPLETE {
		t.Errorf("kind = %v, want BOOT_COMPLETE", got.GetKind())
	}
	if _, err := ReadAgentEvent(bytes.NewReader(framedPayload([]byte{0x0a, 0x05, 0x61}))); err == nil {
		t.Error("ReadAgentEvent(malformed) returned nil error")
	}
}

// framedPayload prepends the 4-byte big-endian length prefix to body,
// matching writeAttachLenPrefixed, so the Read* decoders can be fed
// hand-crafted (including malformed) frames.
func framedPayload(body []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body))) //nolint:gosec // G115: hand-built test payloads are a few bytes
	return append(hdr[:], body...)
}

// TestReadGuestConfigErrors covers the two error branches of each
// reader: a truncated stream (length prefix can't be read) and a
// well-framed but undecodable payload (a field 1 string claiming 5
// bytes with only 1 present → proto.Unmarshal fails).
func TestReadGuestConfigErrors(t *testing.T) {
	malformed := framedPayload([]byte{0x0a, 0x05, 0x61}) // string field, len 5, 1 byte

	if _, err := ReadGuestConfigRequest(bytes.NewReader(nil)); err == nil {
		t.Error("ReadGuestConfigRequest(truncated) returned nil error")
	}
	if _, err := ReadGuestConfigRequest(bytes.NewReader(malformed)); err == nil {
		t.Error("ReadGuestConfigRequest(malformed) returned nil error")
	}
	if _, err := ReadGuestConfig(bytes.NewReader(nil)); err == nil {
		t.Error("ReadGuestConfig(truncated) returned nil error")
	}
	if _, err := ReadGuestConfig(bytes.NewReader(malformed)); err == nil {
		t.Error("ReadGuestConfig(malformed) returned nil error")
	}
}

// writeErrConn is a net.Conn whose Write always fails — used to exercise
// the handshake-write error branch without a real transport.
type writeErrConn struct{}

func (writeErrConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (writeErrConn) Write([]byte) (int, error)        { return 0, errors.New("write boom") }
func (writeErrConn) Close() error                     { return nil }
func (writeErrConn) LocalAddr() net.Addr              { return nil }
func (writeErrConn) RemoteAddr() net.Addr             { return nil }
func (writeErrConn) SetDeadline(time.Time) error      { return nil }
func (writeErrConn) SetReadDeadline(time.Time) error  { return nil }
func (writeErrConn) SetWriteDeadline(time.Time) error { return nil }

// TestOpenControlChannelWriteError covers the handshake-send error branch
// (and the not-ok defer-close): a conn whose Write fails must surface an
// error rather than returning a half-open channel.
func TestOpenControlChannelWriteError(t *testing.T) {
	dial := func(context.Context, uint32) (net.Conn, error) { return writeErrConn{}, nil }
	if _, err := OpenControlChannel(context.Background(), dial); err == nil {
		t.Fatal("OpenControlChannel with failing write returned nil error")
	}
}

// TestControlChannelConfigNil covers the nil-receiver guard on Config.
func TestControlChannelConfigNil(t *testing.T) {
	var c *ControlChannel
	if c.Config() != nil {
		t.Error("nil ControlChannel.Config() should be nil")
	}
}

// TestConfigShareSpecs covers the proto→ShareSpec projection and its
// nil / empty fallbacks.
//
//nolint:dupl // structurally parallel to TestConfigPortForwards by design.
func TestConfigShareSpecs(t *testing.T) {
	if got := configShareSpecs(nil); got != nil {
		t.Errorf("configShareSpecs(nil) = %v, want nil", got)
	}
	if got := configShareSpecs(&agentv1.GuestConfig{}); got != nil {
		t.Errorf("configShareSpecs(empty) = %v, want nil", got)
	}
	specs := configShareSpecs(&agentv1.GuestConfig{
		Shares: []*agentv1.GuestConfig_Share{{Tag: "t", Mountpoint: "/m"}},
	})
	if len(specs) != 1 || specs[0].Tag != "t" || specs[0].Mountpoint != "/m" {
		t.Errorf("specs = %v, want one t:/m", specs)
	}
}

// TestConfigOverlays covers the overlay-mountpoint projection: only
// shares with the overlay bit set, in share order, nil otherwise.
func TestConfigOverlays(t *testing.T) {
	if got := configOverlays(nil); got != nil {
		t.Errorf("configOverlays(nil) = %v, want nil", got)
	}
	cfg := &agentv1.GuestConfig{Shares: []*agentv1.GuestConfig_Share{
		{Tag: "a", Mountpoint: "/a"},
		{Tag: "b", Mountpoint: "/b", Overlay: true},
		{Tag: "c", Mountpoint: "/c", Overlay: true},
	}}
	got := configOverlays(cfg)
	if len(got) != 2 || got[0] != "/b" || got[1] != "/c" {
		t.Errorf("configOverlays = %v, want [/b /c]", got)
	}
}

// TestConfigPortForwards covers the port-forward table projection and
// its nil / empty fallbacks.
//
//nolint:dupl // structurally parallel to TestConfigShareSpecs by design.
func TestConfigPortForwards(t *testing.T) {
	if got := configPortForwards(nil); got != nil {
		t.Errorf("configPortForwards(nil) = %v, want nil", got)
	}
	if got := configPortForwards(&agentv1.GuestConfig{}); got != nil {
		t.Errorf("configPortForwards(empty) = %v, want nil", got)
	}
	specs := configPortForwards(&agentv1.GuestConfig{
		PortForwards: []*agentv1.GuestConfig_PortForward{{VsockPort: 10001, ContainerPort: 8080}},
	})
	if len(specs) != 1 || specs[0].VsockPort != 10001 || specs[0].ContainerPort != 8080 {
		t.Errorf("specs = %v, want one 10001->8080", specs)
	}
}

// TestResolveObservePath covers both branches: config hit (the selected
// Observe field carries a path), and the misses (Observe present but this
// field empty, or nil cfg) which return ok=false.
func TestResolveObservePath(t *testing.T) {
	sel := (*agentv1.GuestConfig_Observe).GetOpensnoop

	// Config carries the path.
	cfg := &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Opensnoop: "/cfg.log"}}
	p, ok := resolveObservePath(cfg, sel)
	if !ok || p != "/cfg.log" {
		t.Fatalf("config-hit: got (%q,%v), want (/cfg.log,true)", p, ok)
	}

	// Observe present but this field empty: miss.
	cfg = &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Execsnoop: "/other.log"}}
	p, ok = resolveObservePath(cfg, sel)
	if ok || p != "" {
		t.Fatalf("config-miss: got (%q,%v), want (\"\",false)", p, ok)
	}

	// nil cfg: miss.
	p, ok = resolveObservePath(nil, sel)
	if ok || p != "" {
		t.Fatalf("nil-cfg: got (%q,%v), want (\"\",false)", p, ok)
	}
}
