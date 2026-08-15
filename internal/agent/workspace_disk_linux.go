//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
	"golang.org/x/sys/unix"
)

type workspaceDirectoryPair struct{ source, destination string }

func defaultWorkspaceDiskDeps() workspaceDiskDeps {
	return workspaceDiskDeps{
		readMagic: readWorkspaceMagic,
		mkdirAll:  os.MkdirAll,
		mountXFS: func(device, target string) error {
			return syscall.Mount(device, target, "xfs", syscall.MS_NODEV|syscall.MS_NOSUID, "prjquota")
		},
		bindMount: func(source, target string) error {
			return syscall.Mount(source, target, "", syscall.MS_BIND, "")
		},
		receiveSeed: workspacesnapshot.ReceiveSeed,
		run:         runWorkspaceDiskCommand,
		usage:       workspaceDiskUsage,
		capacity:    workspaceDiskCapacity,
		flush:       workspaceDiskFlush,
	}
}

func workspaceDiskFlush(path string) error {
	dir, err := os.Open(path) // #nosec G304 -- path is the fixed guest workspace mount.
	if err != nil {
		return err
	}
	flushErr := unix.Syncfs(int(dir.Fd()))
	closeErr := dir.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

func runWorkspaceDiskCommand(ctx context.Context, name string, args ...string) error {
	//daft:lint:disable=commandcontext reason="manual cancellation snapshots /proc before a bounded kill and wait"
	cmd := exec.Command(name, args...) // #nosec G204 -- executable and arguments are fixed guest policy.
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, output.Bytes())
		}
		return nil
	case <-ctx.Done():
		diagnostic := workspaceProcessDiagnostic(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return fmt.Errorf("%w\nprocess diagnostic (before kill):\n%s", ctx.Err(), diagnostic)
	}
}

func workspaceProcessDiagnostic(pid int) string {
	root := "/proc/" + strconv.Itoa(pid)
	var report strings.Builder
	fmt.Fprintf(&report, "pid=%d\n", pid)
	for _, name := range []string{"status", "wchan", "syscall", "stack"} {
		// #nosec G304 -- root is a numeric process ID and name comes from the fixed diagnostic allowlist above.
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			fmt.Fprintf(&report, "%s: %v\n", name, err)
			continue
		}
		if name == "status" {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "Name:") || strings.HasPrefix(line, "State:") || strings.HasPrefix(line, "PPid:") || strings.HasPrefix(line, "Threads:") {
					fmt.Fprintln(&report, line)
				}
			}
			continue
		}
		const diagnosticLimit = 2048
		if len(data) > diagnosticLimit {
			data = data[:diagnosticLimit]
		}
		fmt.Fprintf(&report, "%s: %s\n", name, strings.TrimSpace(string(data)))
	}
	return strings.TrimSpace(report.String())
}

func workspaceDiskCapacity(device string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(string(filepath.Separator), "sys", "class", "block", filepath.Base(device), "size"))
	if err != nil {
		return 0, fmt.Errorf("read block capacity: %w", err)
	}
	return parseWorkspaceDiskSectors(data)
}

func parseWorkspaceDiskSectors(data []byte) (uint64, error) {
	raw := strings.TrimSpace(string(data))
	sectors, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse block capacity %q: %w", raw, err)
	}
	if sectors > ^uint64(0)/512 {
		return 0, fmt.Errorf("block capacity %q overflows bytes", raw)
	}
	return sectors * 512, nil
}

func workspaceDiskUsage(path string) (workspacesnapshot.Usage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return workspacesnapshot.Usage{}, err
	}
	blockSize := uint64(stat.Bsize) // #nosec G115 -- Linux statfs block size is non-negative.
	total := stat.Blocks * blockSize
	available := stat.Bavail * blockSize
	free := stat.Bfree * blockSize
	return workspacesnapshot.Usage{Total: total, Used: total - free, Available: available}, nil
}

func copyWorkspaceTree(source, destination string) error {
	if err := copyWorkspaceRootMetadata(source, destination); err != nil {
		return err
	}
	pending := []workspaceDirectoryPair{{source: source, destination: destination}}
	for len(pending) > 0 {
		pair := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(pair.source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			src := pair.source + "/" + entry.Name()
			dst := pair.destination + "/" + entry.Name()
			info, err := os.Lstat(src)
			if err != nil {
				return err
			}
			switch {
			case info.IsDir():
				if err := os.Mkdir(dst, info.Mode().Perm()); err != nil && !os.IsExist(err) {
					return err
				}
				if err := chownWorkspaceEntry(dst, info, false); err != nil {
					return err
				}
				pending = append(pending, workspaceDirectoryPair{source: src, destination: dst})
			case info.Mode().IsRegular():
				if err := copyWorkspaceFile(src, dst, info); err != nil {
					return err
				}
			case info.Mode()&os.ModeSymlink != 0:
				if err := copyWorkspaceSymlink(src, dst, info); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported workspace seed entry %s (%s)", src, info.Mode())
			}
		}
	}
	return nil
}

func copyWorkspaceRootMetadata(source, destination string) error {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.Chmod(destination, rootInfo.Mode().Perm()); err != nil {
		return err
	}
	if err := chownWorkspaceEntry(destination, rootInfo, false); err != nil {
		return err
	}
	return nil
}

func copyWorkspaceFile(src, dst string, info os.FileInfo) error {
	input, err := os.Open(src) // #nosec G304 -- src is beneath the host-selected seed root.
	if err != nil {
		return err
	}
	output, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()) // #nosec G304,G306 -- destination and source mode are policy.
	if err != nil {
		_ = input.Close()
		return err
	}
	_, copyErr := io.Copy(output, input)
	inputCloseErr := input.Close()
	outputCloseErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if inputCloseErr != nil {
		return inputCloseErr
	}
	if outputCloseErr != nil {
		return outputCloseErr
	}
	return chownWorkspaceEntry(dst, info, false)
}

func copyWorkspaceSymlink(src, dst string, info os.FileInfo) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.Symlink(target, dst); err != nil {
		return err
	}
	return chownWorkspaceEntry(dst, info, true)
}

func chownWorkspaceEntry(path string, info os.FileInfo, symlink bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("workspace seed entry has no Linux stat metadata")
	}
	if symlink {
		return os.Lchown(path, int(stat.Uid), int(stat.Gid))
	}
	return os.Chown(path, int(stat.Uid), int(stat.Gid))
}

func readWorkspaceMagic(path string) ([4]byte, error) {
	var magic [4]byte
	file, err := os.Open(path) // #nosec G304 -- path is host-selected GuestConfig policy.
	if err != nil {
		return magic, err
	}
	_, readErr := file.Read(magic[:])
	closeErr := file.Close()
	if readErr != nil {
		return magic, readErr
	}
	return magic, closeErr
}
