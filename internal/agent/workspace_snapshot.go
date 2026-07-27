package agent

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

func startWorkspaceSnapshotListener(ctx context.Context, w io.Writer, cfg *agentv1.GuestConfig) {
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
		if err := workspacesnapshot.Serve(ctx, listener, roots); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(w, "workspace-snapshot: serve: %v\n", err)
		}
	}()
}
