package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/agent"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
)

const DefaultSecretDeliveryTimeout = 10 * time.Second

var ErrUnknownSecretDelivery = errors.New("orchestrator: unknown secret delivery request")

type NodeSecretRequest struct {
	SandboxUID string
	DeliveryID string
	Provider   string
	ExpiresAt  time.Time
	Payload    []byte
}

type pendingSecretDelivery struct {
	node    identity.Fingerprint
	arrived chan secretDeliveryArrival
}

type secretDeliveryArrival struct {
	response *nodectrlv1.SecretDeliveryResponse
	err      error
}

func (h *NodeControlHandler) DeliverNodeSecret(ctx context.Context, fp identity.Fingerprint, req NodeSecretRequest) error {
	if req.SandboxUID == "" || req.DeliveryID == "" || len(req.DeliveryID) > 128 || req.Provider != "codex" || len(req.Payload) == 0 || len(req.Payload) > agent.MaxSecretPayload || !req.ExpiresAt.After(time.Now()) {
		return errors.New("orchestrator: invalid secret delivery request")
	}
	requestID, err := mintSessionID()
	if err != nil {
		return fmt.Errorf("orchestrator: mint secret delivery ID: %w", err)
	}
	pending := &pendingSecretDelivery{node: fp, arrived: make(chan secretDeliveryArrival, 1)}
	h.mu.Lock()
	h.pendingSecrets[requestID] = pending
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.pendingSecrets[requestID] == pending {
			delete(h.pendingSecrets, requestID)
		}
		h.mu.Unlock()
	}()
	payload := append([]byte(nil), req.Payload...)
	eventReq := &nodectrlv1.SecretDeliveryRequest{RequestId: requestID, SandboxUid: req.SandboxUID, DeliveryId: req.DeliveryID, Provider: req.Provider, ExpiresAt: timestamppb.New(req.ExpiresAt), Payload: payload}
	if err := h.pushSecretDelivery(fp, eventReq); err != nil {
		clear(payload)
		return err
	}
	waitCtx, cancel := waitContextWithFallback(ctx, DefaultSecretDeliveryTimeout)
	defer cancel()
	select {
	case arrival := <-pending.arrived:
		if arrival.err != nil {
			return arrival.err
		}
		resp := arrival.response
		if resp.GetDeliveryId() != req.DeliveryID {
			return errors.New("orchestrator: secret response delivery ID mismatch")
		}
		if !resp.GetInstalled() {
			return fmt.Errorf("orchestrator: Node rejected secret delivery: %s", resp.GetError())
		}
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("orchestrator: wait for secret delivery response: %w", waitCtx.Err())
	}
}

func (h *NodeControlHandler) pushSecretDelivery(fp identity.Fingerprint, req *nodectrlv1.SecretDeliveryRequest) error {
	h.mu.Lock()
	registration, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}
	select {
	case registration.events <- &nodectrlv1.NodeEvent{Event: &nodectrlv1.NodeEvent_SecretDelivery{SecretDelivery: req}}:
		return nil
	default:
		return ErrSandboxEventBacklog
	}
}

func (h *NodeControlHandler) ReportSecretDelivery(_ context.Context, req *connect.Request[nodectrlv1.SecretDeliveryResponse]) (*connect.Response[nodectrlv1.SecretDeliveryAck], error) {
	fp, err := h.requireNode(req.Header())
	if err != nil {
		return nil, err
	}
	resp := req.Msg
	if resp.GetRequestId() == "" || resp.GetDeliveryId() == "" || len(resp.GetError()) > 1024 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: invalid secret delivery response"))
	}
	h.mu.Lock()
	pending := h.pendingSecrets[resp.GetRequestId()]
	if pending == nil {
		h.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, ErrUnknownSecretDelivery)
	}
	if pending.node != fp {
		h.mu.Unlock()
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("orchestrator: secret response came from wrong Node"))
	}
	delete(h.pendingSecrets, resp.GetRequestId())
	h.mu.Unlock()
	pending.arrived <- secretDeliveryArrival{response: resp}
	return connect.NewResponse(&nodectrlv1.SecretDeliveryAck{}), nil
}

func (h *NodeControlHandler) failPendingSecretsLocked(fp identity.Fingerprint, err error) {
	for id, pending := range h.pendingSecrets {
		if pending.node == fp {
			delete(h.pendingSecrets, id)
			pending.arrived <- secretDeliveryArrival{err: err}
		}
	}
}
