package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ErrComposeUnsupported is returned when a config selects the container
// via dockerComposeFile. daft runs a single container per VM, not a
// compose project; this is a deliberate non-goal (see PLAN.md).
var ErrComposeUnsupported = errors.New("devcontainer: dockerComposeFile is not supported")

// ErrNoSource is returned when a config names neither an image nor a
// build (nor a compose file) — there is nothing to run.
var ErrNoSource = errors.New("devcontainer: config selects no image or build")

// ErrImageAndBuild is returned when a config sets both image and build,
// which the spec treats as mutually exclusive.
var ErrImageAndBuild = errors.New("devcontainer: image and build are mutually exclusive")

// Spec is the resolved, daft-shaped intermediate representation: the
// devcontainer.json fields daft consumes, with ${…} variables expanded,
// lifecycle commands normalised, and the image-vs-build choice
// validated. It is intentionally runtime-agnostic — slice 2 maps it onto
// sandboxboot (shares, bundle env, port forwards, post-create exec).
type Spec struct {
	Name string

	// Exactly one of Image or Build is set.
	Image string
	Build *BuildSpec

	// Env is containerEnv flattened to "NAME=VALUE", sorted by name for
	// determinism — ready to merge over the image env the way
	// imagestore.BundleOptions.Env expects.
	Env []string

	// RemoteUser/ContainerUser carry the spec's user intent. daft
	// already derives the container uid from the image USER (#97/#98);
	// these let a config override it. Empty means "honour the image".
	RemoteUser    string
	ContainerUser string

	// Workspace is where the project source mounts. Folder is the
	// in-container path tools open; Mount, when set, is the host→guest
	// bind parsed from workspaceMount.
	Workspace WorkspaceSpec

	// Mounts are additional binds beyond the workspace.
	Mounts []MountSpec

	// ForwardPorts are container-side ports to expose.
	ForwardPorts []int

	// Features are the requested dev-container features (OCI install
	// units). Options are kept raw — feature resolution is a later
	// slice; this records intent.
	Features []FeatureRef

	// Lifecycle holds the normalised hooks. In practice PostCreate is
	// the one that matters (44% of real configs); the rest are recorded
	// for completeness.
	Lifecycle LifecycleSpec
}

// BuildSpec is the resolved image-from-Dockerfile stanza.
type BuildSpec struct {
	Dockerfile string
	Context    string
	Args       map[string]string
	Target     string
	Options    []string
}

// WorkspaceSpec is the resolved workspace location.
type WorkspaceSpec struct {
	Folder string
	Mount  *MountSpec
}

// MountSpec is a resolved mount.
type MountSpec struct {
	Source   string
	Target   string
	Type     string
	ReadOnly bool
}

// FeatureRef is a requested feature: its OCI id and raw options.
type FeatureRef struct {
	ID      string
	Options json.RawMessage
}

// LifecycleSpec holds the normalised hooks. initializeCommand runs on
// the host; the rest run in the container.
type LifecycleSpec struct {
	Initialize    []CommandEntry
	OnCreate      []CommandEntry
	UpdateContent []CommandEntry
	PostCreate    []CommandEntry
	PostStart     []CommandEntry
	PostAttach    []CommandEntry
}

// Substitutions supplies the values for the devcontainer.json variables
// (${localWorkspaceFolder}, ${localEnv:VAR}, …). A zero Substitutions is
// usable; unset fields expand to "".
type Substitutions struct {
	// LocalWorkspaceFolder is the host path of the project being opened.
	LocalWorkspaceFolder string
	// ContainerWorkspaceFolder is the in-container project path. If
	// empty, Resolve defaults it to the config's workspaceFolder.
	ContainerWorkspaceFolder string
	// LocalEnv resolves ${localEnv:NAME}. nil means "no local env".
	LocalEnv func(name string) (string, bool)
	// DevcontainerID resolves ${devcontainerId}.
	DevcontainerID string
}

// Resolve parses jsonc bytes and resolves them in one step.
func Resolve(data []byte, sub Substitutions) (*Spec, error) {
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return c.Resolve(sub)
}

// Resolve validates the image/build choice, expands ${…} variables, and
// normalises the config into a Spec.
func (c *Config) Resolve(sub Substitutions) (*Spec, error) {
	if len(c.DockerComposeFile) > 0 {
		return nil, ErrComposeUnsupported
	}
	hasBuild := c.Build != nil || c.DockerFile != ""
	switch {
	case c.Image != "" && hasBuild:
		return nil, ErrImageAndBuild
	case c.Image == "" && !hasBuild:
		return nil, ErrNoSource
	}

	if sub.ContainerWorkspaceFolder == "" {
		sub.ContainerWorkspaceFolder = c.WorkspaceFolder
	}
	exp := func(s string) string { return sub.expand(s, c.ContainerEnv) }

	spec := &Spec{
		Name:          c.Name,
		Image:         exp(c.Image),
		RemoteUser:    c.RemoteUser,
		ContainerUser: c.ContainerUser,
		Env:           resolveEnv(c.ContainerEnv, exp),
		ForwardPorts:  resolvePorts(c.ForwardPorts),
		Features:      resolveFeatures(c.Features),
		Lifecycle:     resolveLifecycle(c, exp),
	}
	spec.Build = resolveBuild(c, exp)
	if err := resolveWorkspace(c, exp, spec); err != nil {
		return nil, err
	}
	spec.Mounts = resolveMounts(c.Mounts, exp)
	return spec, nil
}

