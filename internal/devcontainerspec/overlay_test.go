package devcontainerspec

import (
	"reflect"
	"testing"

	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

const (
	cacheTarget     = "/cache"
	workspaceTarget = "/workspace"
	pipUserName     = "PIP_USER"
	tokenName       = "TOKEN"
)

func TestOverlayScalarsOverrideWinsWhenSet(t *testing.T) {
	base := workspacecatalog.Spec{
		ImageRef:    "base:1",
		MemoryBytes: 1 << 30,
		CPUCores:    2,
	}
	override := workspacecatalog.Spec{
		ImageRef:   "ws:2", // wins
		CPUCores:   8,      // wins
		NestedVirt: true,   // OR
	}
	got := Overlay(base, override)
	if got.ImageRef != "ws:2" {
		t.Errorf("ImageRef = %q, want ws:2 (override wins)", got.ImageRef)
	}
	if got.MemoryBytes != 1<<30 {
		t.Errorf("MemoryBytes = %d, want base value (override unset)", got.MemoryBytes)
	}
	if got.CPUCores != 8 {
		t.Errorf("CPUCores = %d, want 8 (override wins)", got.CPUCores)
	}
	if !got.NestedVirt {
		t.Error("NestedVirt = false, want true (OR)")
	}
}

func TestOverlayEmptyOverrideKeepsBase(t *testing.T) {
	base := workspacecatalog.Spec{
		ImageRef: "base:1",
		Env:      []workspacecatalog.EnvVar{{Name: "A", Value: "1"}},
		Mounts:   []workspacecatalog.Mount{{Target: "/w"}},
		Ports:    []workspacecatalog.PortMapping{{ContainerPort: 80, Protocol: 1}},
	}
	got := Overlay(base, workspacecatalog.Spec{})
	if !reflect.DeepEqual(got, base) {
		t.Errorf("empty override changed base\n got: %+v\nwant: %+v", got, base)
	}
}

func TestOverlayMountsUnionByTargetOverrideWins(t *testing.T) {
	base := []workspacecatalog.Mount{
		{Target: workspaceTarget, Source: ""},        // devcontainer content mount
		{Target: cacheTarget, Source: "/base/cache"}, // base-only
	}
	override := []workspacecatalog.Mount{
		{Target: cacheTarget, Source: "/ws/cache", ReadOnly: true}, // collides → wins
		{Target: "/extra", Source: "/ws/extra"},                    // override-only
	}
	got := Overlay(
		workspacecatalog.Spec{Mounts: base},
		workspacecatalog.Spec{Mounts: override},
	).Mounts
	want := []workspacecatalog.Mount{
		{Target: workspaceTarget, Source: ""},                      // base kept, in place
		{Target: cacheTarget, Source: "/ws/cache", ReadOnly: true}, // replaced in place
		{Target: "/extra", Source: "/ws/extra"},                    // appended
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mounts overlay\n got: %+v\nwant: %+v", got, want)
	}
}

func TestOverlayEnvUnionByNameOverrideWins(t *testing.T) {
	base := []workspacecatalog.EnvVar{{Name: pipUserName, Value: "0"}, {Name: "KEEP", Value: "b"}}
	override := []workspacecatalog.EnvVar{{Name: pipUserName, Value: "1"}, {Name: "NEW", Value: "n"}}
	got := Overlay(
		workspacecatalog.Spec{Env: base},
		workspacecatalog.Spec{Env: override},
	).Env
	want := []workspacecatalog.EnvVar{
		{Name: pipUserName, Value: "1"}, // override wins in place
		{Name: "KEEP", Value: "b"},
		{Name: "NEW", Value: "n"}, // appended
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env overlay\n got: %+v\nwant: %+v", got, want)
	}
}

func TestOverlayPortsKeyedByPortAndProtocol(t *testing.T) {
	base := []workspacecatalog.PortMapping{
		{ContainerPort: 8080, Protocol: 1}, // tcp
		{ContainerPort: 9000, Protocol: 1},
	}
	override := []workspacecatalog.PortMapping{
		{ContainerPort: 8080, Protocol: 2}, // udp/8080 — distinct key, appended
		{ContainerPort: 9000, Protocol: 1}, // tcp/9000 — collides, replaced
	}
	got := Overlay(
		workspacecatalog.Spec{Ports: base},
		workspacecatalog.Spec{Ports: override},
	).Ports
	want := []workspacecatalog.PortMapping{
		{ContainerPort: 8080, Protocol: 1}, // base tcp/8080 kept (udp didn't collide)
		{ContainerPort: 9000, Protocol: 1}, // replaced in place
		{ContainerPort: 8080, Protocol: 2}, // appended udp
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ports overlay\n got: %+v\nwant: %+v", got, want)
	}
}

// TestOverlayEndToEnd composes a devcontainer-derived base (via the real
// projection) with explicit workspace overrides — the slice-2d shape.
func TestOverlayEndToEnd(t *testing.T) {
	base, err := ToWorkspaceSpec(resolve(t, `{
  "image": "ghcr.io/acme/dev:1",
  "containerEnv": { "PIP_USER": "0" },
  "workspaceMount": "source=${localWorkspaceFolder},target=/workspace,type=bind",
  "forwardPorts": [3000]
}`, "/home/dev/proj"))
	if err != nil {
		t.Fatalf("ToWorkspaceSpec: %v", err)
	}
	// Workspace pins a different image and adds an env var.
	override := workspacecatalog.Spec{
		ImageRef: "ghcr.io/acme/dev:pinned",
		Env:      []workspacecatalog.EnvVar{{Name: tokenName, Value: "x"}},
	}
	got := Overlay(base, override)
	if got.ImageRef != "ghcr.io/acme/dev:pinned" {
		t.Errorf("ImageRef = %q, want the workspace's pin", got.ImageRef)
	}
	// devcontainer env preserved, workspace env added.
	wantEnv := []workspacecatalog.EnvVar{{Name: pipUserName, Value: "0"}, {Name: tokenName, Value: "x"}}
	if !reflect.DeepEqual(got.Env, wantEnv) {
		t.Errorf("Env = %+v, want %+v", got.Env, wantEnv)
	}
	// devcontainer mount + port survive (no override for them).
	if len(got.Mounts) != 1 || got.Mounts[0].Target != workspaceTarget || got.Mounts[0].Source != "" {
		t.Errorf("Mounts = %+v, want the devcontainer content mount", got.Mounts)
	}
	if len(got.Ports) != 1 || got.Ports[0].ContainerPort != 3000 {
		t.Errorf("Ports = %+v, want devcontainer's 3000", got.Ports)
	}
}
