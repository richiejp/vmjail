package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
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

// TestFetchGuestConfigRoundTrip drives the agent's fetch against an
// in-process server over net.Pipe: dial → handshake → response.
func TestFetchGuestConfigRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	want := &agentv1.GuestConfig{
		Bundle: testBundlePath,
		Shares: []*agentv1.GuestConfig_Share{{Tag: testTag, Mountpoint: testMountpoint}},
	}
	srvErr := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		if _, err := ReadGuestConfigRequest(server); err != nil {
			srvErr <- err
			return
		}
		srvErr <- WriteGuestConfig(server, want)
	}()

	var dialedPort uint32
	dial := func(_ context.Context, port uint32) (net.Conn, error) {
		dialedPort = port
		return client, nil
	}
	got, err := FetchGuestConfig(context.Background(), dial)
	if err != nil {
		t.Fatalf("FetchGuestConfig: %v", err)
	}
	if dialedPort != DefaultConfigVsockPort {
		t.Errorf("dialed port %d, want %d", dialedPort, DefaultConfigVsockPort)
	}
	if got.GetBundle() != testBundlePath || len(got.GetShares()) != 1 {
		t.Errorf("config = %v, want bundle + 1 share", got)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// TestFetchGuestConfigDialError surfaces a dial failure (the no-vsock /
// seccomp-denied case) as an error so the caller degrades.
func TestFetchGuestConfigDialError(t *testing.T) {
	dial := func(context.Context, uint32) (net.Conn, error) {
		return nil, errors.New("no vsock device")
	}
	_, err := FetchGuestConfig(context.Background(), dial)
	if err == nil {
		t.Fatal("FetchGuestConfig with failing dial returned nil error")
	}
}

// TestFetchGuestConfigReadError covers a server that accepts + reads the
// handshake but closes before replying — the read must surface an error.
func TestFetchGuestConfigReadError(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		_, _ = ReadGuestConfigRequest(server)
		_ = server.Close()
	}()
	dial := func(context.Context, uint32) (net.Conn, error) { return client, nil }
	if _, err := FetchGuestConfig(context.Background(), dial); err == nil {
		t.Fatal("FetchGuestConfig with closed server returned nil error")
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

// TestConfigShareSpecs covers the proto→ShareSpec projection and its
// nil / empty fallbacks.
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
