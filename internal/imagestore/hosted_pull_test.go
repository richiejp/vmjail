package imagestore

import (
	"context"
	"crypto/tls"
	"net/http/httptest"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/richiejp/darkaifactory/internal/ociregistry"
)

// TestPullHostedRoutesThroughTransport covers the orchestrator-hosted
// pull path: with a transport wired via UseHostedRegistry, a ref whose
// registry is ociregistry.Host is fetched over that transport (not the
// default keychain). A TLS registry stands in for the orchestrator's
// mTLS endpoint.
func TestPullHostedRoutesThroughTransport(t *testing.T) {
	srv := httptest.NewTLSServer(registry.New())
	t.Cleanup(srv.Close)
	rt := ociregistry.Transport(srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test stand-in for TOFU mTLS.

	img, err := random.Image(2048, 2)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref := ociregistry.Ref("test/hosted", "v1")
	if err := crane.Push(img, ref, crane.WithTransport(rt)); err != nil {
		t.Fatalf("seed push: %v", err)
	}

	store, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store.UseHostedRegistry(rt)

	got, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull hosted ref: %v", err)
	}
	if got == nil || got.Ref != ref {
		t.Errorf("pulled = %+v, want ref %s", got, ref)
	}
}
