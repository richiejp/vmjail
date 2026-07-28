package devcontainer

import (
	"errors"
	"reflect"
	"testing"
)

const bindType = "bind"

// pytorchCPU is PyTorch's real .devcontainer/cpu/devcontainer.json
// (github.com/pytorch/pytorch), kept verbatim — comments, trailing
// commas, the docker --mount workspaceMount string, a build stanza with
// a numeric/blank arg, a feature, and a postCreateCommand. It is the
// headline fixture: if daft can resolve this, it can resolve the bulk of
// real-world configs.
const pytorchCPU = `
// For format details, see https://aka.ms/devcontainer.json. For config options, see the
// README at: https://github.com/devcontainers/templates/tree/main/src/anaconda
{
  "name": "PyTorch - CPU",
  "build": {
    "context": "./",
    "dockerfile": "../Dockerfile",
    "args": {
      "USERNAME": "vscode",
      "BUILDKIT_INLINE_CACHE": "0",
      "CLANG_VERSION": ""
    }
  },
  // Mount the full repo only after the container starts
  "workspaceMount": "source=${localWorkspaceFolder},target=/workspace/pytorch,type=bind,consistency=cached",
  "workspaceFolder": "/workspace/pytorch",
  "containerEnv": {
    "PIP_USER": "0"
  },
  "features": {
    "ghcr.io/devcontainers/features/rust:1" : {}
  },
  "postCreateCommand": "bash .devcontainer/scripts/install-dev-tools.sh",
  "customizations": {
    "vscode": {
      "extensions": ["streetsidesoftware.code-spell-checker"]
    }
  }
}
`

