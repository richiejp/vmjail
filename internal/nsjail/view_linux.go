//go:build linux

package nsjail

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

//nolint:gochecknoglobals // Mount boundaries permit deterministic failure-path tests.
var (
	mountFS       = unix.Mount
	inspectMounts = readMounts
	setMountAttrs = unix.MountSetattr
)

type mountEntry struct{ path, kind string }

func readMounts() ([]mountEntry, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}

	return mountTable(string(raw))
}

func pseudo(kind string) bool {
	return slices.Contains([]string{
		fsProc, fsSysfs, "devtmpfs", fsDevpts, "cgroup", "cgroup2",
		"securityfs", "debugfs", "tracefs", "mqueue", "bpf", "configfs",
		"fusectl", "hugetlbfs", "nsfs", "autofs", "efivarfs", "rpc_pipefs",
		"binfmt_misc", "pstore", "selinuxfs", "smackfs", "resctrl",
	}, kind)
}

func mountTable(data string) ([]mountEntry, error) {
	var entries []mountEntry
	scan := bufio.NewScanner(strings.NewReader(data))
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

	for scan.Scan() {
		parts := strings.SplitN(scan.Text(), " - ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid mountinfo line")
		}

		fields := strings.Fields(parts[0])
		tail := strings.Fields(parts[1])
		if len(fields) < 5 || len(tail) < 1 {
			return nil, fmt.Errorf("incomplete mountinfo line")
		}

		path := unescape.Replace(fields[4])
		entries = append(entries, mountEntry{path, tail[0]})
	}

	slices.SortStableFunc(entries, func(a, b mountEntry) int { return len(a.path) - len(b.path) })
	return entries, scan.Err()
}

func beneath(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

func allowedSource(source string, table []mountEntry) bool {
	// The most specific mount determines the filesystem backing the source.
	kind := ""
	for _, m := range table {
		if beneath(source, m.path) {
			kind = m.kind
		}
	}

	return kind != "" && !pseudo(kind)
}

// filteredView pins ordinary submounts in the CLI's private mount namespace.
// New mounts on the host cannot propagate into the resulting export.
func filteredView(source, target string, table []mountEntry, readonly bool) error {
	if !allowedSource(source, table) {
		return fmt.Errorf("refusing pseudo-filesystem source %q", source)
	}

	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing special file %q", source)
	}

	if info.IsDir() {
		err = os.MkdirAll(target, 0o700)
	} else {
		var f *os.File
		// #nosec G304 -- target belongs to the private export staging directory.
		f, err = os.Create(target)
		if err == nil {
			err = f.Close()
		}
	}
	if err != nil {
		return err
	}

	if err = mountFS(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("clone %s: %w", source, err)
	}

	// Inspect the actual clone: a source path may have changed since planning.
	table, err = inspectMounts()
	if err != nil {
		return err
	}
	if !allowedSource(target, table) {
		return fmt.Errorf("refusing cloned pseudo-filesystem %q", target)
	}
	if info.IsDir() {
		if err := maskSubmounts(target, table); err != nil {
			return err
		}
	}

	return protectView(target, readonly)
}

func maskSubmounts(target string, table []mountEntry) error {
	var blocked []string
	for _, entry := range table {
		if !pseudo(entry.kind) || entry.path == target || !beneath(entry.path, target) {
			continue
		}
		if slices.ContainsFunc(blocked, func(path string) bool { return beneath(entry.path, path) }) {
			continue
		}

		masked, err := maskDirectory(entry.path, target)
		if err != nil {
			return fmt.Errorf("mask %s: %w", entry.path, err)
		}
		blocked = append(blocked, masked)
	}
	return nil
}

func protectView(target string, readonly bool) error {
	flags := uint64(unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOSUID)
	if readonly {
		flags |= unix.MOUNT_ATTR_RDONLY
	}

	if err := setMountAttrs(unix.AT_FDCWD, target, unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: flags}); err != nil {
		return fmt.Errorf("protect export %s: %w", target, err)
	}
	return nil
}

func maskDirectory(path, root string) (string, error) {
	for path != root {
		err := mountFS(fsTmpfs, path, fsTmpfs, unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID|unix.MS_NOEXEC, "mode=755,size=4k")
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, unix.EACCES) {
			return "", err
		}
		path = filepath.Dir(path)
	}
	return "", errors.New("cannot safely mask a pseudo-filesystem")
}
