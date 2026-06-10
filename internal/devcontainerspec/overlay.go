package devcontainerspec

import "github.com/localai-org/darkaifactory/internal/workspacecatalog"

// Overlay composes a base workspacecatalog.Spec (derived from a
// devcontainer.json) with the explicit workspace settings the user set
// directly, and returns the effective Spec. The override wins: a
// workspace setting always takes precedence over the devcontainer's, per
// the design that the devcontainer is a base layer the workspace config
// sits on top of.
//
// Per-field rules:
//   - Scalars (ImageRef, resource sizing, GPU, isolation, the build/
//     target identifiers): the override value when it is set (non-empty /
//     non-zero), otherwise the base value. A zero override means "unset"
//     and falls through to the base — there is no way in the catalog to
//     explicitly set a field back to zero to mask the base, which matches
//     how the field-setter RPCs work (you set a value or you don't).
//   - Repeated keyed collections (Mounts by Target, Env by Name, Ports by
//     ContainerPort+Protocol): a union, with the override entry winning on
//     a key collision. Base order is preserved; override-only entries are
//     appended in their own order. This lets a workspace re-point one
//     mount or override one env var without restating the whole set.
//   - Boolean grants (NestedVirt, PrivilegedFuseMounts, PrivilegedBuild): OR. These never
//     originate from a devcontainer (the base carries them false), so the
//     union degenerates to "the override decides" while still composing
//     sensibly if a future base sets one.
func Overlay(base, override workspacecatalog.Spec) workspacecatalog.Spec {
	out := base
	out.ImageRef = overrideString(base.ImageRef, override.ImageRef)
	out.PendingBuildID = overrideString(base.PendingBuildID, override.PendingBuildID)
	out.TargetSandboxFingerprint = overrideString(base.TargetSandboxFingerprint, override.TargetSandboxFingerprint)

	out.Mounts = overlayBy(base.Mounts, override.Mounts, func(m workspacecatalog.Mount) string { return m.Target })
	out.Env = overlayBy(base.Env, override.Env, func(e workspacecatalog.EnvVar) string { return e.Name })
	out.Ports = overlayBy(base.Ports, override.Ports, func(p workspacecatalog.PortMapping) portKey {
		return portKey{p.ContainerPort, p.Protocol}
	})

	out.MemoryBytes = overrideU64(base.MemoryBytes, override.MemoryBytes)
	out.CPUCores = overrideU32(base.CPUCores, override.CPUCores)
	out.GpuCount = overrideU32(base.GpuCount, override.GpuCount)
	out.GpuMinVRAM = overrideU64(base.GpuMinVRAM, override.GpuMinVRAM)
	out.Isolation = overrideI32(base.Isolation, override.Isolation)

	out.NestedVirt = base.NestedVirt || override.NestedVirt
	out.PrivilegedFuseMounts = base.PrivilegedFuseMounts || override.PrivilegedFuseMounts
	out.PrivilegedBuild = base.PrivilegedBuild || override.PrivilegedBuild
	return out
}

func overrideString(base, override string) string {
	if override != "" {
		return override
	}
	return base
}

func overrideU64(base, override uint64) uint64 {
	if override != 0 {
		return override
	}
	return base
}

func overrideU32(base, override uint32) uint32 {
	if override != 0 {
		return override
	}
	return base
}

func overrideI32(base, override int32) int32 {
	if override != 0 {
		return override
	}
	return base
}

// portKey is the union key for ports: the protocol is part of it so
// tcp/8080 and udp/8080 coexist rather than colliding.
type portKey struct {
	port  uint32
	proto int32
}

// overlayBy unions base and override by the key each element maps to,
// with the override winning on a collision. Base order is preserved and a
// colliding override replaces its base element in place; override-only
// elements are appended in their own order. The single keyed-union
// mechanism behind the Mounts/Env/Ports overlays.
func overlayBy[T any, K comparable](base, override []T, key func(T) K) []T {
	if len(override) == 0 {
		return base
	}
	idx := make(map[K]int, len(override))
	for i, e := range override {
		idx[key(e)] = i
	}
	used := make([]bool, len(override))
	out := make([]T, 0, len(base)+len(override))
	for _, e := range base {
		if i, ok := idx[key(e)]; ok {
			out = append(out, override[i])
			used[i] = true
			continue
		}
		out = append(out, e)
	}
	for i, e := range override {
		if !used[i] {
			out = append(out, e)
		}
	}
	return out
}
