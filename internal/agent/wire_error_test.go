package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

// TestControlWireMarshalErrors covers the marshal-error branch of the
// control-channel serializers. With the cmdline gone, this wire is the
// sole path that carries a VM's settings, so its failure modes are worth
// pinning. proto.Marshal rejects a string field holding invalid UTF-8 —
// the one reliably-reachable way to make a well-formed message fail to
// encode — which exercises the `return fmt.Errorf("marshal …")` arm.
func TestControlWireMarshalErrors(t *testing.T) {
	const badUTF8 = "\xff\xfe"

	if err := WriteGuestConfig(&bytes.Buffer{}, &agentv1.GuestConfig{Bundle: badUTF8}); err == nil {
		t.Error("WriteGuestConfig: want marshal error for invalid-UTF-8 bundle")
	}
	if err := WriteGuestConfigRequest(&bytes.Buffer{}, &agentv1.GuestConfigRequest{AgentVersion: badUTF8}); err == nil {
		t.Error("WriteGuestConfigRequest: want marshal error for invalid-UTF-8 version")
	}
}

// TestAttachWireMarshalErrors covers the marshal-error arm of the attach
// Hello serializer, which shares the control channel's framing. The Hello
// carries a repeated string field (Command), so invalid UTF-8 makes
// proto.Marshal reject it.
func TestAttachWireMarshalErrors(t *testing.T) {
	const badUTF8 = "\xff\xfe"

	if err := WriteAttachHello(&bytes.Buffer{}, &agentv1.AgentAttachHello{Command: []string{badUTF8}}); err == nil {
		t.Error("WriteAttachHello: want marshal error for invalid-UTF-8 command")
	}
}

// TestControlWireUnmarshalErrors covers the unmarshal-error branch of the
// host- and agent-side readers: a frame whose length prefix is valid but
// whose payload is not a decodable message. 0xff bytes form an invalid
// protobuf field tag, so each Unmarshal returns an error.
func TestControlWireUnmarshalErrors(t *testing.T) {
	garbage := []byte{0xff, 0xff, 0xff}

	frame := func() *bytes.Buffer {
		var b bytes.Buffer
		if err := writeAttachLenPrefixed(&b, garbage); err != nil {
			t.Fatalf("frame setup: %v", err)
		}
		return &b
	}

	if _, err := ReadGuestConfigRequest(frame()); err == nil {
		t.Error("ReadGuestConfigRequest: want unmarshal error for garbage payload")
	}
	if _, err := ReadAgentEvent(frame()); err == nil {
		t.Error("ReadAgentEvent: want unmarshal error for garbage payload")
	}
}

// TestControlWireFramingErrors covers the framing-error branch (the
// `readAttachPayload` failure arm) of each reader: an empty reader yields
// io.EOF before any payload, which must propagate as an error rather than
// a zero-valued message.
func TestControlWireFramingErrors(t *testing.T) {
	if _, err := ReadGuestConfigRequest(bytes.NewReader(nil)); err == nil {
		t.Error("ReadGuestConfigRequest: want framing error on empty reader")
	}
	if _, err := ReadGuestConfig(bytes.NewReader(nil)); err == nil {
		t.Error("ReadGuestConfig: want framing error on empty reader")
	}
	if _, err := ReadAgentEvent(bytes.NewReader(nil)); err == nil {
		t.Error("ReadAgentEvent: want framing error on empty reader")
	}
}

// TestWriteContainerResolvConfErrors covers both failure arms of the
// resolv.conf writer the net bridge calls when a container is present:
// the mkdir failure (a path component is a regular file) and the atomic
// write failure (the temp target is a directory, so os.WriteFile can't
// create it). Both are reported on the console and don't stop the boot.
func TestWriteContainerResolvConfErrors(t *testing.T) {
	// mkdir failure: bundle is a regular file, so <bundle>/rootfs/etc
	// can't be created.
	notADir := filepath.Join(t.TempDir(), "bundle-is-a-file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	WriteContainerResolvConf(&buf, notADir)
	if !strings.Contains(buf.String(), NetReportPrefix+"resolv.conf mkdir:") {
		t.Errorf("missing mkdir error report: %q", buf.String())
	}

	// write failure: the etc dir exists, but the atomic temp target is a
	// pre-existing directory, so os.WriteFile fails.
	bundle := t.TempDir()
	etc := filepath.Join(bundle, "rootfs", "etc")
	if err := os.MkdirAll(filepath.Join(etc, "resolv.conf.tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	WriteContainerResolvConf(&buf, bundle)
	if !strings.Contains(buf.String(), NetReportPrefix+"resolv.conf write:") {
		t.Errorf("missing write error report: %q", buf.String())
	}
}

// TestWriteFileAtomicWriteError pins the write-error arm of writeFileAtomic
// directly: writing into a directory that doesn't exist fails at the
// os.WriteFile step (before the rename).
func TestWriteFileAtomicWriteError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir", "f")
	if err := writeFileAtomic(missing, []byte("x"), 0o644); err == nil {
		t.Error("writeFileAtomic: want error writing into a nonexistent dir")
	}
}
