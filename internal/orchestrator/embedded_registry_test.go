package orchestrator

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/ociregistry"
)

// TestEmbeddedRegistryPushPull proves the orchestrator serves a working
// OCI registry on its mTLS endpoint: an approved peer pushes an image
// through the ociregistry sentinel-host transport and pulls it back with
// the same digest. This exercises the /v2/ mount, the mTLS gate, and the
// loopback-https workaround (sentinel Host + dial redirect) together.
func TestEmbeddedRegistryPushPull(t *testing.T) {
	p := newTestPair(t, true, true) // client + server mutually approved

	cliTLS, err := identity.ClientTLSConfig(p.cliID, p.cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	rt := ociregistry.Transport(p.server.Addr(), cliTLS)

	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref := ociregistry.Ref("test/embedded", "v1")

	if err := crane.Push(img, ref, crane.WithTransport(rt)); err != nil {
		t.Fatalf("push to embedded registry: %v", err)
	}
	pulled, err := crane.Pull(ref, crane.WithTransport(rt))
	if err != nil {
		t.Fatalf("pull from embedded registry: %v", err)
	}

	want, err := img.Digest()
	if err != nil {
		t.Fatalf("source digest: %v", err)
	}
	got, err := pulled.Digest()
	if err != nil {
		t.Fatalf("pulled digest: %v", err)
	}
	if got != want {
		t.Errorf("round-tripped digest = %s, want %s", got, want)
	}
}

// TestEmbeddedRegistryRejectsUnapprovedPeer pins that the registry is
// behind the same mTLS gate as the RPCs: a peer the orchestrator has not
// approved cannot push.
func TestEmbeddedRegistryRejectsUnapprovedPeer(t *testing.T) {
	p := newTestPair(t, false, true) // server does NOT approve the client

	cliTLS, err := identity.ClientTLSConfig(p.cliID, p.cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	rt := ociregistry.Transport(p.server.Addr(), cliTLS)

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	if err := crane.Push(img, ociregistry.Ref("test/denied", "v1"), crane.WithTransport(rt)); err == nil {
		t.Fatal("expected push from an unapproved peer to be rejected by the mTLS gate")
	}
}
