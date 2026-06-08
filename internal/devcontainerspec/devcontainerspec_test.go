package devcontainerspec

import (
	"errors"
	"reflect"
	"testing"

	"github.com/localai-org/darkaifactory/internal/devcontainer"
	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

// resolve is a test helper: parse+resolve a devcontainer.json body with
// the given local workspace folder.
func resolve(t *testing.T, body, localWS string) *devcontainer.Spec {
	t.Helper()
	spec, err := devcontainer.Resolve([]byte(body), devcontainer.Substitutions{LocalWorkspaceFolder: localWS})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return spec
}

func TestToWorkspaceSpecImageBased(t *testing.T) {
	dc := resolve(t, `{
  "image": "ghcr.io/acme/dev:1",
  "containerEnv": { "PIP_USER": "0", "TOKEN": "" },
  "workspaceMount": "source=${localWorkspaceFolder},target=/workspace/app,type=bind",
  "workspaceFolder": "/workspace/app",
  "mounts": [ "source=/host/cache,target=/cache,type=bind,readonly" ],
  "forwardPorts": [3000, "8080"]
}`, "/home/dev/app")

	got, err := ToWorkspaceSpec(dc)
	if err != nil {
		t.Fatalf("ToWorkspaceSpec: %v", err)
	}

	want := workspacecatalog.Spec{
		ImageRef: "ghcr.io/acme/dev:1",
		Mounts: []workspacecatalog.Mount{
			// (B) push-CAS: workspace-content mount has an empty Source;
			// the local /home/dev/app dir is pushed up separately.
			{Target: "/workspace/app", Source: ""},
			// Extra mount keeps its absolute Source (direct host bind).
			{Target: "/cache", Source: "/host/cache", ReadOnly: true},
		},
		Env: []workspacecatalog.EnvVar{
			{Name: "PIP_USER", Value: "0"},
			{Name: "TOKEN", Value: ""},
		},
		Ports: []workspacecatalog.PortMapping{
			{ContainerPort: 3000, Protocol: int32(sandboxv1.Protocol_PROTOCOL_TCP)},
			{ContainerPort: 8080, Protocol: int32(sandboxv1.Protocol_PROTOCOL_TCP)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToWorkspaceSpec mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestPyTorchCPURejectedUntilBuilder pins the current boundary: PyTorch's
// real CPU config is build-based, so it cannot boot until the slice-3
// image builder lands. The error must be explicit, not a silent drop.
func TestPyTorchCPURejectedUntilBuilder(t *testing.T) {
	dc := resolve(t, `{
  "name": "PyTorch - CPU",
  "build": { "dockerfile": "../Dockerfile", "context": "./" },
  "workspaceFolder": "/workspace/pytorch"
}`, "/home/dev/pytorch")

	_, err := ToWorkspaceSpec(dc)
	if !errors.Is(err, ErrBuildUnsupported) {
		t.Errorf("err = %v, want ErrBuildUnsupported", err)
	}
}

func TestToWorkspaceSpecErrors(t *testing.T) {
	tcs := []struct {
		name string
		spec *devcontainer.Spec
		want error
	}{
		{"nil", nil, nil},
		{"no image", &devcontainer.Spec{}, ErrNoImage},
		{"build", &devcontainer.Spec{Build: &devcontainer.BuildSpec{}}, ErrBuildUnsupported},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ToWorkspaceSpec(tc.spec)
			if err == nil {
				t.Fatal("want error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRelativeAndDuplicateMountTargetsRejected(t *testing.T) {
	rel := &devcontainer.Spec{
		Image:  "x",
		Mounts: []devcontainer.MountSpec{{Target: "relative/path", Source: "/s"}},
	}
	if _, err := ToWorkspaceSpec(rel); err == nil {
		t.Error("relative mount target: want error")
	}

	dup := &devcontainer.Spec{
		Image:     "x",
		Workspace: devcontainer.WorkspaceSpec{Mount: &devcontainer.MountSpec{Target: "/w", Source: "/a"}},
		Mounts:    []devcontainer.MountSpec{{Target: "/w", Source: "/b"}},
	}
	if _, err := ToWorkspaceSpec(dup); err == nil {
		t.Error("duplicate mount target: want error")
	}
}

func TestPortRange(t *testing.T) {
	for _, bad := range []int{0, -1, 70000} {
		s := &devcontainer.Spec{Image: "x", ForwardPorts: []int{bad}}
		if _, err := ToWorkspaceSpec(s); err == nil {
			t.Errorf("port %d: want range error", bad)
		}
	}
}

// TestNoMountsNoPorts confirms a bare image-only config projects to nil
// slices (not empty non-nil ones) so it round-trips cleanly.
func TestNoMountsNoPorts(t *testing.T) {
	got, err := ToWorkspaceSpec(&devcontainer.Spec{Image: "x"})
	if err != nil {
		t.Fatalf("ToWorkspaceSpec: %v", err)
	}
	if got.Mounts != nil || got.Ports != nil || got.Env != nil {
		t.Errorf("bare image spec should project nil slices: %+v", got)
	}
	if got.ImageRef != "x" {
		t.Errorf("ImageRef = %q", got.ImageRef)
	}
}
