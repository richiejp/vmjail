package sandboxexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/localai-org/darkaifactory/internal/devcontainer"
	"github.com/localai-org/darkaifactory/internal/devcontainerspec"
	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/localai-org/darkaifactory/internal/sandboxboot"
	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

// resolveDevcontainerOverlay reads the devcontainer.json named by
// spec.devcontainer_path out of the already-materialised workspace
// content, projects it to a base workspace spec, and overlays the
// workspace's explicit spec fields on top (explicit wins). It returns the
// effective image ref and env the assembler should boot with.
//
// This runs node-side at dispatch — the only place the pushed content
// exists — after resolveMounts has reconstructed it onto disk. Reading is
// deliberately strict: a missing/garbled devcontainer fails the dispatch
// loudly rather than silently booting something unexpected (the
// orchestrator trusted the path provisionally at Ready time; this is
// where it's validated for real). image_ref, env, and forwardPorts are
// overlaid.
func resolveDevcontainerOverlay(spec *sandboxv1.SandboxSpec, mounts []sandboxboot.Mount) (imageRef string, env []string, ports []*sandboxv1.PortMapping, err error) {
	rel := spec.GetDevcontainerPath()
	file, err := findDevcontainerFile(mounts, rel)
	if err != nil {
		return "", nil, nil, err
	}
	// #nosec G304 — file is rooted-and-cleaned under a materialised mount
	// dir by findDevcontainerFile, so it cannot escape the workspace content.
	data, err := os.ReadFile(file)
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandboxexec: read devcontainer %q: %w", rel, err)
	}
	resolved, err := devcontainer.Resolve(data, devcontainer.Substitutions{})
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandboxexec: devcontainer %q: %w", rel, err)
	}
	// ToWorkspaceSpec guarantees a non-empty image on success (it rejects
	// image-less and build-based configs), so the overlay's effective
	// image is always set here.
	base, err := devcontainerspec.ToWorkspaceSpec(resolved)
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandboxexec: devcontainer %q: %w", rel, err)
	}
	eff := devcontainerspec.Overlay(base, explicitSpec(spec))
	return eff.ImageRef, envStrings(eff.Env), portMappingsToProto(eff.Ports), nil
}

// findDevcontainerFile locates rel (a path within the workspace content,
// e.g. ".devcontainer/devcontainer.json") under one of the materialised
// mount backing dirs. rel is rooted-and-cleaned so a "../" can't escape a
// mount; the first mount that contains the file wins.
func findDevcontainerFile(mounts []sandboxboot.Mount, rel string) (string, error) {
	clean := strings.TrimPrefix(filepath.Clean("/"+filepath.FromSlash(rel)), string(filepath.Separator))
	if clean == "" || clean == "." {
		return "", fmt.Errorf("sandboxexec: invalid devcontainer path %q", rel)
	}
	for _, m := range mounts {
		if m.Source == "" {
			continue
		}
		cand := filepath.Join(m.Source, clean)
		if fi, statErr := os.Stat(cand); statErr == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("sandboxexec: devcontainer %q not found in any workspace mount", rel)
}

// explicitSpec projects the SandboxSpec's user-set image+env+ports onto a
// workspacecatalog.Spec — the "override" side of the overlay.
func explicitSpec(spec *sandboxv1.SandboxSpec) workspacecatalog.Spec {
	out := workspacecatalog.Spec{ImageRef: spec.GetImageRef()}
	for _, e := range spec.GetEnv() {
		out.Env = append(out.Env, workspacecatalog.EnvVar{Name: e.GetName(), Value: e.GetValue()})
	}
	for _, p := range spec.GetPorts() {
		out.Ports = append(out.Ports, workspacecatalog.PortMapping{
			ContainerPort: p.GetContainerPort(),
			Protocol:      int32(p.GetProtocol()),
		})
	}
	return out
}

// portMappingsToProto converts the overlay's effective catalog ports back
// to the wire type portForwardsFromSpec consumes.
func portMappingsToProto(in []workspacecatalog.PortMapping) []*sandboxv1.PortMapping {
	if len(in) == 0 {
		return nil
	}
	out := make([]*sandboxv1.PortMapping, 0, len(in))
	for _, p := range in {
		out = append(out, &sandboxv1.PortMapping{
			ContainerPort: p.ContainerPort,
			Protocol:      sandboxv1.Protocol(p.Protocol),
		})
	}
	return out
}

// envStrings flattens catalog EnvVars to the "NAME=VALUE" form
// BootOptions.Env expects.
func envStrings(in []workspacecatalog.EnvVar) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}
