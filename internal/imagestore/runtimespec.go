package imagestore

import (
	"encoding/json"
	"fmt"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// runtimeSpec is the subset of the OCI runtime spec we generate. It
// is intentionally incomplete — only fields the agent's `crun run
// --no-pivot` cares about, mirroring contrib/hello-bundle/config.json.
//
// Marshalled as-is to config.json. JSON tags use the OCI spec field
// names so the result is a valid runtime-spec document.
type runtimeSpec struct {
	OCIVersion string             `json:"ociVersion"`
	Process    runtimeProcess     `json:"process"`
	Root       runtimeRoot        `json:"root"`
	Hostname   string             `json:"hostname"`
	Mounts     []runtimeMount     `json:"mounts"`
	Linux      runtimeLinuxConfig `json:"linux"`
}

type runtimeProcess struct {
	Terminal        bool                `json:"terminal"`
	User            runtimeUser         `json:"user"`
	Args            []string            `json:"args"`
	Env             []string            `json:"env,omitempty"`
	Cwd             string              `json:"cwd"`
	Capabilities    runtimeCapabilities `json:"capabilities"`
	Rlimits         []any               `json:"rlimits"`
	NoNewPrivileges bool                `json:"noNewPrivileges"`
}

type runtimeUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type runtimeCapabilities struct {
	Bounding  []string `json:"bounding"`
	Effective []string `json:"effective"`
	Permitted []string `json:"permitted"`
	Ambient   []string `json:"ambient"`
}

type runtimeRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type runtimeMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type runtimeLinuxConfig struct {
	Namespaces    []runtimeNamespace `json:"namespaces"`
	MaskedPaths   []string           `json:"maskedPaths"`
	ReadonlyPaths []string           `json:"readonlyPaths"`
}

type runtimeNamespace struct {
	Type string `json:"type"`
}

// buildRuntimeConfig produces a runtime spec for img with overrides
// from opts applied. The image's Entrypoint+Cmd, Env, and WorkingDir
// are honoured by default; opts.{Args,Env,Cwd,Hostname,ExtraMounts}
// replace / extend each.
func buildRuntimeConfig(img v1.Image, opts BundleOptions) (runtimeSpec, error) {
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return runtimeSpec{}, fmt.Errorf("imagestore: image config: %w", err)
	}
	imgCfg := cfgFile.Config

	args := mergeArgs(opts.Args, imgCfg.Entrypoint, imgCfg.Cmd)
	if len(args) == 0 {
		return runtimeSpec{}, fmt.Errorf(
			"imagestore: image has no entrypoint/cmd; supply BundleOptions.Args",
		)
	}

	mounts := defaultRuntimeMounts()
	for _, m := range opts.ExtraMounts {
		mounts = append(mounts, runtimeMount(m))
	}

	return runtimeSpec{
		OCIVersion: "1.0.2",
		Process: runtimeProcess{
			User:            runtimeUser{UID: 0, GID: 0},
			Args:            args,
			Env:             firstNonNil(opts.Env, imgCfg.Env),
			Cwd:             firstNonEmpty(opts.Cwd, imgCfg.WorkingDir, "/"),
			Capabilities:    emptyCapabilities(),
			Rlimits:         []any{},
			NoNewPrivileges: true,
		},
		Root:     runtimeRoot{Path: "rootfs", Readonly: true},
		Hostname: firstNonEmpty(opts.Hostname, "", "daft-container"),
		Mounts:   mounts,
		Linux:    defaultRuntimeLinuxConfig(),
	}, nil
}

// mergeArgs returns override unchanged when non-nil, otherwise
// concatenates entrypoint+cmd into a fresh slice (matching how the
// OCI runtime spec composes them).
func mergeArgs(override, entrypoint, cmd []string) []string {
	if override != nil {
		return override
	}
	out := make([]string, 0, len(entrypoint)+len(cmd))
	out = append(out, entrypoint...)
	out = append(out, cmd...)
	return out
}

func firstNonNil[T any](primary, fallback []T) []T {
	if primary != nil {
		return primary
	}
	return fallback
}

func firstNonEmpty(primary, secondary, fallback string) string {
	if primary != "" {
		return primary
	}
	if secondary != "" {
		return secondary
	}
	return fallback
}

func emptyCapabilities() runtimeCapabilities {
	return runtimeCapabilities{
		Bounding: []string{}, Effective: []string{},
		Permitted: []string{}, Ambient: []string{},
	}
}

func defaultRuntimeLinuxConfig() runtimeLinuxConfig {
	return runtimeLinuxConfig{
		Namespaces: []runtimeNamespace{
			{Type: "pid"},
			{Type: "ipc"},
			{Type: "uts"},
			{Type: "mount"},
			{Type: "network"},
			{Type: "cgroup"},
		},
		MaskedPaths: []string{
			"/proc/kcore", "/proc/keys", "/proc/latency_stats",
			"/proc/timer_list", "/proc/sched_debug",
		},
		ReadonlyPaths: []string{
			"/proc/asound", "/proc/bus", "/proc/fs",
			"/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
		},
	}
}

// defaultRuntimeMounts mirrors contrib/hello-bundle/config.json. Kept
// in code instead of an asset file because the runtime spec depends
// on per-call data (env, cwd) anyway, so the whole document is built
// programmatically.
func defaultRuntimeMounts() []runtimeMount {
	return []runtimeMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{
			Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
		},
		{
			Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{"nosuid", "noexec", "nodev", "ro"},
		},
	}
}

// writeJSON marshals v to path, atomically (write-and-rename).
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("imagestore: marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
