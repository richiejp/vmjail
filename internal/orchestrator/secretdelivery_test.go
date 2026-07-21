package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	secretdeliveryv1 "github.com/richiejp/darkaifactory/internal/genpb/secretdelivery/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const testSecretDeliveryID = "delivery"

type fakeAgentSecretDeliverer struct {
	req NodeSecretRequest
	err error
}

func (f *fakeAgentSecretDeliverer) DeliverNodeSecret(_ context.Context, _ identity.Fingerprint, req NodeSecretRequest) error {
	f.req = req
	return f.err
}

func awaitingCredentialRun(t *testing.T) (runs *agentruncatalog.Store, owner, node identity.Fingerprint, run agentruncatalog.Run) {
	runs, _ = agentRunStore(t)
	owner = identity.Fingerprint{1}
	node = identity.Fingerprint{2}
	var err error
	run, err = runs.Create(agentruncatalog.CreateInput{UUID: "run", WorkspaceUUID: "workspace", ProfileName: "profile", ProfileDigest: "sha256:x", Command: []string{testAgentProfile}, OwnerFingerprint: owner, CredentialRequired: true, CredentialProvider: testAgentProfile})
	if err != nil {
		t.Fatal(err)
	}
	run, err = runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateQueued}, To: agentruncatalog.StateAwaitingCredential, SandboxUID: "sb", NodeFingerprint: node, NodeInstanceID: "boot", Message: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	return runs, owner, node, run
}

func TestSecretDeliveryInstallsPersistsThenStarts(t *testing.T) {
	runs, owner, node, run := awaitingCredentialRun(t)
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, "boot", "n", "v", nodectrl.Resources{})
	d := &fakeAgentSecretDeliverer{}
	starter := &fakeAgentStarter{snapshot: nodejob.Snapshot{ID: orchestratorTestJobID}}
	agents := NewAgentRunHandler(runs, nil)
	h := NewSecretDeliveryHandler(runs, nodes, d, starter, agents)
	req := connect.NewRequest(&secretdeliveryv1.DeliverSecretRequest{AgentRunUuid: run.UUID, DeliveryId: testSecretDeliveryID, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Kind: secretdeliveryv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: []byte(`{"tokens":{"access_token":"marker"}}`)})
	req.Header().Set(peerFingerprintHeader, owner.String())
	resp, err := h.Deliver(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.GetInstalled() || d.req.SandboxUID != "sb" {
		t.Fatalf("resp=%+v delivered=%+v", resp.Msg, d.req)
	}
	got, err := runs.Get(owner, run.UUID)
	if err != nil || got.State != agentruncatalog.StateRunning || got.CredentialDeliveryID != testSecretDeliveryID || got.NodeJobID != orchestratorTestJobID {
		t.Fatalf("run=%+v err=%v", got, err)
	}
	for _, b := range req.Msg.Payload {
		if b != 0 {
			t.Fatal("request payload not zeroed")
		}
	}
	// Same delivery is idempotently acknowledged without a second relay.
	d.req = NodeSecretRequest{}
	req.Msg.Payload = []byte(`{}`)
	if _, err := h.Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if d.req.SandboxUID != "" {
		t.Fatal("idempotent retry redelivered")
	}
}

