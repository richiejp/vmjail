package nodectrlclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

type failingRandomReader struct{}

func (failingRandomReader) Read([]byte) (int, error) {
	return 0, errors.New("random source failed")
}

const testURL = "https://x"

func TestNewRejectsMissingFields(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	peers := identity.NewMemoryPeerStore()

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty_url", Config{Identity: id, Peers: peers}, "empty URL"},
		{"nil_identity", Config{URL: testURL, Peers: peers}, "nil identity"},
		{"nil_peers", Config{URL: testURL, Identity: id}, "nil peer store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestNewSurfacesTLSConfigError covers the ClientTLSConfig
// failure branch by passing an Identity with missing fields.
func TestNewSurfacesTLSConfigError(t *testing.T) {
	_, err := New(Config{
		URL:      testURL,
		Identity: &identity.Identity{}, // empty: ClientTLSConfig rejects
		Peers:    identity.NewMemoryPeerStore(),
	})
	if err == nil || !strings.Contains(err.Error(), "TLS config") {
		t.Errorf("err = %v, want substring 'TLS config'", err)
	}
}

func TestNewAcceptsValidConfig(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c, err := New(Config{
		URL:      "https://orchestrator.invalid:8443",
		Identity: id,
		Peers:    identity.NewMemoryPeerStore(),
		Version:  "v0.0.0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Identity() != id {
		t.Error("Identity() did not return the configured identity")
	}
	if c.cfg.InstanceID == "" {
		t.Error("New did not mint a process instance ID")
	}
	if len(c.cfg.InstanceID) != 36 || c.cfg.InstanceID[14] != '4' {
		t.Errorf("minted instance ID = %q, want UUIDv4", c.cfg.InstanceID)
	}
}

func TestNewPreservesAndBoundsInstanceID(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	peers := identity.NewMemoryPeerStore()
	c, err := New(Config{URL: testURL, Identity: id, Peers: peers, InstanceID: "fixed-process"})
	if err != nil {
		t.Fatalf("New fixed instance: %v", err)
	}
	if c.cfg.InstanceID != "fixed-process" {
		t.Errorf("InstanceID = %q, want fixed-process", c.cfg.InstanceID)
	}
	_, err = New(Config{URL: testURL, Identity: id, Peers: peers, InstanceID: strings.Repeat("x", 129)})
	if err == nil || !strings.Contains(err.Error(), "instance ID is too long") {
		t.Errorf("oversize err = %v, want instance ID length error", err)
	}
}

func TestNewSurfacesInstanceIDRandomnessFailure(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, err = newWithRandom(Config{URL: testURL, Identity: id, Peers: identity.NewMemoryPeerStore()}, failingRandomReader{})
	if err == nil || !strings.Contains(err.Error(), "mint instance ID: random source failed") {
		t.Errorf("err = %v, want random source failure", err)
	}
}
