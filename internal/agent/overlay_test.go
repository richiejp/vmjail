package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
)

const (
	overlayMergedTest = "/workspace"
	fstypeOverlay     = "overlay"
	fstypeVirtiofs    = "virtiofs"
)

func TestOverlayLowerPath(t *testing.T) {
	if got := OverlayLowerPath(overlayMergedTest); got != "/workspace.lower" {
		t.Errorf("OverlayLowerPath(%q) = %q, want /workspace.lower", overlayMergedTest, got)
	}
}

func TestMountOverlayFirstMountsTmpfs(t *testing.T) {
	f := newFakeShareDeps()
	if err := MountOverlay(0, overlayMergedTest, f.toShareDeps()); err != nil {
		t.Fatalf("MountOverlay: %v", err)
	}
	if len(f.mounts) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(f.mounts), f.mounts)
	}
	if f.mounts[0].fstype != "tmpfs" || f.mounts[0].target != OverlayStoreRoot {
		t.Errorf("first mount = %+v, want tmpfs at %q", f.mounts[0], OverlayStoreRoot)
	}
	if f.mounts[0].data != "size="+OverlayTmpfsSize {
		t.Errorf("tmpfs data = %q, want size=%s", f.mounts[0].data, OverlayTmpfsSize)
	}
	if f.mounts[1].fstype != fstypeOverlay || f.mounts[1].target != overlayMergedTest {
		t.Errorf("second mount = %+v, want overlay at %q", f.mounts[1], overlayMergedTest)
	}
	wantData := fmt.Sprintf("lowerdir=%s.lower,upperdir=%s/0/upper,workdir=%s/0/work",
		overlayMergedTest, OverlayStoreRoot, OverlayStoreRoot)
	if f.mounts[1].data != wantData {
		t.Errorf("overlay data = %q, want %q", f.mounts[1].data, wantData)
	}
}

// TestMountOverlaySecondReusesTmpfs verifies idx > 0 skips the
// tmpfs mount so a second overlay shares the same tmpfs root.
func TestMountOverlaySecondReusesTmpfs(t *testing.T) {
	f := newFakeShareDeps()
	if err := MountOverlay(1, "/data", f.toShareDeps()); err != nil {
		t.Fatalf("MountOverlay: %v", err)
	}
	if len(f.mounts) != 1 {
		t.Fatalf("got %d mounts, want 1: %+v", len(f.mounts), f.mounts)
	}
	if f.mounts[0].fstype != fstypeOverlay {
		t.Errorf("only mount should be overlay, got %+v", f.mounts[0])
	}
}

func TestMountOverlayReportsMountError(t *testing.T) {
	f := newFakeShareDeps()
	f.mountErr = errors.New("mount bang")
	err := MountOverlay(0, overlayMergedTest, f.toShareDeps())
	if err == nil || !strings.Contains(err.Error(), "mount bang") {
		t.Errorf("err = %v, want it to wrap mount bang", err)
	}
}

func TestMountOverlayReportsMkdirError(t *testing.T) {
	f := newFakeShareDeps()
	f.mkdirErr = errors.New("mkdir bang")
	err := MountOverlay(0, overlayMergedTest, f.toShareDeps())
	if err == nil || !strings.Contains(err.Error(), "mkdir bang") {
		t.Errorf("err = %v, want it to wrap mkdir bang", err)
	}
}

// TestApplySharesFromConfigRedirectsToLowerWhenOverlaid pins the
// agent-side handoff: a GuestConfig share with its overlay bit set must
// drive the .lower + overlayfs handoff — the share is mounted at
// <mp>.lower and an overlayfs follows at <mp>.
func TestApplySharesFromConfigRedirectsToLowerWhenOverlaid(t *testing.T) {
	f := newFakeShareDeps()
	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{
		Shares: []*agentv1.GuestConfig_Share{
			{Tag: testTag, Mountpoint: overlayMergedTest, Overlay: true},
		},
	}}
	deps := runDeps{
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
		share:       f.toShareDeps(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if len(f.mounts) != 3 {
		t.Fatalf("mounts = %+v, want 3 (virtiofs+tmpfs+overlay)", f.mounts)
	}
	if f.mounts[0].fstype != fstypeVirtiofs || f.mounts[0].target != overlayMergedTest+".lower" {
		t.Errorf("share mount = %+v, want virtiofs at %q.lower", f.mounts[0], overlayMergedTest)
	}
	if f.mounts[2].fstype != fstypeOverlay || f.mounts[2].target != overlayMergedTest {
		t.Errorf("overlay mount = %+v, want overlay at %q", f.mounts[2], overlayMergedTest)
	}
}

// TestApplySharesFromConfigReportsOverlayMountError pins the
// overlay: <err> console report path when MountOverlay fails after
// the share is in place. The same fake's mountErr fires for both
// share + overlay mounts; we still expect the overlay error to
// surface via the overlay reporter.
func TestApplySharesFromConfigReportsOverlayMountError(t *testing.T) {
	f := newFakeShareDeps()
	f.mountErr = errors.New("overlay mount bang")
	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{
		Shares: []*agentv1.GuestConfig_Share{
			{Tag: "workspace", Mountpoint: "/workspace", Overlay: true},
		},
	}}
	deps := runDeps{
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
		share:       f.toShareDeps(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if !strings.Contains(buf.String(), OverlayReportPrefix+"mount tmpfs: overlay mount bang") {
		t.Errorf("expected overlay mount error report, got: %q", buf.String())
	}
}
