package nodectrlclient

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

type registrationHandler struct {
	nodectrlv1connect.UnimplementedNodeControlServiceHandler
	register  func(context.Context, *connect.Request[nodectrlv1.RegisterNodeRequest], *connect.ServerStream[nodectrlv1.NodeEvent]) error
	heartbeat func(context.Context, *connect.Request[nodectrlv1.HeartbeatRequest]) (*connect.Response[nodectrlv1.HeartbeatResponse], error)
}

func (h registrationHandler) RegisterNode(ctx context.Context, req *connect.Request[nodectrlv1.RegisterNodeRequest], stream *connect.ServerStream[nodectrlv1.NodeEvent]) error {
	return h.register(ctx, req, stream)
}

func (h registrationHandler) Heartbeat(ctx context.Context, req *connect.Request[nodectrlv1.HeartbeatRequest]) (*connect.Response[nodectrlv1.HeartbeatResponse], error) {
	return h.heartbeat(ctx, req)
}

func TestRegistrationLifecycle(t *testing.T) {
	const (
		cancelMode           = "cancel"
		heartbeatFailureMode = "heartbeat failure"
		closedMode           = "closed"
		streamFailureMode    = "stream failure"
		missingReadyMode     = "missing ready"
		wrongFirstEventMode  = "wrong first event"
		extraEventMode       = "extra event"
	)
	for _, mode := range []string{cancelMode, heartbeatFailureMode, closedMode, streamFailureMode, missingReadyMode, wrongFirstEventMode, extraEventMode} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ready := &nodectrlv1.NodeEvent{Event: &nodectrlv1.NodeEvent_Ready{Ready: &nodectrlv1.SubscriptionReady{ProtocolVersion: localProtocolVersionProto()}}}
			h := registrationHandler{
				register: func(ctx context.Context, req *connect.Request[nodectrlv1.RegisterNodeRequest], stream *connect.ServerStream[nodectrlv1.NodeEvent]) error {
					if req.Msg.Version != "test" || req.Msg.Hostname != "local" || req.Msg.InstanceId != "instance" || req.Msg.Resources.Cpus != 2 {
						t.Error("registration lost host identity or resources")
					}
					switch mode {
					case missingReadyMode:
						return errors.New("registration refused")
					case wrongFirstEventMode:
						return stream.Send(&nodectrlv1.NodeEvent{})
					}
					if err := stream.Send(ready); err != nil {
						return err
					}
					switch mode {
					case closedMode:
						return nil
					case streamFailureMode:
						return errors.New("stream failed")
					case extraEventMode:
						return stream.Send(&nodectrlv1.NodeEvent{})
					}
					<-ctx.Done()
					return ctx.Err()
				},
				heartbeat: func(_ context.Context, req *connect.Request[nodectrlv1.HeartbeatRequest]) (*connect.Response[nodectrlv1.HeartbeatResponse], error) {
					if req.Msg.Current.Cpus != 2 || len(req.Msg.LiveSandboxes) != 1 || len(req.Msg.WorkspaceStorage) != 1 {
						t.Error("heartbeat lost resource snapshots")
					}
					if mode == heartbeatFailureMode {
						return nil, errors.New("heartbeat rejected")
					}
					cancel()
					return connect.NewResponse(&nodectrlv1.HeartbeatResponse{}), nil
				},
			}
			_, handler := nodectrlv1connect.NewNodeControlServiceHandler(h)
			server := httptest.NewServer(handler)
			defer server.Close()
			interval := time.Hour
			if mode == cancelMode || mode == heartbeatFailureMode {
				interval = time.Millisecond
			}
			c := &Client{cfg: Config{
				Version: "test", Hostname: "local", InstanceID: "instance", HeartbeatInterval: interval,
				LiveSnapshot: func() []*nodectrlv1.LiveSandbox { return []*nodectrlv1.LiveSandbox{{}} },
				WorkspaceStorageSnapshot: func(context.Context) []*nodectrlv1.WorkspaceStorageReport {
					return []*nodectrlv1.WorkspaceStorageReport{{}}
				},
			}, ctrl: nodectrlv1connect.NewNodeControlServiceClient(server.Client(), server.URL)}
			err := c.RegisterAndServe(ctx, nodectrl.Resources{CPUs: 2})
			wants := map[string]string{heartbeatFailureMode: "heartbeat rejected", closedMode: "closed by server", streamFailureMode: "stream failed", missingReadyMode: "registration refused", wrongFirstEventMode: "first event", extraEventMode: "closed by server"}
			if mode == cancelMode {
				if err != nil {
					t.Fatal(err)
				}
				if ctx.Err() != context.Canceled {
					t.Fatal("heartbeat did not cancel registration")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), wants[mode]) {
				t.Fatalf("error = %v, want %q", err, wants[mode])
			}
		})
	}
}
