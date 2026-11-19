package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
)

// DefaultConfigVsockPort is the AF_VSOCK port the host's per-VM config
// server listens on and the agent dials, right after printing its
// banner, to fetch this VM's operational settings (shares, bundle, …).
// A fixed protocol constant — the agent must know where to dial before
// it has any config, and per-VM identity is implicit in the hybrid-vsock
// socket the connection lands on (one UDS per Sandbox), so nothing about
// the port needs to vary. Chosen below the trace (8000), attach (9000),
// and port-forward base (10000) ports since config is fetched first.
const DefaultConfigVsockPort uint32 = 7000

// DefaultTraceVsockPort is the fixed AF_VSOCK port the agent ships its
// span buffer to (and the host's trace sink listens on). A protocol
// constant like the config/attach/net ports — the GuestConfig's
// trace_enabled bit decides whether to ship, never which port. Sits above
// config (7000) and below attach (9000).
const DefaultTraceVsockPort uint32 = 8000

// ConfigReportPrefix tags console lines about the config fetch — dial
// failures, handshake/read errors. Same separator convention as
// ShareReportPrefix / ContainerReportPrefix so the host scanner's regex
// doesn't have to special-case this stream.
const ConfigReportPrefix = "config: "

// configHandshakeVersion is the agent_version the agent stamps on its
// GuestConfigRequest handshake. The host ignores it today; it exists so
// the server can branch on schema as the protocol evolves.
const configHandshakeVersion = "1"

// configFetchTimeout bounds the whole dial + handshake + response read.
// VMs boot fast and the host server is always listening, so a healthy
// fetch completes in milliseconds; a longer wait means the channel is
// wedged or absent (no vsock device, AF_VSOCK seccomp-denied), in which
// case the agent must stop waiting and degrade rather than hang boot.
// Variable rather than constant so tests can shrink it.
//
//nolint:gochecknoglobals // test-hook timeout; mutated only in tests.
var configFetchTimeout = 2 * time.Second

// ControlChannel is the agent's open control connection to the host: the
// GuestConfig fetched at handshake plus a writer for status events. The
// agent holds it for the boot's duration and Closes it on shutdown,
// which the host reads as EOF. Status events (boot-complete, …) are sent
// on this same connection — VM status belongs on the protocol, not the
// serial console banner.
type ControlChannel struct {
	conn net.Conn
	cfg  *agentv1.GuestConfig
}

