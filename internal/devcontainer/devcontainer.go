// Package devcontainer parses a devcontainer.json (the containers.dev
// "development container" spec) into a small, daft-shaped intermediate
// representation (Spec).
//
// It is deliberately a *consumer* of the spec, not a full
// implementation: daft already owns the runtime half (virtio-fs shares,
// the image store, uid translation, port forwards, in-container exec),
// so this package resolves only the fields that map onto that machinery
// — image/build, containerEnv, remoteUser, the workspace mount,
// additional mounts, forwardPorts, the lifecycle hooks, and features.
// Editor-only keys (customizations, settings, extensions) and the rarely
// used knobs (userEnvProbe, waitFor, parallel-by-default semantics) are
// ignored on purpose; see PLAN.md for the field-frequency survey that
// drove the cut.
//
// Parsing is split from resolution: Parse turns jsonc bytes into the raw
// Config (handling the spec's several polymorphic fields), and
// Config.Resolve applies ${…} variable substitution, normalises the
// lifecycle commands, and validates the image-vs-build choice to produce
// a Spec. Resolve is the convenience entry point that does both.
package devcontainer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Config is the raw, decoded devcontainer.json — the subset of fields
// daft consumes, with the spec's polymorphic shapes captured by the
// custom types below. Resolve turns it into a Spec.
type Config struct {
	Name string `json:"name"`

	// Exactly one of Image, Build (or the legacy top-level DockerFile),
	// or DockerComposeFile selects how the container is created. Resolve
	// enforces that and rejects compose (unsupported).
	Image             string        `json:"image"`
	Build             *Build        `json:"build"`
	DockerFile        string        `json:"dockerFile"` // legacy: build via top-level dockerFile+context
	Context           string        `json:"context"`    // legacy companion to DockerFile
	DockerComposeFile StringOrSlice `json:"dockerComposeFile"`

	ContainerEnv    map[string]string `json:"containerEnv"`
	RemoteUser      string            `json:"remoteUser"`
	ContainerUser   string            `json:"containerUser"`
	OverrideCommand *bool             `json:"overrideCommand"`

	WorkspaceFolder string  `json:"workspaceFolder"`
	WorkspaceMount  string  `json:"workspaceMount"`
	Mounts          []Mount `json:"mounts"`

	ForwardPorts []Port `json:"forwardPorts"`

	Features map[string]json.RawMessage `json:"features"`

	InitializeCommand    LifecycleCommand `json:"initializeCommand"`
	OnCreateCommand      LifecycleCommand `json:"onCreateCommand"`
	UpdateContentCommand LifecycleCommand `json:"updateContentCommand"`
	PostCreateCommand    LifecycleCommand `json:"postCreateCommand"`
	PostStartCommand     LifecycleCommand `json:"postStartCommand"`
	PostAttachCommand    LifecycleCommand `json:"postAttachCommand"`
}

// Build is the image-from-Dockerfile stanza. Args values are coerced to
// strings (the spec allows string, number, or bool); a null arg becomes
// the empty string.
type Build struct {
	Dockerfile string    `json:"dockerfile"`
	Context    string    `json:"context"`
	Args       ScalarMap `json:"args"`
	Target     string    `json:"target"`
	Options    []string  `json:"options"`
}

// Parse decodes jsonc bytes into a Config. Comments and trailing commas
// are stripped first (see stripJSONC) since encoding/json rejects both.
// Unknown keys are ignored by design — the spec has ~40 fields and daft
// consumes a deliberate subset.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(stripJSONC(data), &c); err != nil {
		return nil, fmt.Errorf("devcontainer: parse: %w", err)
	}
	return &c, nil
}

// StringOrSlice decodes a JSON value that may be either a single string
// or an array of strings (e.g. dockerComposeFile).
type StringOrSlice []string

// UnmarshalJSON accepts "x" or ["x","y"].
func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = StringOrSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("devcontainer: expected string or []string, got %s", data)
	}
	*s = many
	return nil
}

// ScalarMap decodes an object whose values are scalars (string, number,
// bool, or null) into a map[string]string, coercing each to its string
// form. Used for build.args, where tooling commonly writes "0" as 0.
type ScalarMap map[string]string

// UnmarshalJSON coerces each scalar value to a string; null becomes "".
func (m *ScalarMap) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("devcontainer: args must be an object: %w", err)
	}
	out := make(ScalarMap, len(raw))
	for k, v := range raw {
		s, err := scalarToString(v)
		if err != nil {
			return fmt.Errorf("devcontainer: arg %q: %w", k, err)
		}
		out[k] = s
	}
	*m = out
	return nil
}

