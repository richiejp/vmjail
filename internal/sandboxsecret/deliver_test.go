package sandboxsecret

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/agentcredential"
	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

func TestDeliverRoundTripAndValidation(t *testing.T) {
	host, guest := net.Pipe()
	d, err := New(func(_ context.Context, sandbox string, port uint32) (net.Conn, error) {
		if sandbox != "sb" || port != agent.DefaultSecretVsockPort {
			t.Fatalf("dial %q:%d", sandbox, port)
		}
		return host, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan *agentv1.SecretInstallRequest, 1)
	go func() {
		req, _ := agent.ReadSecretInstallRequest(guest)
		seen <- req
		_ = agent.WriteSecretInstallResponse(guest, &agentv1.SecretInstallResponse{DeliveryId: req.GetDeliveryId(), Installed: true})
	}()
	payload := []byte(`{"tokens":{"access_token":"marker"}}`)
	if err := d.Deliver(t.Context(), Request{SandboxUID: "sb", DeliveryID: "delivery", Provider: string(agentcredential.ProviderCodex), ExpiresAt: time.Now().Add(time.Minute), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; !bytes.Equal(got.GetPayload(), payload) {
		t.Fatalf("payload = %q", got.GetPayload())
	}
	_ = guest.Close()

	for _, bad := range []Request{{}, {SandboxUID: "sb", DeliveryID: "x", Provider: "other", ExpiresAt: time.Now().Add(time.Minute), Payload: payload}, {SandboxUID: "sb", DeliveryID: "x", Provider: string(agentcredential.ProviderCodex), ExpiresAt: time.Now().Add(-time.Minute), Payload: payload}} {
		if err := d.Deliver(t.Context(), bad); err == nil {
			t.Fatalf("Deliver(%+v) succeeded", bad)
		}
	}
}

func TestDeliverRejectsGuestFailureAndMismatchedID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *agentv1.SecretInstallResponse
		want     string
	}{
		{"rejected", &agentv1.SecretInstallResponse{DeliveryId: "id", Error: "no"}, "rejected"},
		{"mismatch", &agentv1.SecretInstallResponse{DeliveryId: "other", Installed: true}, "mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			d, _ := New(func(context.Context, string, uint32) (net.Conn, error) { return host, nil })
			go func() {
				_, _ = agent.ReadSecretInstallRequest(guest)
				_ = agent.WriteSecretInstallResponse(guest, tc.response)
			}()
			err := d.Deliver(t.Context(), Request{SandboxUID: "sb", DeliveryID: "id", Provider: string(agentcredential.ProviderCodex), ExpiresAt: time.Now().Add(time.Minute), Payload: []byte(`{}`)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			_ = guest.Close()
		})
	}
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
}

func TestDeliverReportsTransportFailures(t *testing.T) {
	request := Request{SandboxUID: "sb", DeliveryID: "id", Provider: string(agentcredential.ProviderCodex), ExpiresAt: time.Now().Add(time.Minute), Payload: []byte(`{}`)}

	dialFailure, _ := New(func(context.Context, string, uint32) (net.Conn, error) {
		return nil, errors.New("dial failed")
	})
	if err := dialFailure.Deliver(t.Context(), request); err == nil || !strings.Contains(err.Error(), "dial guest") {
		t.Fatalf("dial error = %v", err)
	}

	for _, tc := range []struct {
		name string
		peer func(net.Conn)
		want string
	}{
		{name: "write", peer: func(conn net.Conn) { _ = conn.Close() }, want: "write request"},
		{name: "read", peer: func(conn net.Conn) {
			_, _ = agent.ReadSecretInstallRequest(conn)
			_ = conn.Close()
		}, want: "read response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			d, _ := New(func(context.Context, string, uint32) (net.Conn, error) { return host, nil })
			go tc.peer(guest)
			if err := d.Deliver(t.Context(), request); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