func TestResolvePyTorchCPU(t *testing.T) {
	spec, err := Resolve([]byte(pytorchCPU), Substitutions{
		LocalWorkspaceFolder: "/home/dev/pytorch",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if spec.Name != "PyTorch - CPU" {
		t.Errorf("Name = %q", spec.Name)
	}
	if spec.Image != "" {
		t.Errorf("Image = %q, want empty (build-based)", spec.Image)
	}
	if spec.Build == nil {
		t.Fatal("Build = nil, want a build stanza")
	}
	if spec.Build.Dockerfile != "../Dockerfile" || spec.Build.Context != "./" {
		t.Errorf("Build dockerfile/context = %q/%q", spec.Build.Dockerfile, spec.Build.Context)
	}
	// Numeric "0" must stay "0", blank arg must stay "".
	if got := spec.Build.Args["BUILDKIT_INLINE_CACHE"]; got != "0" {
		t.Errorf("BUILDKIT_INLINE_CACHE = %q, want 0", got)
	}
	if got, ok := spec.Build.Args["CLANG_VERSION"]; !ok || got != "" {
		t.Errorf("CLANG_VERSION = %q (present=%v), want empty+present", got, ok)
	}

	// containerEnv → sorted NAME=VALUE.
	if !reflect.DeepEqual(spec.Env, []string{"PIP_USER=0"}) {
		t.Errorf("Env = %v", spec.Env)
	}

	// workspaceMount: ${localWorkspaceFolder} expanded, target parsed.
	if spec.Workspace.Folder != "/workspace/pytorch" {
		t.Errorf("Workspace.Folder = %q", spec.Workspace.Folder)
	}
	wm := spec.Workspace.Mount
	if wm == nil {
		t.Fatal("Workspace.Mount = nil")
	}
	if wm.Source != "/home/dev/pytorch" || wm.Target != "/workspace/pytorch" || wm.Type != bindType {
		t.Errorf("Workspace.Mount = %+v", wm)
	}

	if len(spec.Features) != 1 || spec.Features[0].ID != "ghcr.io/devcontainers/features/rust:1" {
		t.Errorf("Features = %+v", spec.Features)
	}

	pc := spec.Lifecycle.PostCreate
	if len(pc) != 1 || pc[0].Shell != "bash .devcontainer/scripts/install-dev-tools.sh" {
		t.Errorf("PostCreate = %+v", pc)
	}
}

func TestResolveOverrideCommand(t *testing.T) {
	defaulted, err := Resolve([]byte(`{"image":"example.invalid/dev:1"}`), Substitutions{})
	if err != nil {
		t.Fatal(err)
	}
	if !defaulted.OverrideCommand {
		t.Fatal("overrideCommand should default to true for an image devcontainer")
	}
	disabled, err := Resolve([]byte(`{"image":"example.invalid/dev:1","overrideCommand":false}`), Substitutions{})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.OverrideCommand {
		t.Fatal("explicit overrideCommand=false was not preserved")
	}
}

func TestResolveImageBasedAndSubstitution(t *testing.T) {
	cfg := `{
  "name": "img",
  "image": "ghcr.io/acme/${localEnv:FLAVOR:base}:latest",
  "remoteUser": "vscode",
  "forwardPorts": [3000, "127.0.0.1:5432", "8080"],
  "containerEnv": { "B": "${containerWorkspaceFolder}", "A": "${localEnv:MISSING:fallback}" },
  "mounts": [
    "source=/host/cache,target=/cache,type=bind,readonly",
    { "source": "/host/data", "target": "/data", "type": "bind" }
  ],
  "postCreateCommand": ["npm", "ci"],
  "postStartCommand": { "z": "echo z", "a": ["echo", "a"] }
}`
	spec, err := Resolve([]byte(cfg), Substitutions{
		ContainerWorkspaceFolder: "/work",
		LocalEnv: func(name string) (string, bool) {
			if name == "FLAVOR" {
				return "gpu", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Image != "ghcr.io/acme/gpu:latest" {
		t.Errorf("Image = %q, want gpu flavor", spec.Image)
	}
	if spec.RemoteUser != "vscode" {
		t.Errorf("RemoteUser = %q", spec.RemoteUser)
	}
	// int, host:port, and bare string → container ports, in order.
	if !reflect.DeepEqual(spec.ForwardPorts, []int{3000, 5432, 8080}) {
		t.Errorf("ForwardPorts = %v", spec.ForwardPorts)
	}
	// Env sorted by name; ${containerWorkspaceFolder} and a missing
	// localEnv with a fallback both resolved.
	if !reflect.DeepEqual(spec.Env, []string{"A=fallback", "B=/work"}) {
		t.Errorf("Env = %v", spec.Env)
	}
	// mount string (readonly flag) + object form.
	want := []MountSpec{
		{Source: "/host/cache", Target: "/cache", Type: bindType, ReadOnly: true},
		{Source: "/host/data", Target: "/data", Type: "bind"},
	}
	if !reflect.DeepEqual(spec.Mounts, want) {
		t.Errorf("Mounts = %+v, want %+v", spec.Mounts, want)
	}
	// argv form preserved without a shell.
	pc := spec.Lifecycle.PostCreate
	if len(pc) != 1 || pc[0].Shell != "" || !reflect.DeepEqual(pc[0].Argv, []string{"npm", "ci"}) {
		t.Errorf("PostCreate = %+v", pc)
	}
	// object form → entries sorted by name (a before z).
	ps := spec.Lifecycle.PostStart
	if len(ps) != 2 || ps[0].Name != "a" || ps[1].Name != "z" {
		t.Fatalf("PostStart = %+v", ps)
	}
	if !reflect.DeepEqual(ps[0].Argv, []string{"echo", "a"}) || ps[1].Shell != "echo z" {
		t.Errorf("PostStart entries = %+v", ps)
	}
}

func TestResolveErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want error
	}{
		{"compose", `{"dockerComposeFile": "docker-compose.yml", "service": "app"}`, ErrComposeUnsupported},
		{"compose-array", `{"dockerComposeFile": ["a.yml","b.yml"], "service": "app"}`, ErrComposeUnsupported},
		{"no-source", `{"name": "x"}`, ErrNoSource},
		{"image-and-build", `{"image": "x", "build": {"dockerfile": "D"}}`, ErrImageAndBuild},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve([]byte(tc.cfg), Substitutions{})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestResolveLegacyDockerFileAndWorkspaceDefault(t *testing.T) {
	// Legacy top-level dockerFile+context, and containerWorkspaceFolder
	// defaulting to workspaceFolder when the caller leaves it unset.
	cfg := `{
  "dockerFile": "Containerfile",
  "context": "sub",
  "workspaceFolder": "/src",
  "onCreateCommand": "echo ${containerWorkspaceFolder}"
}`
	spec, err := Resolve([]byte(cfg), Substitutions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Build == nil || spec.Build.Dockerfile != "Containerfile" || spec.Build.Context != "sub" {
		t.Fatalf("Build = %+v", spec.Build)
	}
	oc := spec.Lifecycle.OnCreate
	if len(oc) != 1 || oc[0].Shell != "echo /src" {
		t.Errorf("OnCreate = %+v (want default containerWorkspaceFolder=/src)", oc)
	}
}

func TestParseErrorsAndUnknownVarKept(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Error("Parse of malformed json: want error")
	}
	// Unknown variable is left verbatim (typo stays visible).
	spec, err := Resolve([]byte(`{"image": "x", "containerEnv": {"K": "${bogusVar}/y"}}`), Substitutions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(spec.Env, []string{"K=${bogusVar}/y"}) {
		t.Errorf("Env = %v, want unknown var preserved", spec.Env)
	}
}

func TestWorkspaceMountMissingTarget(t *testing.T) {
	_, err := Resolve([]byte(`{"image":"x","workspaceMount":"source=/s,type=bind"}`), Substitutions{})
	if err == nil {
		t.Error("workspaceMount without target: want error")
	}
}
