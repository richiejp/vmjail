//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// devPtsTarget is the canonical mount-point for the devpts
// instance the agent allocates PTY pairs from. /dev/pts/ptmx is
// the multiplexer; per-slave nodes appear at /dev/pts/N.
const devPtsTarget = "/dev/pts"

// devPtsPtmx is the per-mount path to the PTY multiplexer. Using
// it (not /dev/ptmx) means the agent doesn't depend on whatever
// node — or absence of node — the initramfs left at /dev/ptmx.
const devPtsPtmx = "/dev/pts/ptmx"

// devPtsMountOpts follows the modern Linux convention:
//   - newinstance: an isolated pts namespace for this mount.
//   - ptmxmode=0666: the multiplexer is openable by any uid.
//   - gid=5,mode=0620: the conventional tty-group ownership.
//
// gid=5 matches the user-space "tty" group on most distros;
// when no such group exists in the agent's namespace the mount
// silently falls back to gid 0, which is what we want anyway.
const devPtsMountOpts = "newinstance,ptmxmode=0666,gid=5,mode=0620"

// devPtsDeps factors the side-effects of ensureDevPts so the
// happy + sad branches can be exercised in unit tests without
// actually mounting devpts on the test host.
type devPtsDeps struct {
	Stat     func(string) (fs.FileInfo, error)
	MkdirAll func(string, os.FileMode) error
	Mount    func(source, target, fstype string, flags uintptr, data string) error
}

// ensureDevPts makes /dev/pts/ptmx available to the agent. If
// it's already there (host with devtmpfs / a prior call) the
// function is a no-op. Otherwise it mkdir's /dev/pts and mounts
// devpts at it. Surfaced as a function so openPtyPair can call
// it lazily on first PTY allocation.
//
// The mount needs CAP_SYS_ADMIN; the agent has it (PID 1 in
// the microVM). Idempotent — a second mount returns EBUSY,
// which the Stat short-circuit above avoids triggering.
func ensureDevPts() error {
	return ensureDevPtsWith(devPtsDeps{
		Stat:     os.Stat,
		MkdirAll: os.MkdirAll,
		Mount:    unix.Mount,
	})
}

func ensureDevPtsWith(d devPtsDeps) error {
	if _, err := d.Stat(devPtsPtmx); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", devPtsPtmx, err)
	}
	if err := d.MkdirAll(devPtsTarget, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", devPtsTarget, err)
	}
	if err := d.Mount("devpts", devPtsTarget, "devpts",
		unix.MS_NOSUID|unix.MS_NOEXEC, devPtsMountOpts); err != nil {
		return fmt.Errorf("mount devpts at %s: %w", devPtsTarget, err)
	}
	return nil
}
