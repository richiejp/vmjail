//go:build !linux

package agent

import "context"

func runXFSgrowTraced(ctx context.Context, run func(context.Context, string, ...string) error, target string) error {
	return run(ctx, "/bin/xfs_growfs", target)
}