// OpenControlChannel dials the host config port via dial, sends a
// GuestConfigRequest handshake, reads back the GuestConfig response, and
// returns the channel with the connection still open for later event
// sends. The handshake + config read are bounded by configFetchTimeout;
// the deadline is then cleared because events are sent much later (after
// the boot sequence). Any failure (dial, write, read) returns an error —
// the caller degrades to a banner-only boot rather than blocking.
func OpenControlChannel(
	ctx context.Context,
	dial func(ctx context.Context, port uint32) (net.Conn, error),
) (*ControlChannel, error) {
	hsCtx, cancel := context.WithTimeout(ctx, configFetchTimeout)
	defer cancel()

	conn, err := dial(hsCtx, DefaultConfigVsockPort)
	if err != nil {
		return nil, fmt.Errorf("agent: dial config port: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if deadline, has := hsCtx.Deadline(); has {
		_ = conn.SetDeadline(deadline)
	}

	req := &agentv1.GuestConfigRequest{AgentVersion: configHandshakeVersion}
	if err := WriteGuestConfigRequest(conn, req); err != nil {
		return nil, fmt.Errorf("agent: send config handshake: %w", err)
	}
	cfg, err := ReadGuestConfig(conn)
	if err != nil {
		return nil, fmt.Errorf("agent: read config: %w", err)
	}
	// Config is in hand; clear the handshake deadline so a later event
	// send isn't tripped by the elapsed boot time.
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return &ControlChannel{conn: conn, cfg: cfg}, nil
}

// Config returns the GuestConfig delivered at handshake. Nil-safe.
func (c *ControlChannel) Config() *agentv1.GuestConfig {
	if c == nil {
		return nil
	}
	return c.cfg
}

// SendEvent writes one status event frame to the host.
func (c *ControlChannel) SendEvent(kind agentv1.AgentEvent_Kind) error {
	return WriteAgentEvent(c.conn, &agentv1.AgentEvent{Kind: kind})
}

// Close closes the underlying connection; the host sees EOF.
func (c *ControlChannel) Close() error {
	return c.conn.Close()
}

// WriteGuestConfigRequest marshals + length-prefixes the handshake onto
// w, reusing the attach channel's byte framing (4-byte BE length prefix
// — see writeAttachLenPrefixed).
func WriteGuestConfigRequest(w io.Writer, req *agentv1.GuestConfigRequest) error {
	payload, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("agent: marshal GuestConfigRequest: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

// ReadGuestConfigRequest reads one length-prefixed GuestConfigRequest
// off r. Used by the host's config server to consume the handshake
// before replying.
func ReadGuestConfigRequest(r io.Reader) (*agentv1.GuestConfigRequest, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var req agentv1.GuestConfigRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("agent: unmarshal GuestConfigRequest: %w", err)
	}
	return &req, nil
}

// WriteGuestConfig marshals + length-prefixes the config onto w. Used by
// the host's config server to reply to a handshake.
func WriteGuestConfig(w io.Writer, cfg *agentv1.GuestConfig) error {
	payload, err := proto.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("agent: marshal GuestConfig: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

// ReadGuestConfig reads one length-prefixed GuestConfig off r. An empty
// payload (a config with no fields set) is legal and decodes to a
// zero-valued GuestConfig — the share-only / bare-boot case.
func ReadGuestConfig(r io.Reader) (*agentv1.GuestConfig, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var cfg agentv1.GuestConfig
	if err := proto.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("agent: unmarshal GuestConfig: %w", err)
	}
	return &cfg, nil
}

// WriteAgentEvent marshals + length-prefixes an event onto w (the
// agent's side of the control channel).
func WriteAgentEvent(w io.Writer, ev *agentv1.AgentEvent) error {
	payload, err := proto.Marshal(ev)
	if err != nil {
		return fmt.Errorf("agent: marshal AgentEvent: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

// ReadAgentEvent reads one length-prefixed AgentEvent off r (the host's
// side of the control channel). Returns the framing error (io.EOF when
// the agent closed the connection) so the host's read loop can stop.
func ReadAgentEvent(r io.Reader) (*agentv1.AgentEvent, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var ev agentv1.AgentEvent
	if err := proto.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("agent: unmarshal AgentEvent: %w", err)
	}
	return &ev, nil
}

// configShareSpecs converts the shares carried in a GuestConfig to the
// agent's ShareSpec form. Returns nil when cfg is nil or carries no
// shares (a bare boot, or the vsock-unreachable degrade).
func configShareSpecs(cfg *agentv1.GuestConfig) []ShareSpec {
	protoShares := cfg.GetShares()
	if len(protoShares) == 0 {
		return nil
	}
	specs := make([]ShareSpec, 0, len(protoShares))
	for _, s := range protoShares {
		specs = append(specs, ShareSpec{Tag: s.GetTag(), Mountpoint: s.GetMountpoint(), ReadOnly: s.GetReadOnly()})
	}
	return specs
}

// configOverlays returns the mountpoints of the GuestConfig shares whose
// overlay bit is set, in share order. Consumed by the agent's applyShares
// to redirect those shares through a tmpfs overlay. Returns nil when no
// share requests an overlay.
func configOverlays(cfg *agentv1.GuestConfig) []string {
	var mps []string
	for _, s := range cfg.GetShares() {
		if s.GetOverlay() {
			mps = append(mps, s.GetMountpoint())
		}
	}
	return mps
}

// resolveObservePath returns the effective Tier-2 gadget output path from
// the GuestConfig's Observe block (sel extracts the field), or ("", false)
// when the gadget was not requested. cfg may be nil (GetObserve is
// nil-safe). The host is now the sole source of these paths — it ships
// only absolute, validated paths over the control channel — so there is
// no parse/malformed case left for the gadget starters to handle.
func resolveObservePath(
	cfg *agentv1.GuestConfig,
	sel func(*agentv1.GuestConfig_Observe) string,
) (path string, ok bool) {
	if obs := cfg.GetObserve(); obs != nil {
		if p := sel(obs); p != "" {
			return p, true
		}
	}
	return "", false
}

// configPortForwards converts the port-forward table carried in a
// GuestConfig to the agent's PortForwardSpec form. Returns nil when cfg
// is nil or carries none (no port-forwards requested for this VM).
func configPortForwards(cfg *agentv1.GuestConfig) []PortForwardSpec {
	protoPFs := cfg.GetPortForwards()
	if len(protoPFs) == 0 {
		return nil
	}
	specs := make([]PortForwardSpec, 0, len(protoPFs))
	for _, pf := range protoPFs {
		specs = append(specs, PortForwardSpec{
			VsockPort:     pf.GetVsockPort(),
			ContainerPort: pf.GetContainerPort(),
		})
	}
	return specs
}
