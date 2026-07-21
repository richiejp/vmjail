package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	secretdeliveryv1 "github.com/richiejp/darkaifactory/internal/genpb/secretdelivery/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/secretdelivery/v1/secretdeliveryv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const MaxSecretDeliveryTTL = 5 * time.Minute

type agentSecretDeliverer interface {
	DeliverNodeSecret(context.Context, identity.Fingerprint, NodeSecretRequest) error
}

type SecretDeliveryHandler struct {
	secretdeliveryv1connect.UnimplementedSecretDeliveryServiceHandler
	runs      *agentruncatalog.Store
	nodes     *nodectrl.Registry
	deliverer agentSecretDeliverer
	starter   agentJobStarter
	now       func() time.Time
	agents    *AgentRunHandler
}

func NewSecretDeliveryHandler(runs *agentruncatalog.Store, nodes *nodectrl.Registry, deliverer agentSecretDeliverer, starter agentJobStarter, agents *AgentRunHandler) *SecretDeliveryHandler {
	return &SecretDeliveryHandler{runs: runs, nodes: nodes, deliverer: deliverer, starter: starter, now: time.Now, agents: agents}
}

func (h *SecretDeliveryHandler) Deliver(ctx context.Context, req *connect.Request[secretdeliveryv1.DeliverSecretRequest]) (*connect.Response[secretdeliveryv1.DeliverSecretResponse], error) {
	if h.runs == nil || h.nodes == nil || h.deliverer == nil || h.starter == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: secret delivery is not configured"))
	}
	payload := req.Msg.Payload
	defer clear(payload)
	ownerValue := req.Header().Get(peerFingerprintHeader)
	owner, err := identity.ParseFingerprint(ownerValue)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("orchestrator: no mTLS peer identity"))
	}
	run, err := h.runs.Get(owner, req.Msg.GetAgentRunUuid())
	if err != nil {
		return nil, agentRunError(err)
	}
	if run.State == agentruncatalog.StateStarting || run.State == agentruncatalog.StateRunning {
		if run.CredentialDeliveryID == req.Msg.GetDeliveryId() && run.CredentialDeliveryID != "" {
			return connect.NewResponse(deliveryResponse(run.UUID, run.CredentialDeliveryID)), nil
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: AgentRun credential was already delivered"))
	}
	if run.State != agentruncatalog.StateAwaitingCredential {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("orchestrator: AgentRun is not awaiting a credential"))
	}
	if err := h.validate(req.Msg, run); err != nil {
		return nil, err
	}
	node, ok := h.nodes.Lookup(run.NodeFingerprint)
	if !ok || node.InstanceID != run.NodeInstanceID {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("orchestrator: bound Node process is unavailable"))
	}
	expires := req.Msg.GetExpiresAt().AsTime()
	if err := h.deliverer.DeliverNodeSecret(ctx, run.NodeFingerprint, NodeSecretRequest{SandboxUID: run.SandboxUID, DeliveryID: req.Msg.GetDeliveryId(), Provider: run.CredentialProvider, ExpiresAt: expires, Payload: payload}); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	run, err = h.runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateAwaitingCredential}, To: agentruncatalog.StateStarting, Message: "credential installed", CredentialDeliveryID: req.Msg.GetDeliveryId()})
	if err != nil {
		return nil, agentRunError(err)
	}
	if h.agents != nil {
		h.agents.clearCredentialWait(run.UUID)
	}
	command := append([]string(nil), run.Command...)
	if run.Prompt != "" {
		command = append(command, run.Prompt)
	}
	job, startErr := h.starter.StartNodeJob(ctx, run.NodeFingerprint, nodejob.StartRequest{IdempotencyKey: run.UUID, SandboxUID: run.SandboxUID, Command: command})
	if startErr == nil {
		_, err = recordJobRunning(h.runs, owner, run.UUID, job.ID, "job started")
		if err != nil {
			return nil, agentRunError(err)
		}
	}
	return connect.NewResponse(deliveryResponse(run.UUID, req.Msg.GetDeliveryId())), nil
}

func (h *SecretDeliveryHandler) validate(req *secretdeliveryv1.DeliverSecretRequest, run agentruncatalog.Run) error {
	if req.GetDeliveryId() == "" || len(req.GetDeliveryId()) > 128 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: invalid delivery ID"))
	}
	if req.GetKind() != secretdeliveryv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON || run.CredentialProvider != agentruncatalog.CredentialProviderCodex {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: secret kind does not match AgentRun provider"))
	}
	if len(req.GetPayload()) == 0 || len(req.GetPayload()) > agent.MaxSecretPayload || !json.Valid(req.GetPayload()) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: invalid Codex auth payload"))
	}
	if req.GetExpiresAt() == nil || !req.GetExpiresAt().IsValid() {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: invalid secret expiry"))
	}
	now, expires := h.now(), req.GetExpiresAt().AsTime()
	if !expires.After(now) || expires.After(now.Add(MaxSecretDeliveryTTL)) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: secret expiry is outside the allowed window"))
	}
	return nil
}

func deliveryResponse(run, delivery string) *secretdeliveryv1.DeliverSecretResponse {
	return &secretdeliveryv1.DeliverSecretResponse{AgentRunUuid: run, DeliveryId: delivery, Installed: true}
}
