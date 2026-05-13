package agent

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// WorkspaceOverlayCmdlineKey names the kernel cmdline parameter that
// asks the agent to wrap a virtio-fs share in a tmpfs-backed
// overlayfs. Format: `daft.workspace.overlay=<mountpoint>` where
// <mountpoint> is the in-guest path of the share to overlay (the
// merged dir the container sees). Multiple tokens are honoured —
// one per share to overlay.
//
// Effect: the named share is mounted at `<mountpoint>.lower` instead
// of `<mountpoint>`; a tmpfs at OverlayStoreRoot backs an overlayfs
// upper+work pair at `<OverlayStoreRoot>/<idx>/{upper,work}`; an
// overlayfs is mounted at `<mountpoint>` with the lower share as
// lowerdir. Container writes go to the tmpfs upper and are gone
// when the VM exits — the underlying share is never written to.
const WorkspaceOverlayCmdlineKey = "daft.workspace.overlay"

// OverlayStoreRoot is the in-guest tmpfs that backs every overlay's
// upper+work dirs. One tmpfs total; per-overlay subdirs disambiguate
// by index. Lives under /run so it's clearly volatile.
const OverlayStoreRoot = "/run/daft/overlay"

// OverlayReportPrefix tags console lines about overlay setup —
// cmdline parse errors, mount/mkdir failures. Same convention as
// ShareReportPrefix so the host scanner's regex doesn't have to
// special-case this stream.
const OverlayReportPrefix = "overlay: "

// FormatWorkspaceOverlayCmdline renders an in-guest mountpoint back
// to the wire form. Used by the sandbox-host when assembling the
// kernel cmdline.
func FormatWorkspaceOverlayCmdline(mountpoint string) string {
	return fmt.Sprintf("%s=%s", WorkspaceOverlayCmdlineKey, mountpoint)
}

// ParseWorkspaceOverlayCmdlines returns the merged mountpoints from
// every daft.workspace.overlay=... token. A malformed token aborts
// rather than being silently skipped — a stray bad token usually
// means the host wired the cmdline wrong, and continuing past it
// would mask the bug.
func ParseWorkspaceOverlayCmdlines(cmdline string) ([]string, error) {
	vals := findAllCmdlineValues(cmdline, WorkspaceOverlayCmdlineKey)
	for _, v := range vals {
		if err := validateAbsoluteCmdlinePath(WorkspaceOverlayCmdlineKey, v); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// OverlayLowerPath returns the path the share-mount step should
// redirect a to-be-overlaid share to. Centralised so the share-mount
// and overlay-mount halves stay in lockstep without an extra
// arg-passing dance.
func OverlayLowerPath(merged string) string {
	return merged + ".lower"
}

// OverlayTmpfsSize caps the upper-layer tmpfs to keep a runaway
// container write from OOM-ing a 256 MiB guest. 64 MiB is a
// conservative default; raise it on the kernel cmdline (future
// slice) if a workload needs more headroom.
const OverlayTmpfsSize = "64M"

// MountOverlay sets up an overlayfs at merged with the lower
// directory at OverlayLowerPath(merged) (where the share-mount step
// is expected to have already landed) and tmpfs-backed upper+work
// dirs under OverlayStoreRoot. The first call (idx == 0) mounts the
// shared tmpfs root; later calls reuse it.
func MountOverlay(idx int, merged string, deps ShareDeps) error {
	if idx == 0 {
		if err := deps.MkdirAll(OverlayStoreRoot, 0o755); err != nil {
			return fmt.Errorf("mkdir tmpfs root: %w", err)
		}
		if err := deps.Mount("tmpfs", OverlayStoreRoot, "tmpfs", "size="+OverlayTmpfsSize); err != nil {
			return fmt.Errorf("mount tmpfs: %w", err)
		}
	}
	base := filepath.Join(OverlayStoreRoot, strconv.Itoa(idx))
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	for _, d := range [...]string{upper, work, merged} {
		if err := deps.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		OverlayLowerPath(merged), upper, work)
	if err := deps.Mount("overlay", merged, "overlay", data); err != nil {
		return fmt.Errorf("mount overlay at %s: %w", merged, err)
	}
	return nil
}
