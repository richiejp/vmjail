package localrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/richiejp/vmjail/internal/ocibundle"
)

type bind struct {
	source, target string
	readonly       bool
}

const (
	unixNetwork   = "unix"
	commandBundle = "/command"
	hostRootMount = "/hostroot"

	mountBind   = "bind"
	mountNoDev  = "nodev"
	mountNoExec = "noexec"
	mountNoSuid = "nosuid"
	fsProc      = "proc"
	fsTmpfs     = "tmpfs"
	fsSysfs     = "sysfs"
	fsDevpts    = "devpts"
	modeSticky  = "mode=1777"
)

func parseBind(value string, readonly bool) (bind, error) {
	src, dst, ok := strings.Cut(value, ":")
	if !ok {
		dst = src
	}
	if !filepath.IsAbs(src) || !filepath.IsAbs(dst) {
		return bind{}, fmt.Errorf("mount requires absolute SRC[:DST]: %q", value)
	}

	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		return bind{}, err
	}

	dst = filepath.Clean(dst)
	if dst == "/" {
		return bind{}, fmt.Errorf("use --chroot for the root filesystem")
	}
	for _, reserved := range []string{"/proc", "/sys", "/dev"} {
		if beneath(dst, reserved) {
			return bind{}, fmt.Errorf("guest owns %s; host device and pseudo-filesystem mounts are unsupported", reserved)
		}
	}

	return bind{source: src, target: dst, readonly: readonly}, nil
}

func writeSpec(path string, o Options, mounts []ocibundle.Mount) error {
	limits := []ocibundle.Rlimit{}
	for _, limit := range []struct {
		name  string
		value uint64
	}{
		{"RLIMIT_AS", o.AddressSpace * 1024 * 1024},
		{"RLIMIT_CPU", o.CPUTime},
		{"RLIMIT_NOFILE", o.Files},
	} {
		if limit.value > 0 {
			limits = append(limits, ocibundle.Rlimit{Type: limit.name, Soft: limit.value, Hard: limit.value})
		}
	}

	spec := ocibundle.Spec{
		OCIVersion: ocibundle.Version,
		Root:       ocibundle.Root{Path: hostRootMount, Readonly: true},
		Hostname:   o.Hostname,
		Process: ocibundle.Process{
			User:            ocibundle.User{UID: int(o.User), GID: int(o.Group)},
			Args:            o.Command,
			Env:             environment(o),
			Cwd:             o.Cwd,
			NoNewPrivileges: true,
			Rlimits:         limits,
		},
		Linux: ocibundle.Linux{
			Namespaces: []ocibundle.Namespace{{Type: "mount"}, {Type: "pid"}, {Type: "ipc"}, {Type: "uts"}},
		},
		Mounts: mounts,
	}

	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	return os.WriteFile(path, raw, 0o600)
}

func guestMounts() []ocibundle.Mount {
	return []ocibundle.Mount{
		{Destination: "/proc", Type: fsProc, Source: fsProc, Options: []string{mountNoSuid, mountNoDev, mountNoExec}},
		{Destination: "/sys", Type: fsSysfs, Source: fsSysfs, Options: []string{"ro", mountNoSuid, mountNoDev, mountNoExec}},
		{Destination: "/dev", Type: fsTmpfs, Source: fsTmpfs, Options: []string{mountNoSuid, mountNoExec, "mode=755", "size=16m"}},
		{Destination: "/dev/pts", Type: fsDevpts, Source: fsDevpts, Options: []string{mountNoSuid, mountNoExec, "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: fsTmpfs, Source: fsTmpfs, Options: []string{mountNoSuid, mountNoDev, modeSticky, "size=64m"}},
		{Destination: "/tmp", Type: fsTmpfs, Source: fsTmpfs, Options: []string{mountNoSuid, mountNoDev, modeSticky, "size=128m"}},
	}
}
