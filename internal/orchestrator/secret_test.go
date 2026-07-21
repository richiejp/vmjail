package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

func TestNodeSecretDeliveryRoundTripAndAuthentication(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{9}
	events := h.attachStream(node)
	done := make(chan error, 1)
	go func() {
		done <- h.DeliverNodeSecret(t.Context(), node, NodeSecretRequest{SandboxUID: "sb", DeliveryID: "delivery", Provider: testAgentProfile, ExpiresAt: time.Now().Add(time.Minute), Payload: []byte(`{"token":"marker"}`)})
	}()
	req := (<-events).GetSecretDelivery()
	if req.GetSandboxUid() != "sb" || req.GetProvider() != testAgentProfile || string(req.GetPayload()) != `{"token":"marker"}` {
		t.Fatalf("request=%+v", req)
	}
	wrong := connect.NewRequest(&nodectrlv1.SecretDeliveryResponse{RequestId: req.GetRequestId(), DeliveryId: "delivery", Installed: true})
	wrong.Header().Set(peerFingerprintHeader, (identity.Fingerprint{8}).String())
	if _, err := h.ReportSecretDelivery(t.Context(), wrong); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("wrong node err=%v", err)
	}
	wrong.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportSecretDelivery(t.Context(), wrong); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReportSecretDelivery(t.Context(), wrong); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("duplicate=%v", err)
	}
}

func TestNodeSecretDeliveryFailures(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{7}
	if err := h.DeliverNodeSecret(t.Context(), node, NodeSecretRequest{}); err == nil {
		t.Fatal("invalid request succeeded")
	}
	valid := NodeSecretRequest{SandboxUID: "sb", DeliveryID: "id", Provider: testAgentProfile, ExpiresAt: time.Now().Add(time.Minute), Payload: []byte(`{}`)}
	if err := h.DeliverNodeSecret(t.Context(), node, valid); !errors.Is(err, ErrNodeNotConnected) {
		t.Fatalf("disconnected=%v", err)
	}
	events := h.attachStream(node)
	done := make(chan error, 1)
	go func() { done <- h.DeliverNodeSecret(t.Context(), node, valid) }()
	req := (<-events).GetSecretDelivery()
	resp := connect.NewRequest(&nodectrlv1.SecretDeliveryResponse{RequestId: req.GetRequestId(), DeliveryId: "id", Error: "guest failed"})
	resp.Header().Set(peerFingerprintHeader, node.String())
	_, _ = h.ReportSecretDelivery(t.Context(), resp)
	if err := <-done; err == nil {
		t.Fatal("guest rejection succeeded")
	}

	events = h.attachStream(node)
	done = make(chan error, 1)
	go func() { done <- h.DeliverNodeSecret(t.Context(), node, valid) }()
	req = (<-events).GetSecretDelivery()
	mismatch := connect.NewRequest(&nodectrlv1.SecretDeliveryResponse{RequestId: req.GetRequestId(), DeliveryId: "other", Installed: true})
	mismatch.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportSecretDelivery(t.Context(), mismatch); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "delivery ID mismatch") {
		t.Fatalf("mismatch error=%v", err)
	}
	// Disconnect releases a correlated waiter instead of leaving it until timeout.
	events = h.attachStream(node)
	done = make(chan error, 1)
	go func() { done <- h.DeliverNodeSecret(t.Context(), node, valid) }()
	<-events
	h.RevokeRegistration(node)
	if err := <-done; !errors.Is(err, ErrNodeNotConnected) {
		t.Fatalf("disconnect error=%v", err)
	}
}

func TestNodeSecretDeliveryRejectsMalformedResponseAndBacklog(t *testing.T) {
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	node := identity.Fingerprint{6}
	invalid := connect.NewRequest(&nodectrlv1.SecretDeliveryResponse{})
	invalid.Header().Set(peerFingerprintHeader, node.String())
	if _, err := h.ReportSecretDelivery(t.Context(), invalid); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid response=%v", err)
	}

	events := h.attachStream(node)
	for i := 0; i < cap(events); i++ {
		events <- &nodectrlv1.NodeEvent{}
	}
	valid := NodeSecretRequest{SandboxUID: "sb", DeliveryID: "id", Provider: testAgentProfile, ExpiresAt: time.Now().Add(time.Minute), Payload: []byte(`{}`)}
	if err := h.DeliverNodeSecret(t.Context(), node, valid); !errors.Is(err, ErrSandboxEventBacklog) {
		t.Fatalf("backlog error=%v", err)
	}
}

func TestClearQueuedSecretEventsZeroesOnlySecretPayloads(t *testing.T) {
	events := make(chan *nodectrlv1.NodeEvent, 2)
	payload := []byte("marker")
	events <- &nodectrlv1.NodeEvent{Event: &nodectrlv1.NodeEvent_SecretDelivery{SecretDelivery: &nodectrlv1.SecretDeliveryRequest{Payload: payload}}}
	events <- &nodectrlv1.NodeEvent{}
	clearQueuedSecretEvents(events)
	for _, b := range payload {
		if b != 0 {
			t.Fatal("queued payload not cleared")
		}
	}
}
