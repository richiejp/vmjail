package orchestrator

import (
	"errors"
	"io"
	"log"
	"net"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/ociregistry"
)

// TestSeedRegistryEdgeCases covers the no-seed fast path and the
// bad-ref error path of seedRegistry directly (the happy path is covered
// end-to-end by TestEmbeddedRegistrySeed).
func TestSeedRegistryEdgeCases(t *testing.T) {
	if err := seedRegistry(nil, nil); err != nil {
		t.Fatalf("seedRegistry(_, nil) = %v, want nil (no seeds)", err)
	}
	h := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	if err := seedRegistry(h, []RegistrySeed{{Ref: "BAD REF", Image: img}}); err == nil {
		t.Fatal("seedRegistry with an unparseable ref should error")
	}

	// Listener-failure path via the seedListen seam.
	orig := seedListen
	t.Cleanup(func() { seedListen = orig })
	seedListen = func(string, string) (net.Listener, error) {
		return nil, errors.New("listen boom")
	}
	if err := seedRegistry(h, []RegistrySeed{{Ref: ociregistry.BuiltinBuildRef(), Image: img}}); err == nil {
		t.Fatal("seedRegistry should surface a listener failure")
	}
}

// TestHostedRepoTag covers stripping a hosted ref's host to its
// host-independent repo:tag storage key, plus the parse-error path.
func TestHostedRepoTag(t *testing.T) {
	got, err := hostedRepoTag(ociregistry.BuiltinBuildRef())
	if err != nil {
		t.Fatalf("hostedRepoTag: %v", err)
	}
	if want := ociregistry.BuiltinBuildRepo + ":" + ociregistry.BuiltinBuildTag; got != want {
		t.Errorf("hostedRepoTag = %q, want %q", got, want)
	}
	if _, err := hostedRepoTag("BAD REF"); err == nil {
		t.Error("hostedRepoTag(bad ref) = nil error, want a parse error")
	}
}

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

// TestEmbeddedRegistrySeed proves Config.RegistrySeeds are loaded into the
// embedded registry at construction (before the listener binds) and served:
// an approved peer pulls the seeded ref and gets the same digest. This is
// the path the daft-orchestrator binary uses to auto-seed its bundled build
// image under ociregistry.BuiltinBuildRef(), so a build-based devcontainer
// needs no DAFT_BUILD_IMAGE_REF.
func TestEmbeddedRegistrySeed(t *testing.T) {
	seed, err := random.Image(1024, 2)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref := ociregistry.BuiltinBuildRef()
	p := newTestPair(t, true, true, func(c *Config) {
		c.RegistrySeeds = []RegistrySeed{{Ref: ref, Image: seed}}
	})

	cliTLS, err := identity.ClientTLSConfig(p.cliID, p.cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	rt := ociregistry.Transport(p.server.Addr(), cliTLS)

	pulled, err := crane.Pull(ref, crane.WithTransport(rt))
	if err != nil {
		t.Fatalf("pull seeded ref %s: %v", ref, err)
	}
	want, err := seed.Digest()
	if err != nil {
		t.Fatalf("seed digest: %v", err)
	}
	got, err := pulled.Digest()
	if err != nil {
		t.Fatalf("pulled digest: %v", err)
	}
	if got != want {
		t.Errorf("seeded image digest = %s, want %s", got, want)
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
