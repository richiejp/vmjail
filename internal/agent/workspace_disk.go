package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

const (
	workspaceDiskRoot = "/run/daft/workspace-disk"
	xfsMagicString    = "XFSB"
)

type workspaceDiskDeps struct {
	readMagic func(string) ([4]byte, error)
	mkdirAll  func(string, os.FileMode) error
	mountXFS  func(string, string) error
	bindMount func(string, string) error
	copyTree  func(string, string) error
	run       func(string, ...string) error
}

func applyWorkspaceDisk(w io.Writer, deps workspaceDiskDeps, cfg *agentv1.GuestConfig) error {
	disk := cfg.GetWorkspaceDisk()
	if disk == nil {
		return nil
	}
	if disk.GetDevice() == "" || !filepath.IsAbs(disk.GetDevice()) {
		return errors.New("agent: workspace disk device must be absolute")
	}
	magic, err := deps.readMagic(disk.GetDevice())
	if err != nil {
		return fmt.Errorf("agent: read workspace disk: %w", err)
	}
	initialize := magic == [4]byte{}
	if initialize {
		if err := deps.run("/bin/mkfs.xfs", "-f", "-m", "reflink=1", disk.GetDevice()); err != nil {
			return fmt.Errorf("agent: format workspace XFS: %w", err)
		}
	} else if string(magic[:]) != xfsMagicString {
		return fmt.Errorf("agent: workspace disk has unknown filesystem magic %x", magic)
	}
	if err := deps.mkdirAll(workspaceDiskRoot, 0o700); err != nil {
		return fmt.Errorf("agent: create workspace disk root: %w", err)
	}
	if err := deps.mountXFS(disk.GetDevice(), workspaceDiskRoot); err != nil {
		if string(magic[:]) != xfsMagicString {
			return fmt.Errorf("agent: mount new workspace XFS: %w", err)
		}
		if repairErr := deps.run("/bin/xfs_repair", disk.GetDevice()); repairErr != nil {
			return fmt.Errorf("agent: repair workspace XFS after mount failure: %w", repairErr)
		}
		if err := deps.mountXFS(disk.GetDevice(), workspaceDiskRoot); err != nil {
			return fmt.Errorf("agent: mount repaired workspace XFS: %w", err)
		}
	}
	for _, mount := range disk.GetMounts() {
		if err := applyWorkspaceMount(deps, mount, initialize); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(w, "workspace-disk: mounted %s with %d mappings\n", disk.GetDevice(), len(disk.GetMounts()))
	return nil
}

func applyWorkspaceMount(deps workspaceDiskDeps, mount *agentv1.GuestConfig_WorkspaceDisk_Mount, initialize bool) error {
	if mount == nil || !filepath.IsAbs(mount.GetTarget()) {
		return errors.New("agent: workspace mount target must be absolute")
	}
	directory := filepath.Clean(mount.GetDirectory())
	if directory == "." || filepath.IsAbs(directory) || directory == ".." || strings.HasPrefix(directory, ".."+string(filepath.Separator)) {
		return fmt.Errorf("agent: unsafe workspace disk directory %q", mount.GetDirectory())
	}
	source := filepath.Join(workspaceDiskRoot, directory)
	if err := deps.mkdirAll(source, 0o700); err != nil {
		return fmt.Errorf("agent: create workspace source: %w", err)
	}
	if initialize && mount.GetSeed() != "" {
		if !filepath.IsAbs(mount.GetSeed()) {
			return errors.New("agent: workspace seed must be absolute")
		}
		if err := deps.copyTree(mount.GetSeed(), source); err != nil {
			return fmt.Errorf("agent: seed workspace mount: %w", err)
		}
	}
	if err := deps.mkdirAll(mount.GetTarget(), 0o700); err != nil {
		return fmt.Errorf("agent: create workspace target: %w", err)
	}
	if err := deps.bindMount(source, mount.GetTarget()); err != nil {
		return fmt.Errorf("agent: bind workspace mount: %w", err)
	}
	return nil
}
