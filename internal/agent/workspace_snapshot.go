package agent

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

func startWorkspaceSnapshotListener(ctx context.Context, w io.Writer, cfg *agentv1.GuestConfig, deps workspaceDiskDeps) {
	disk := cfg.GetWorkspaceDisk()
	if disk == nil || len(disk.GetMounts()) == 0 {
		return
	}
	roots := make(map[string]string, len(disk.GetMounts()))
	for _, mount := range disk.GetMounts() {
		if mount != nil {
			roots[mount.GetDirectory()] = filepath.Join(workspaceDiskRoot, mount.GetDirectory())
		}
	}
	listener, err := vsockListener(workspacesnapshot.DefaultVsockPort)
	if err != nil {
		_, _ = fmt.Fprintf(w, "workspace-snapshot: listen: %v\n", err)
		return
	}
	go func() {
		control := workspaceSnapshotControl(w, deps, disk.GetDevice())
		if err := workspacesnapshot.Serve(ctx, listener, roots, control); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(w, "workspace-snapshot: serve: %v\n", err)
		}
	}()
}

func workspaceSnapshotControl(w io.Writer, deps workspaceDiskDeps, device string) workspacesnapshot.Control {
	return workspacesnapshot.Control{
		Usage:    func() (workspacesnapshot.Usage, error) { return deps.usage(workspaceDiskRoot) },
		Capacity: func() (uint64, error) { return deps.capacity(device) },
		Flush:    func() error { return deps.flush(workspaceDiskRoot) },
		Grow: func(requestCtx context.Context) error {
			err := runXFSgrowTraced(requestCtx, deps.run, workspaceDiskRoot)
			if err != nil {
				_, _ = fmt.Fprintf(w, "workspace-disk: XFS grow diagnostic: %v\n", err)
			}
			return err
		},
	}
}