func TestSecretDeliveryRejectsInvalidAndUnavailable(t *testing.T) {
	runs, owner, node, run := awaitingCredentialRun(t)
	nodes := nodectrl.NewRegistry(nil)
	d := &fakeAgentSecretDeliverer{}
	h := NewSecretDeliveryHandler(runs, nodes, d, &fakeAgentStarter{}, nil)
	base := func() *connect.Request[secretdeliveryv1.DeliverSecretRequest] {
		r := connect.NewRequest(&secretdeliveryv1.DeliverSecretRequest{AgentRunUuid: run.UUID, DeliveryId: "id", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Kind: secretdeliveryv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: []byte(`{}`)})
		r.Header().Set(peerFingerprintHeader, owner.String())
		return r
	}
	if _, err := h.Deliver(t.Context(), base()); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("missing node=%v", err)
	}
	nodes.RegisterInstance(node, "other", "n", "v", nodectrl.Resources{})
	if _, err := h.Deliver(t.Context(), base()); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("wrong instance=%v", err)
	}
	nodes.RegisterInstance(node, "boot", "n", "v", nodectrl.Resources{})
	d.err = errors.New("relay")
	if _, err := h.Deliver(t.Context(), base()); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("relay=%v", err)
	}
	for _, mutate := range []func(*secretdeliveryv1.DeliverSecretRequest){func(r *secretdeliveryv1.DeliverSecretRequest) { r.DeliveryId = "" }, func(r *secretdeliveryv1.DeliverSecretRequest) { r.Payload = []byte("bad") }, func(r *secretdeliveryv1.DeliverSecretRequest) {
		r.ExpiresAt = timestamppb.New(time.Now().Add(10 * time.Minute))
	}, func(r *secretdeliveryv1.DeliverSecretRequest) {
		r.Kind = secretdeliveryv1.SecretKind_SECRET_KIND_UNSPECIFIED
	}} {
		r := base()
		mutate(r.Msg)
		if _, err := h.Deliver(t.Context(), r); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("invalid error=%v", err)
		}
	}
}

func TestSecretDeliveryStateAuthenticationAndAmbiguousStart(t *testing.T) {
	if _, err := NewSecretDeliveryHandler(nil, nil, nil, nil, nil).Deliver(t.Context(), connect.NewRequest(&secretdeliveryv1.DeliverSecretRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unconfigured=%v", err)
	}
	runs, owner, node, run := awaitingCredentialRun(t)
	nodes := nodectrl.NewRegistry(nil)
	nodes.RegisterInstance(node, "boot", "n", "v", nodectrl.Resources{})
	d := &fakeAgentSecretDeliverer{}
	starter := &fakeAgentStarter{err: errors.New("ambiguous")}
	h := NewSecretDeliveryHandler(runs, nodes, d, starter, nil)
	message := &secretdeliveryv1.DeliverSecretRequest{AgentRunUuid: run.UUID, DeliveryId: testSecretDeliveryID, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Kind: secretdeliveryv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: []byte(`{}`)}
	if _, err := h.Deliver(t.Context(), connect.NewRequest(message)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated=%v", err)
	}
	message.Payload = []byte(`{}`)
	req := connect.NewRequest(message)
	req.Header().Set(peerFingerprintHeader, owner.String())
	if _, err := h.Deliver(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	got, _ := runs.Get(owner, run.UUID)
	if got.State != agentruncatalog.StateStarting || got.CredentialDeliveryID != testSecretDeliveryID {
		t.Fatalf("run=%+v", got)
	}
	retry := connect.NewRequest(&secretdeliveryv1.DeliverSecretRequest{AgentRunUuid: run.UUID, DeliveryId: "different"})
	retry.Header().Set(peerFingerprintHeader, owner.String())
	if _, err := h.Deliver(t.Context(), retry); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("different retry=%v", err)
	}
}

func TestSecretDeliveryRejectsRunThatIsNotAwaitingCredential(t *testing.T) {
	runs, owner, _, run := awaitingCredentialRun(t)
	if _, err := runs.Transition(owner, run.UUID, agentruncatalog.TransitionInput{From: []agentruncatalog.State{agentruncatalog.StateAwaitingCredential}, To: agentruncatalog.StateFailed, Message: "failed"}); err != nil {
		t.Fatal(err)
	}
	h := NewSecretDeliveryHandler(runs, nodectrl.NewRegistry(nil), &fakeAgentSecretDeliverer{}, &fakeAgentStarter{}, nil)
	req := connect.NewRequest(&secretdeliveryv1.DeliverSecretRequest{AgentRunUuid: run.UUID})
	req.Header().Set(peerFingerprintHeader, owner.String())
	if _, err := h.Deliver(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("non-awaiting run error=%v", err)
	}
}
