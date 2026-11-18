package nodectrlclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/richiejp/darkaifactory/internal/sandboxsecret"
)

type fakeSecretDeliverer struct {
	req sandboxsecret.Request
	err error
}

func (f *fakeSecretDeliverer) Deliver(_ context.Context, req sandboxsecret.Request) error {
	f.req = req
	return f.err
}

type fakeNodeControlSecret struct {
	nodectrlv1connect.NodeControlServiceClient
	resp     *nodectrlv1.SecretDeliveryResponse
	err      error
	reported chan struct{}
}

type reportingWriter chan string

func (w reportingWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func (f *fakeNodeControlSecret) ReportSecretDelivery(_ context.Context, req *connect.Request[nodectrlv1.SecretDeliveryResponse]) (*connect.Response[nodectrlv1.SecretDeliveryAck], error) {
	f.resp = req.Msg
	err := f.err
	if f.reported != nil {
		close(f.reported)
	}
	return connect.NewResponse(&nodectrlv1.SecretDeliveryAck{}), err
}

func TestDispatchEventHandlesSecretDeliveryAsynchronously(t *testing.T) {
	reported := make(chan struct{})
	ctrl := &fakeNodeControlSecret{reported: reported}
	c := &Client{cfg: Config{SecretDeliverer: &fakeSecretDeliverer{}}, ctrl: ctrl}
	event := &nodectrlv1.NodeEvent{Event: &nodectrlv1.NodeEvent_SecretDelivery{SecretDelivery: &nodectrlv1.SecretDeliveryRequest{RequestId: "r", SandboxUid: "sb", DeliveryId: "d", Provider: "codex", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Payload: []byte(`{}`)}}}
	if err := c.dispatchEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatal("secret delivery was not reported")
	}

	stderr := make(reportingWriter, 1)
	reported = make(chan struct{})
	ctrl.reported, ctrl.err = reported, errors.New("report failed")
	c.cfg.Stderr = stderr
	event.GetSecretDelivery().Payload = []byte(`{}`)
	if err := c.dispatchEvent(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatal("failed secret delivery was not reported")
	}
	select {
	case got := <-stderr:
		if got != "nodectrlclient: secret delivery: report failed\n" {
			t.Fatalf("stderr = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("secret delivery failure was not logged")
	}
}

func TestHandleSecretDeliveryCopiesClearsAndReports(t *testing.T) {
	d := &fakeSecretDeliverer{}
	ctrl := &fakeNodeControlSecret{}
	c := &Client{cfg: Config{SecretDeliverer: d}, ctrl: ctrl}
	req := &nodectrlv1.SecretDeliveryRequest{RequestId: "r", SandboxUid: "sb", DeliveryId: "d", Provider: "codex", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Payload: []byte(`{"token":"secret"}`)}
	if err := c.handleSecretDelivery(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(req.Payload) != len(d.req.Payload) {
		t.Fatalf("payload lengths req=%d delivered=%d", len(req.Payload), len(d.req.Payload))
	}
	for _, b := range req.Payload {
		if b != 0 {
			t.Fatal("wire payload was not cleared")
		}
	}
	if !ctrl.resp.GetInstalled() || ctrl.resp.GetDeliveryId() != "d" {
		t.Fatalf("response=%+v", ctrl.resp)
	}

	d.err = errors.New("reject")
	req.Payload = []byte(`{}`)
	if err := c.handleSecretDelivery(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if ctrl.resp.GetError() != "reject" {
		t.Fatalf("response=%+v", ctrl.resp)
	}
	d.err = errors.New(strings.Repeat("x", maxSecretDeliveryErrorBytes+1))
	req.Payload = []byte(`{}`)
	if err := c.handleSecretDelivery(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(ctrl.resp.GetError()) != maxSecretDeliveryErrorBytes {
		t.Fatalf("bounded error length = %d", len(ctrl.resp.GetError()))
	}
	c.cfg.SecretDeliverer = nil
	req.Payload = []byte(`{}`)
	if err := c.handleSecretDelivery(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if ctrl.resp.GetError() == "" {
		t.Fatal("nil deliverer reported success")
	}
}
