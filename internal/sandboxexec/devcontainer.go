package sandboxexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/richiejp/darkaifactory/internal/devcontainer"
	"github.com/richiejp/darkaifactory/internal/devcontainerspec"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/sandboxboot"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// devcontainerResolution is the effective boot inputs a devcontainer.json
// projects to: the image (empty for a build-based config), env, forward
// ports, the normalised postStartCommand argv lists, and a build plan
// (non-nil only for a build-based config the workspace didn't override).
type devcontainerResolution struct {
	ImageRef  string
	Env       []string
	Ports     []*sandboxv1.PortMapping
	PostStart [][]string
	Build     *devcontainerBuildPlan
}

// resolveDevcontainerOverlay reads the devcontainer.json named by
// spec.devcontainer_path out of the already-materialised workspace
// content, projects it to a base workspace spec, and overlays the
// workspace's explicit spec fields on top (explicit wins). It returns the
// effective image ref + env + forwardPorts + postStartCommand the assembler
// should boot with and, for a build-based config, a resolved build plan
// (nil otherwise).
//
// This runs node-side at dispatch — the only place the pushed content
// exists — after resolveMounts has reconstructed it onto disk. Reading is
// deliberately strict: a missing/garbled devcontainer fails the dispatch
// loudly rather than silently booting something unexpected (the
// orchestrator trusted the path provisionally at Ready time; this is
// where it's validated for real).
//
// For an image-based config the effective image is always set. For a
// build-based config the projection leaves it empty and returns the build
// plan instead — the caller builds the image, unless the workspace
// explicitly overrode the image (overlay then fills it and the plan is
// ignored).
func resolveDevcontainerOverlay(spec *sandboxv1.SandboxSpec, mounts []sandboxboot.Mount) (devcontainerResolution, error) {
	rel := spec.GetDevcontainerPath()
	file, root, err := findDevcontainerFile(mounts, rel)
	if err != nil {
		return devcontainerResolution{}, err
	}
	// #nosec G304 — file is rooted-and-cleaned under a materialised mount
	// dir by findDevcontainerFile, so it cannot escape the workspace content.
	data, err := os.ReadFile(file)
	if err != nil {
		return devcontainerResolution{}, fmt.Errorf("sandboxexec: read devcontainer %q: %w", rel, err)
	}
	resolved, err := devcontainer.Resolve(data, devcontainer.Substitutions{})
	if err != nil {
		return devcontainerResolution{}, fmt.Errorf("sandboxexec: devcontainer %q: %w", rel, err)
	}
	// ToWorkspaceSpec projects env/ports/mounts for both image- and
	// build-based configs, leaving ImageRef empty for the latter.
	base, err := devcontainerspec.ToWorkspaceSpec(resolved)
	if err != nil {
		return devcontainerResolution{}, fmt.Errorf("sandboxexec: devcontainer %q: %w", rel, err)
	}
	eff := devcontainerspec.Overlay(base, explicitSpec(spec))
	// Only resolve a build plan when the config is build-based and the
	// overlay didn't fill the image from an explicit override — otherwise
	// the build would run pointlessly.
	var build *devcontainerBuildPlan
	if resolved.Build != nil && eff.ImageRef == "" {
		build, err = resolveBuildPlan(file, root, resolved.Build)
		if err != nil {
			return devcontainerResolution{}, fmt.Errorf("sandboxexec: devcontainer %q: %w", rel, err)
		}
	}
	return devcontainerResolution{
		ImageRef:  eff.ImageRef,
		Env:       envStrings(eff.Env),
		Ports:     portMappingsToProto(eff.Ports),
		PostStart: normalizePostStart(resolved.Lifecycle.PostStart),
		Build:     build,
	}, nil
}

// normalizePostStart flattens the resolved devcontainer postStartCommand
// hooks into argv lists the agent can crun-exec directly: a shell-form
// entry becomes ["/bin/sh","-c",<shell>]; an argv-form entry passes
// through. Empty/blank entries are dropped. Returns nil when there are no
// runnable commands, so the boot config stays zero-valued for the common
// (no-postStart) case.
func normalizePostStart(entries []devcontainer.CommandEntry) [][]string {
	var out [][]string
	for _, e := range entries {
		switch {
		case len(e.Argv) > 0:
			out = append(out, e.Argv)
		case e.Shell != "":
			out = append(out, []string{"/bin/sh", "-c", e.Shell})
		}
	}
	return out
}

// findDevcontainerFile locates rel (a path within the workspace content,
// e.g. ".devcontainer/devcontainer.json") under one of the materialised
// mount backing dirs. rel is rooted-and-cleaned so a "../" can't escape a
// mount; the first mount that contains the file wins. It returns the file
// path and the mount Source (root) it was found under — the root bounds a
// build context so a hostile config can't escape the workspace content.
func findDevcontainerFile(mounts []sandboxboot.Mount, rel string) (file, root string, err error) {
	clean := strings.TrimPrefix(filepath.Clean("/"+filepath.FromSlash(rel)), string(filepath.Separator))
	if clean == "" || clean == "." {
		return "", "", fmt.Errorf("sandboxexec: invalid devcontainer path %q", rel)
	}
	for _, m := range mounts {
		if m.Source == "" {
			continue
		}
		cand := filepath.Join(m.Source, clean)
		if fi, statErr := os.Stat(cand); statErr == nil && !fi.IsDir() {
			return cand, m.Source, nil
		}
	}
	return "", "", fmt.Errorf("sandboxexec: devcontainer %q not found in any workspace mount", rel)
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
