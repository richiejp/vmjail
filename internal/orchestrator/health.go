package orchestrator

import (
	"context"

	"connectrpc.com/connect"

	healthv1 "github.com/richiejp/darkaifactory/internal/genpb/health/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
)

// HealthHandler implements healthv1connect.HealthServiceHandler.
// Stateless; one instance is reused across all requests.
type HealthHandler struct {
	healthv1connect.UnimplementedHealthServiceHandler
}

// NewHealthHandler returns a ready-to-use Health handler.
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// Ping echoes the request's Message back. Currently the entire
// Health surface; future readiness/build-info methods will join it.
func (*HealthHandler) Ping(_ context.Context, req *connect.Request[healthv1.PingRequest]) (*connect.Response[healthv1.PingResponse], error) {
	return connect.NewResponse(&healthv1.PingResponse{
		Message: req.Msg.GetMessage(),
	}), nil
}
