// Package devcontainerspec projects resolved devcontainer configurations into
// local launch settings and combines them with explicit overrides. Build inputs,
// lifecycle commands and user settings remain on devcontainer.Spec for callers.
package devcontainerspec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/richiejp/vmjail/internal/devcontainer"
	sandboxv1 "github.com/richiejp/vmjail/internal/genpb/sandbox/v1"
)

// ErrNoImage is returned when a Spec selects neither an image nor a build.
var ErrNoImage = errors.New("devcontainerspec: devcontainer selects no image")

// ToSpec projects a devcontainer.Spec onto a
// Spec: image_ref (empty for a build-based config — the
// image is built later from s.Build), the workspace mount and any extra
// mounts, containerEnv, and forwardPorts (as TCP). Mount targets must be
// absolute (the executor rejects relative/empty targets); ports must be
// in 1..65535.
func ToSpec(s *devcontainer.Spec) (Spec, error) {
	if s == nil {
		return Spec{}, errors.New("devcontainerspec: nil spec")
	}
	if s.Image == "" && s.Build == nil {
		return Spec{}, ErrNoImage
	}

	mounts, err := projectMounts(s)
	if err != nil {
		return Spec{}, err
	}
	ports, err := projectPorts(s.ForwardPorts)
	if err != nil {
		return Spec{}, err
	}
	return Spec{
		ImageRef: s.Image,
		Mounts:   mounts,
		Env:      projectEnv(s.Env),
		Ports:    ports,
	}, nil
}

// projectMounts preserves resolved host sources for the workspace and extra
// mounts. Targets must be absolute and unique.
func projectMounts(s *devcontainer.Spec) ([]Mount, error) {
	out := make([]Mount, 0, 1+len(s.Mounts))
	if m := s.Workspace.Mount; m != nil {
		out = append(out, Mount{Target: m.Target, Source: m.Source, ReadOnly: m.ReadOnly})
	}
	for _, m := range s.Mounts {
		out = append(out, Mount{Target: m.Target, Source: m.Source, ReadOnly: m.ReadOnly})
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
// local EnvVar. The devcontainer.Spec guarantees this shape (resolveEnv
// builds it), so a malformed entry is a programmer error, not user input.
func projectEnv(env []string) []EnvVar {
	if len(env) == 0 {
		return nil
	}
	out := make([]EnvVar, 0, len(env))
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		out = append(out, EnvVar{Name: name, Value: value})
	}
	return out
}

// projectPorts maps each forwarded container port to a TCP PortMapping.
// devcontainer.json has no UDP form, so every entry is TCP.
func projectPorts(ports []int) ([]PortMapping, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	out := make([]PortMapping, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("devcontainerspec: forwardPorts entry %d out of range 1..65535", p)
		}
		out = append(out, PortMapping{
			ContainerPort: uint32(p),
			Protocol:      int32(sandboxv1.Protocol_PROTOCOL_TCP),
		})
	}
	return out, nil
}
