//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	kernelTraceInstance = "daft-xfs-grow"
	kernelTraceLimit    = 2 << 10
	xfsCommitEvent      = "xfs/xfs_trans_commit"
)

//nolint:gochecknoglobals // Serializes the one named trace instance shared by concurrent control requests.
var kernelTraceMu sync.Mutex

// xfsGrowTraceEvents is deliberately an allowlist. It captures the filesystem
// transaction, its metadata buffers, the underlying block requests, and the
// ioctl boundary without granting a caller arbitrary tracefs control. Broad
// scheduler events are excluded because they overwrite useful storage events.
//
//nolint:gochecknoglobals // Immutable allowlist defining the named diagnostic profile.
var xfsGrowTraceEvents = []string{
	"xfs/xfs_trans_alloc", xfsCommitEvent,
	"xfs/xfs_buf_read", "xfs/xfs_buf_submit", "xfs/xfs_buf_iodone",
	"xfs/xfs_buf_iowait", "xfs/xfs_buf_iowait_done",
	"block/block_bio_queue", "block/block_rq_issue", "block/block_rq_complete",
	"syscalls/sys_enter_ioctl", "syscalls/sys_exit_ioctl",
}

// runXFSgrowTraced records a bounded, isolated tracefs instance around the
// guest-kernel grow. The grow remains authoritative if tracing is unavailable.
func runXFSgrowTraced(ctx context.Context, run func(context.Context, string, ...string) error, target string) error {
	return runXFSgrowTracedWith(ctx, run, target, func() (*kernelTrace, error) {
		return startKernelTrace(tracefsMountpoint, kernelTraceInstance, xfsGrowTraceEvents)
	})
}

func runXFSgrowTracedWith(ctx context.Context, run func(context.Context, string, ...string) error, target string, start func() (*kernelTrace, error)) error {
	kernelTraceMu.Lock()
	defer kernelTraceMu.Unlock()

	trace, traceErr := start()
	done := make(chan error, 1)
	go func() { done <- run(ctx, "/bin/xfs_growfs", target) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if traceErr != nil {
		if err != nil {
			return errors.Join(err, fmt.Errorf("kernel trace unavailable: %w", traceErr))
		}
		return nil
	}
	report, stopErr := trace.stop()
	if err == nil {
		return stopErr
	}
	if stopErr != nil {
		return errors.Join(err, fmt.Errorf("kernel trace collection failed: %w", stopErr))
	}
	return fmt.Errorf("%w\nkernel trace profile xfs-grow (bounded):\n%s", err, report)
}

type kernelTrace struct{ root string }

func startKernelTrace(root, name string, events []string) (*kernelTrace, error) {
	return startKernelTraceAt(root, name, events, ensureTracefs)
}

func startKernelTraceAt(root, name string, events []string, mount func() error) (*kernelTrace, error) {
	if err := mount(); err != nil {
		return nil, err
	}
	instance := filepath.Join(root, "instances", name)
	if err := os.Mkdir(instance, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create trace instance: %w", err)
	}
	if err := os.WriteFile(filepath.Join(instance, "buffer_size_kb"), []byte("256\n"), 0o600); err != nil {
		return nil, fmt.Errorf("bound trace buffer: %w", err)
	}
	enabled := 0
	for _, event := range events {
		path := filepath.Join(instance, "events", event, "enable")
		if err := os.WriteFile(path, []byte("1\n"), 0o600); err == nil {
			enabled++
		}
	}
	if enabled == 0 {
		return nil, errors.New("no events from trace profile are available")
	}
	if err := os.WriteFile(filepath.Join(instance, "trace"), nil, 0o600); err != nil {
		return nil, fmt.Errorf("clear trace: %w", err)
	}
	if err := os.WriteFile(filepath.Join(instance, "tracing_on"), []byte("1\n"), 0o600); err != nil {
		return nil, fmt.Errorf("enable trace: %w", err)
	}
	return &kernelTrace{root: instance}, nil
}

func (t *kernelTrace) stop() (string, error) {
	if err := os.WriteFile(filepath.Join(t.root, "tracing_on"), []byte("0\n"), 0o600); err != nil {
		return "", fmt.Errorf("disable trace: %w", err)
	}
	f, err := os.Open(filepath.Join(t.root, "trace"))
	if err != nil {
		return "", fmt.Errorf("open trace: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, kernelTraceLimit+1))
	if err != nil {
		return "", fmt.Errorf("read trace: %w", err)
	}
	if len(data) > kernelTraceLimit {
		data = append(data[:kernelTraceLimit], []byte("\n[trace truncated]\n")...)
	}
	return strings.TrimSpace(string(data)), nil
}