func resolveBuild(c *Config, exp func(string) string) *BuildSpec {
	if c.Build != nil {
		b := &BuildSpec{
			Dockerfile: exp(c.Build.Dockerfile),
			Context:    exp(c.Build.Context),
			Target:     c.Build.Target,
			Options:    c.Build.Options,
		}
		if len(c.Build.Args) > 0 {
			b.Args = make(map[string]string, len(c.Build.Args))
			for k, v := range c.Build.Args {
				b.Args[k] = exp(v)
			}
		}
		return b
	}
	if c.DockerFile != "" { // legacy top-level form
		return &BuildSpec{Dockerfile: exp(c.DockerFile), Context: exp(c.Context)}
	}
	return nil
}

func resolveEnv(env map[string]string, exp func(string) string) []string {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+exp(env[name]))
	}
	return out
}

func resolvePorts(ports []Port) []int {
	if len(ports) == 0 {
		return nil
	}
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		out = append(out, int(p))
	}
	return out
}

func resolveFeatures(features map[string]json.RawMessage) []FeatureRef {
	if len(features) == 0 {
		return nil
	}
	ids := make([]string, 0, len(features))
	for id := range features {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]FeatureRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, FeatureRef{ID: id, Options: features[id]})
	}
	return out
}

func resolveWorkspace(c *Config, exp func(string) string, spec *Spec) error {
	spec.Workspace.Folder = exp(c.WorkspaceFolder)
	if c.WorkspaceMount == "" {
		return nil
	}
	var m Mount
	if err := m.parseMountString(c.WorkspaceMount); err != nil {
		return fmt.Errorf("devcontainer: workspaceMount: %w", err)
	}
	ms := MountSpec{
		Source:   exp(m.Source),
		Target:   exp(m.Target),
		Type:     m.Type,
		ReadOnly: m.ReadOnly,
	}
	if ms.Target == "" {
		return errors.New("devcontainer: workspaceMount has no target")
	}
	spec.Workspace.Mount = &ms
	return nil
}

func resolveMounts(mounts []Mount, exp func(string) string) []MountSpec {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]MountSpec, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, MountSpec{
			Source:   exp(m.Source),
			Target:   exp(m.Target),
			Type:     m.Type,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func resolveLifecycle(c *Config, exp func(string) string) LifecycleSpec {
	return LifecycleSpec{
		Initialize:    expandEntries(c.InitializeCommand.Entries, exp),
		OnCreate:      expandEntries(c.OnCreateCommand.Entries, exp),
		UpdateContent: expandEntries(c.UpdateContentCommand.Entries, exp),
		PostCreate:    expandEntries(c.PostCreateCommand.Entries, exp),
		PostStart:     expandEntries(c.PostStartCommand.Entries, exp),
		PostAttach:    expandEntries(c.PostAttachCommand.Entries, exp),
	}
}

func expandEntries(entries []CommandEntry, exp func(string) string) []CommandEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]CommandEntry, 0, len(entries))
	for _, e := range entries {
		ne := CommandEntry{Name: e.Name, Shell: exp(e.Shell)}
		if len(e.Argv) > 0 {
			ne.Argv = make([]string, len(e.Argv))
			for i, a := range e.Argv {
				ne.Argv[i] = exp(a)
			}
		}
		out = append(out, ne)
	}
	return out
}

// expand replaces ${…} variables in s. Unknown variables are left
// verbatim so a typo is visible rather than silently blanked.
func (sub Substitutions) expand(s string, containerEnv map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	//daft:lint:disable=forloopbound reason="terminates when strings.Index finds no more ${ (returns -1); s shrinks past each match every iteration"
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			b.WriteString(s) // unterminated — leave the rest as-is
			break
		}
		j += i
		b.WriteString(s[:i])
		name := s[i+2 : j]
		if val, ok := sub.lookup(name, containerEnv); ok {
			b.WriteString(val)
		} else {
			b.WriteString(s[i : j+1]) // leave ${name} verbatim
		}
		s = s[j+1:]
	}
	return b.String()
}

// lookup resolves a single variable name. ok is false for an unknown
// variable (so the caller can leave it verbatim); it is true with an
// empty value for a known-but-unset variable.
func (sub Substitutions) lookup(name string, containerEnv map[string]string) (string, bool) {
	switch name {
	case "localWorkspaceFolder":
		return sub.LocalWorkspaceFolder, true
	case "localWorkspaceFolderBasename":
		return base(sub.LocalWorkspaceFolder), true
	case "containerWorkspaceFolder":
		return sub.ContainerWorkspaceFolder, true
	case "containerWorkspaceFolderBasename":
		return base(sub.ContainerWorkspaceFolder), true
	case "devcontainerId":
		return sub.DevcontainerID, true
	}
	if rest, ok := strings.CutPrefix(name, "localEnv:"); ok {
		return envWithDefault(rest, sub.localEnvLookup), true
	}
	if rest, ok := strings.CutPrefix(name, "containerEnv:"); ok {
		return envWithDefault(rest, mapLookup(containerEnv)), true
	}
	return "", false
}

func (sub Substitutions) localEnvLookup(name string) (string, bool) {
	if sub.LocalEnv == nil {
		return "", false
	}
	return sub.LocalEnv(name)
}

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// envWithDefault resolves "NAME" or "NAME:fallback" against lookup.
func envWithDefault(spec string, lookup func(string) (string, bool)) string {
	name, fallback, hasFallback := strings.Cut(spec, ":")
	if v, ok := lookup(name); ok {
		return v
	}
	if hasFallback {
		return fallback
	}
	return ""
}

// base is path.Base but returns "" for "" (path.Base("") is ".").
func base(p string) string {
	if p == "" {
		return ""
	}
	return path.Base(p)
}
