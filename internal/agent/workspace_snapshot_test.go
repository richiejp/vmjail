package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

type snapshotReport chan string

func (w snapshotReport) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func TestStartWorkspaceSnapshotListener(t *testing.T) {
	saved := vsockListener
	t.Cleanup(func() { vsockListener = saved })
	listener := newFakeVsockListener(workspacesnapshot.DefaultVsockPort)
	var port uint32
	vsockListener = func(got uint32) (net.Listener, error) {
		port = got
		return listener, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	startWorkspaceSnapshotListener(ctx, &bytes.Buffer{}, &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Directory: testWorkspaceDirectory}, nil},
	}}, defaultWorkspaceDiskDeps())
	if port != workspacesnapshot.DefaultVsockPort {
		t.Fatalf("listen port = %d", port)
	}
	cancel()
}

func TestStartWorkspaceSnapshotListenerDisabledAndListenError(t *testing.T) {
	saved := vsockListener
	t.Cleanup(func() { vsockListener = saved })
	calls := 0
	vsockListener = func(uint32) (net.Listener, error) {
		calls++
		return nil, errors.New("unavailable")
	}
	var report bytes.Buffer
	startWorkspaceSnapshotListener(t.Context(), &report, nil, workspaceDiskDeps{})
	startWorkspaceSnapshotListener(t.Context(), &report, &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Directory: testWorkspaceDirectory}},
	}}, defaultWorkspaceDiskDeps())
	if calls != 1 || !strings.Contains(report.String(), "unavailable") {
		t.Fatalf("calls=%d report=%q", calls, report.String())
	}
}

func TestWorkspaceSnapshotListenerReportsServeError(t *testing.T) {
	saved := vsockListener
	t.Cleanup(func() { vsockListener = saved })
	listener := newErrFakeListener(errors.New("accept failed"))
	vsockListener = func(uint32) (net.Listener, error) { return listener, nil }
	report := make(snapshotReport, 1)
	startWorkspaceSnapshotListener(t.Context(), report, &agentv1.GuestConfig{WorkspaceDisk: &agentv1.GuestConfig_WorkspaceDisk{
		Mounts: []*agentv1.GuestConfig_WorkspaceDisk_Mount{{Directory: testWorkspaceDirectory}},
	}}, defaultWorkspaceDiskDeps())
	if got := <-report; !strings.Contains(got, "accept failed") {
		t.Fatalf("report = %q", got)
	}
}

func TestWorkspaceSnapshotControl(t *testing.T) {
	want := workspacesnapshot.Usage{Total: 10, Used: 8, Available: 2}
	wantCapacity := uint64(4096)
	var report bytes.Buffer
	control := workspaceSnapshotControl(&report, workspaceDiskDeps{
		usage:    func(string) (workspacesnapshot.Usage, error) { return want, nil },
		capacity: func(string) (uint64, error) { return wantCapacity, nil },
		run:      func(context.Context, string, ...string) error { return errors.New("grow failure") },
	}, "/dev/vda")
	if got, err := control.Usage(); err != nil || got != want {
		t.Fatalf("usage = %#v, %v", got, err)
	}
	if got, err := control.Capacity(); err != nil || got != wantCapacity {
		t.Fatalf("capacity = %d, %v", got, err)
	}
	if err := control.Grow(t.Context()); err == nil || !strings.Contains(report.String(), "XFS grow diagnostic") {
		t.Fatalf("grow = %v, report = %q", err, report.String())
	}
}
