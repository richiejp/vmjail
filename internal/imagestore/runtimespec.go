package imagestore

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

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
	UIDMappings   []runtimeIDMapping `json:"uidMappings,omitempty"`
	GIDMappings   []runtimeIDMapping `json:"gidMappings,omitempty"`
	MaskedPaths   []string           `json:"maskedPaths"`
	ReadonlyPaths []string           `json:"readonlyPaths"`
}

type runtimeNamespace struct {
	Type string `json:"type"`
}

type runtimeIDMapping struct {
	ContainerID int `json:"containerID"`
	HostID      int `json:"hostID"`
	Size        int `json:"size"`
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

	user, err := parseImageUser(imgCfg.User)
	if err != nil {
		return runtimeSpec{}, err
	}
	linux := defaultRuntimeLinuxConfig()
	applyUserNamespace(&linux, opts.HostUID, opts.HostGID)
	return runtimeSpec{
		OCIVersion: "1.0.2",
		Process: runtimeProcess{
			User:            user,
			Args:            args,
			Env:             firstNonNil(opts.Env, imgCfg.Env),
			Cwd:             firstNonEmpty(opts.Cwd, imgCfg.WorkingDir, "/"),
			Capabilities:    emptyCapabilities(),
			Rlimits:         []any{},
			NoNewPrivileges: true,
		},
		// Rootfs is mounted writable. v0 design puts the VM at
		// the security boundary, so a writable container rootfs is
		// fine. Load-bearing for nix: nix 2.34's LocalStore tries
		// to mount-remount /nix/store rw when uid 0 and the store
		// is ro (which it is when rootfs is ro). With empty Linux
		// capabilities the remount fails. Writable rootfs means
		// statvfs(/nix/store) reports rw and nix skips the remount.
		Root:     runtimeRoot{Path: "rootfs", Readonly: false},
		Hostname: firstNonEmpty(opts.Hostname, "", "daft-container"),
		Mounts:   mounts,
		Linux:    linux,
	}, nil
}

// applyUserNamespace appends a `user` namespace and a one-entry
// uid/gid map when the caller asked for one. See BundleOptions
// HostUID / HostGID for the why.
func applyUserNamespace(linux *runtimeLinuxConfig, hostUID, hostGID int) {
	if hostUID == 0 && hostGID == 0 {
		return
	}
	linux.Namespaces = append(linux.Namespaces, runtimeNamespace{Type: "user"})
	linux.UIDMappings = []runtimeIDMapping{
		{ContainerID: 0, HostID: hostUID, Size: 1},
	}
	linux.GIDMappings = []runtimeIDMapping{
		{ContainerID: 0, HostID: hostGID, Size: 1},
	}
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

// parseImageUser projects the OCI image-config `User` field onto
// the runtime spec's numeric uid/gid pair. Empty string defaults
// to 0:0 (root) which matches the historical pre-image behaviour.
// Only numeric uid[:gid] is supported — symbolic names would
// require /etc/passwd lookups inside the image, which we don't
// stage. Returns an error on a name-form User or a malformed
// integer; the bundler surfaces it before any disk write.
func parseImageUser(u string) (runtimeUser, error) {
	if u == "" {
		return runtimeUser{UID: 0, GID: 0}, nil
	}
	uidStr, gidStr, _ := strings.Cut(u, ":")
	uid, uerr := strconv.Atoi(uidStr)
	if uerr != nil {
		return runtimeUser{}, fmt.Errorf(
			"imagestore: image User %q: only numeric uid[:gid] supported", u,
		)
	}
	gid := 0
	if gidStr != "" {
		g, gerr := strconv.Atoi(gidStr)
		if gerr != nil {
			return runtimeUser{}, fmt.Errorf(
				"imagestore: image User %q gid not numeric", u,
			)
		}
		gid = g
	}
	return runtimeUser{UID: uid, GID: gid}, nil
}

func emptyCapabilities() runtimeCapabilities {
	return runtimeCapabilities{
		Bounding: []string{}, Effective: []string{},
		Permitted: []string{}, Ambient: []string{},
	}
}

func defaultRuntimeLinuxConfig() runtimeLinuxConfig {
	return runtimeLinuxConfig{
		// `network` is intentionally omitted: the v0 design is one
		// container per sandbox VM, and the VM is the security
		// boundary, so the container shares the VM's loopback
		// instead of getting its own netns. This is load-bearing
		// for `daft workspace forward`, where the agent dials the
		// container's port via 127.0.0.1 from inside the VM's
		// init namespace — a separate container netns would put
		// the container's listener out of reach of that dial.
		// Slice D5 (multi-container per VM) will need to revisit.
		Namespaces: []runtimeNamespace{
			{Type: "pid"},
			{Type: "ipc"},
			{Type: "uts"},
			{Type: "mount"},
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

// Mount-option keywords reused across the default mount set.
const (
	mountOptNosuid = "nosuid"
	mountOptNodev  = "nodev"
	mountTypeTmpfs = "tmpfs"
)

// defaultRuntimeMounts mirrors contrib/hello-bundle/config.json. Kept
// in code instead of an asset file because the runtime spec depends
// on per-call data (env, cwd) anyway, so the whole document is built
// programmatically.
//
// /tmp is mounted as a 256M tmpfs because most build tools (nix,
// gcc) need a writable scratch dir and the rootfs may be read-
// only depending on the OCI spec. Size capped so a runaway writer
// can't OOM the host.
func defaultRuntimeMounts() []runtimeMount {
	return []runtimeMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{
			Destination: "/dev", Type: mountTypeTmpfs, Source: mountTypeTmpfs,
			Options: []string{mountOptNosuid, "strictatime", "mode=755", "size=65536k"},
		},
		{
			Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{mountOptNosuid, "noexec", mountOptNodev, "ro"},
		},
		{
			Destination: "/tmp", Type: mountTypeTmpfs, Source: mountTypeTmpfs,
			Options: []string{mountOptNosuid, mountOptNodev, "mode=1777", "size=268435456"},
		},
		// /nix/store/.links is a tmpfs because nix's LocalStore
		// unconditionally mkdirs this on first run, and the
		// virtiofs-served /nix/store can't be modified by the
		// container (the daemon executes as its own host uid).
		// Empty tmpfs is a no-op for non-nix images.
		{
			Destination: "/nix/store/.links", Type: mountTypeTmpfs, Source: mountTypeTmpfs,
			Options: []string{mountOptNosuid, mountOptNodev, "mode=0755", "size=16777216"},
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
