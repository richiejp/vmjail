package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
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

// FetchGuestConfig dials the host config port via dial, sends a
// GuestConfigRequest handshake, and reads back the single GuestConfig
// response. The whole exchange is bounded by configFetchTimeout. Any
// failure (dial, write, read, unmarshal) returns an error — the caller
// degrades to a cmdline/banner-only boot rather than blocking.
func FetchGuestConfig(
	ctx context.Context,
	dial func(ctx context.Context, port uint32) (net.Conn, error),
) (*agentv1.GuestConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, configFetchTimeout)
	defer cancel()

	conn, err := dial(ctx, DefaultConfigVsockPort)
	if err != nil {
		return nil, fmt.Errorf("agent: dial config port: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
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
	return cfg, nil
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

// configShareSpecs converts the shares carried in a GuestConfig to the
// agent's ShareSpec form. Returns nil when cfg is nil or carries no
// shares, which the caller treats as "fall back to the cmdline shares"
// during the slice-1 transition.
func configShareSpecs(cfg *agentv1.GuestConfig) []ShareSpec {
	protoShares := cfg.GetShares()
	if len(protoShares) == 0 {
		return nil
	}
	specs := make([]ShareSpec, 0, len(protoShares))
	for _, s := range protoShares {
		specs = append(specs, ShareSpec{Tag: s.GetTag(), Mountpoint: s.GetMountpoint()})
	}
	return specs
}