func scalarToString(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		// JSON numbers decode as float64; render integers without a
		// trailing ".0" so BUILDKIT_INLINE_CACHE: 0 stays "0".
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported scalar %T", v)
	}
}

// Port is a single forwardPorts entry, which the spec allows as either
// an integer (8080) or a string ("8080" or "host:8080"). Only the
// container-side port number is retained — daft forwards by port.
type Port int

// UnmarshalJSON accepts 8080, "8080", or "127.0.0.1:8080".
func (p *Port) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*p = Port(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("devcontainer: port must be int or string, got %s", data)
	}
	// "host:port" → keep the last colon-separated field.
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("devcontainer: port %q is not a number", s)
	}
	*p = Port(n)
	return nil
}

// Mount is one additional bind/volume mount. The spec allows either the
// docker-CLI --mount string ("source=…,target=…,type=bind") or an
// object with the same keys; both decode here.
type Mount struct {
	Source   string
	Target   string
	Type     string
	ReadOnly bool
}

// UnmarshalJSON accepts the --mount string form or the object form.
func (m *Mount) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("devcontainer: mount string: %w", err)
		}
		return m.parseMountString(s)
	}
	// Object form. Alias the field set so we don't recurse into this
	// UnmarshalJSON, and accept the common key spellings.
	var obj struct {
		Source      string `json:"source"`
		Src         string `json:"src"`
		Target      string `json:"target"`
		Destination string `json:"destination"`
		Dst         string `json:"dst"`
		Type        string `json:"type"`
		ReadOnly    any    `json:"readonly"`
		RO          any    `json:"ro"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("devcontainer: mount object: %w", err)
	}
	m.Source = firstNonEmpty(obj.Source, obj.Src)
	m.Target = firstNonEmpty(obj.Target, obj.Destination, obj.Dst)
	m.Type = obj.Type
	m.ReadOnly = truthy(obj.ReadOnly) || truthy(obj.RO)
	return nil
}

// parseMountString parses "source=…,target=…,type=bind,readonly" into m.
func (m *Mount) parseMountString(s string) error {
	for field := range strings.SplitSeq(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, val, hasVal := strings.Cut(field, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "source", "src":
			m.Source = val
		case "target", "destination", "dst":
			m.Target = val
		case "type":
			m.Type = val
		case "readonly", "ro":
			// "readonly" alone (no =value) means true.
			m.ReadOnly = !hasVal || truthy(val)
		}
	}
	return nil
}

// LifecycleCommand is one lifecycle hook value. The spec allows three
// shapes: a shell string ("npm install"), an argv array
// (["npm","install"], run without a shell), or an object of named
// commands run in parallel ({"a":"…","b":["…"]}). All normalise to a
// list of entries here.
type LifecycleCommand struct {
	Entries []CommandEntry
}

// CommandEntry is a single resolved command. Exactly one of Shell or
// Argv is meaningful: Shell holds the raw shell-string form (run via a
// shell), Argv holds the exec-array form (run directly). Name is set
// only for the object form.
type CommandEntry struct {
	Name  string
	Shell string
	Argv  []string
}

// IsZero reports whether the hook was absent.
func (l LifecycleCommand) IsZero() bool { return len(l.Entries) == 0 }

// UnmarshalJSON decodes the string / array / object forms.
func (l *LifecycleCommand) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "null":
		return nil
	case strings.HasPrefix(trimmed, "{"):
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			return fmt.Errorf("devcontainer: command object: %w", err)
		}
		// Map iteration is unordered; sort by name so resolution is
		// deterministic (the named commands run in parallel anyway, so
		// order is presentation-only).
		names := make([]string, 0, len(obj))
		for name := range obj {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			e, err := decodeCommandEntry(obj[name])
			if err != nil {
				return fmt.Errorf("devcontainer: command %q: %w", name, err)
			}
			e.Name = name
			l.Entries = append(l.Entries, e)
		}
		return nil
	default:
		e, err := decodeCommandEntry(data)
		if err != nil {
			return err
		}
		l.Entries = []CommandEntry{e}
		return nil
	}
}

// decodeCommandEntry decodes a single command value: a string (shell
// form) or an array (argv form).
func decodeCommandEntry(data []byte) (CommandEntry, error) {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return CommandEntry{Shell: s}, nil
	}
	var argv []string
	if err := json.Unmarshal(data, &argv); err != nil {
		return CommandEntry{}, fmt.Errorf("devcontainer: command must be string or []string, got %s", data)
	}
	return CommandEntry{Argv: argv}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// truthy interprets the loosely-typed readonly/ro values the --mount
// syntax and object form both allow: bool true, or the strings
// "true"/"1"/"" (a bare "readonly" flag).
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "":
			return true
		}
	}
	return false
}
