// Package sandboxsecret carries a bounded credential from sandboxd to the
// dedicated listener in one live Sandbox VM.
package sandboxsecret

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/agentcredential"
	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

type Dialer func(context.Context, string, uint32) (net.Conn, error)

type Request struct {
	SandboxUID string
	DeliveryID string
	Provider   string
	ExpiresAt  time.Time
	Payload    []byte
}

type Deliverer struct{ dial Dialer }

func New(dial Dialer) (*Deliverer, error) {
	if dial == nil {
		return nil, errors.New("sandboxsecret: nil dialer")
	}
	return &Deliverer{dial: dial}, nil
}

func (d *Deliverer) Deliver(ctx context.Context, req Request) error {
	if req.SandboxUID == "" || req.DeliveryID == "" || req.Provider != string(agentcredential.ProviderCodex) || len(req.Payload) == 0 || len(req.Payload) > agent.MaxSecretPayload || !req.ExpiresAt.After(time.Now()) {
		return errors.New("sandboxsecret: invalid delivery request")
	}
	conn, err := d.dial(ctx, req.SandboxUID, agent.DefaultSecretVsockPort)
	if err != nil {
		return fmt.Errorf("sandboxsecret: dial guest: %w", err)
	}
	defer func() { _ = conn.Close() }()
	payload := append([]byte(nil), req.Payload...)
	defer clear(payload)
	wreq := &agentv1.SecretInstallRequest{DeliveryId: req.DeliveryID, ExpiresAt: timestamppb.New(req.ExpiresAt), Kind: agentv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: payload}
	if err := agent.WriteSecretInstallRequest(conn, wreq); err != nil {
		return fmt.Errorf("sandboxsecret: write request: %w", err)
	}
	clear(wreq.Payload)
	resp, err := agent.ReadSecretInstallResponse(conn)
	if err != nil {
		return fmt.Errorf("sandboxsecret: read response: %w", err)
	}
	if resp.GetDeliveryId() != req.DeliveryID {
		return errors.New("sandboxsecret: response delivery ID mismatch")
	}
	if !resp.GetInstalled() {
		return fmt.Errorf("sandboxsecret: guest rejected delivery: %s", resp.GetError())
	}
	return nil
}
