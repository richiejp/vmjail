// Package devcontainerspec projects a resolved devcontainer.Spec onto a
// workspacecatalog.Spec — the catalog's domain type that the
// orchestrator already projects to a sandbox.v1.SandboxSpec at Launch
// and that the node executor turns into virtio-fs shares (with #100
// uid-translation), env, and port forwards.
//
// This is the devcontainer.json → bootable-workspace seam: once a config
// is a workspacecatalog.Spec it flows through the entire existing
// pipeline unchanged. The projection is image-based only — a build
// stanza needs the in-sandbox image builder (PLAN.md D6c.5 slice 3) that
// does not exist yet, so it is rejected rather than silently ignored.
//
// Two devcontainer fields have no home in workspacecatalog.Spec and so
// are NOT consumed here: postCreateCommand (no post-boot-exec mechanism
// at launch yet — slice 2b) and remoteUser/containerUser (daft derives
// the container uid from the image USER; an override field is slice 4).
// They remain on the devcontainer.Spec for the caller, which is the
// honest place for them until the machinery exists.
package devcontainerspec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/localai-org/darkaifactory/internal/devcontainer"
	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

// ErrBuildUnsupported is returned for a build-based config. daft can
// only pull images today (imagestore only pulls); building a
// devcontainer Dockerfile is PLAN.md D6c.5 slice 3.
var ErrBuildUnsupported = errors.New("devcontainerspec: build-based devcontainer needs an image builder (not yet implemented); use an image-based config")

// ErrNoImage is returned when a Spec names no image (and no build).
var ErrNoImage = errors.New("devcontainerspec: devcontainer selects no image")

// ToWorkspaceSpec projects an image-based devcontainer.Spec onto a
// workspacecatalog.Spec: image_ref, the workspace mount and any extra
// mounts, containerEnv, and forwardPorts (as TCP). Mount targets must be
// absolute (the executor rejects relative/empty targets); ports must be
// in 1..65535.
func ToWorkspaceSpec(s *devcontainer.Spec) (workspacecatalog.Spec, error) {
	if s == nil {
		return workspacecatalog.Spec{}, errors.New("devcontainerspec: nil spec")
	}
	if s.Build != nil {
		return workspacecatalog.Spec{}, ErrBuildUnsupported
	}
	if s.Image == "" {
		return workspacecatalog.Spec{}, ErrNoImage
	}

	mounts, err := projectMounts(s)
	if err != nil {
		return workspacecatalog.Spec{}, err
	}
	ports, err := projectPorts(s.ForwardPorts)
	if err != nil {
		return workspacecatalog.Spec{}, err
	}
	return workspacecatalog.Spec{
		ImageRef: s.Image,
		Mounts:   mounts,
		Env:      projectEnv(s.Env),
		Ports:    ports,
	}, nil
}

// projectMounts emits the workspace-content mount first (when
// workspaceMount was given) followed by the extra mounts, all as catalog
// Mounts. A target collision is rejected — two binds at one path would
// race the agent's mount loop the same way duplicate spec mounts do.
//
// The workspace-content mount uses model (B), push-CAS: its Source is
// left EMPTY so the node resolves it to the workspace UUID + CAS at
// dispatch (the established `daft workspace mount` shape), with the local
// project dir pushed up separately — NOT a raw host bind of
// ${localWorkspaceFolder}. Extra devcontainer mounts keep their Source:
// an absolute one is a direct host bind (the advanced `mount add
// --source /abs` path the executor honours).
func projectMounts(s *devcontainer.Spec) ([]workspacecatalog.Mount, error) {
	out := make([]workspacecatalog.Mount, 0, 1+len(s.Mounts))
	if m := s.Workspace.Mount; m != nil {
		out = append(out, workspacecatalog.Mount{Target: m.Target, Source: "", ReadOnly: m.ReadOnly})
	}
	for _, m := range s.Mounts {
		out = append(out, workspacecatalog.Mount{Target: m.Target, Source: m.Source, ReadOnly: m.ReadOnly})
	}
	seen := make(map[string]struct{}, len(out))
	for _, m := range out {
		if !strings.HasPrefix(m.Target, "/") {
			return nil, fmt.Errorf("devcontainerspec: mount target %q must be absolute", m.Target)
		}
		if _, dup := seen[m.Target]; dup {
			return nil, fmt.Errorf("devcontainerspec: mount target %q duplicated", m.Target)
		}
		seen[m.Target] = struct{}{}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// projectEnv splits each "NAME=VALUE" entry on the first '=' into a
// catalog EnvVar. The devcontainer.Spec guarantees this shape (resolveEnv
// builds it), so a malformed entry is a programmer error, not user input.
func projectEnv(env []string) []workspacecatalog.EnvVar {
	if len(env) == 0 {
		return nil
	}
	out := make([]workspacecatalog.EnvVar, 0, len(env))
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		out = append(out, workspacecatalog.EnvVar{Name: name, Value: value})
	}
	return out
}

// projectPorts maps each forwarded container port to a TCP PortMapping.
// devcontainer.json has no UDP form, so every entry is TCP.
func projectPorts(ports []int) ([]workspacecatalog.PortMapping, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	out := make([]workspacecatalog.PortMapping, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("devcontainerspec: forwardPorts entry %d out of range 1..65535", p)
		}
		out = append(out, workspacecatalog.PortMapping{
			ContainerPort: uint32(p),
			Protocol:      int32(sandboxv1.Protocol_PROTOCOL_TCP),
		})
	}
	return out, nil
}
