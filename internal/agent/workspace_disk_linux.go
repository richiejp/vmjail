//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
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
		copyTree: copyWorkspaceTree,
		run: func(name string, args ...string) error {
			output, err := exec.Command(name, args...).CombinedOutput() // #nosec G204 -- executable and arguments are fixed guest policy.
			if err != nil {
				return fmt.Errorf("%w: %s", err, output)
			}
			return nil
		},
	}
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
