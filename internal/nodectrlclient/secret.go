package nodectrlclient

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/sandboxsecret"
)

const maxSecretDeliveryErrorBytes = 1024

func (c *Client) handleSecretDelivery(ctx context.Context, req *nodectrlv1.SecretDeliveryRequest) error {
	resp := &nodectrlv1.SecretDeliveryResponse{RequestId: req.GetRequestId(), DeliveryId: req.GetDeliveryId()}
	payload := append([]byte(nil), req.GetPayload()...)
	clear(req.Payload)
	defer clear(payload)
	var err error
	if c.cfg.SecretDeliverer == nil {
		err = errors.New("secret delivery is not configured")
	} else {
		expires := time.Time{}
		if req.GetExpiresAt() != nil {
			expires = req.GetExpiresAt().AsTime()
		}
		err = c.cfg.SecretDeliverer.Deliver(ctx, sandboxsecret.Request{SandboxUID: req.GetSandboxUid(), DeliveryID: req.GetDeliveryId(), Provider: req.GetProvider(), ExpiresAt: expires, Payload: payload})
	}
	if err != nil {
		resp.Error = err.Error()
		if len(resp.Error) > maxSecretDeliveryErrorBytes {
			resp.Error = resp.Error[:maxSecretDeliveryErrorBytes]
		}
	} else {
		resp.Installed = true
	}
	_, reportErr := c.ctrl.ReportSecretDelivery(ctx, connect.NewRequest(resp))
	return reportErr
}
