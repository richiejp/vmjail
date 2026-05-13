package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	overlayMergedTest = "/workspace"
	fstypeOverlay     = "overlay"
)

func TestParseWorkspaceOverlayCmdlinesHappyPath(t *testing.T) {
	got, err := ParseWorkspaceOverlayCmdlines(
		"console=hvc0 daft.workspace.overlay=/workspace daft.workspace.overlay=/data reboot=t",
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []string{overlayMergedTest, "/data"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseWorkspaceOverlayCmdlinesAbsent(t *testing.T) {
	got, err := ParseWorkspaceOverlayCmdlines("console=hvc0 reboot=t")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParseWorkspaceOverlayCmdlinesRejectsBadValues(t *testing.T) {
	cases := []struct {
		name      string
		cmdline   string
		errSubstr string
	}{
		{caseNameEmptyValue, "daft.workspace.overlay=", errSubEmptyValue},
		{"relative", "daft.workspace.overlay=workspace", errSubMustBeAbsolute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseWorkspaceOverlayCmdlines(tc.cmdline)
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("err = %v, want substring %q", err, tc.errSubstr)
			}
		})
	}
}

func TestFormatWorkspaceOverlayCmdlineRoundTrips(t *testing.T) {
	for _, mp := range []string{overlayMergedTest, "/mnt/x", "/data"} {
		s := FormatWorkspaceOverlayCmdline(mp)
		got, err := ParseWorkspaceOverlayCmdlines(s)
		if err != nil {
			t.Fatalf("round-trip %q via %q: %v", mp, s, err)
		}
		if len(got) != 1 || got[0] != mp {
			t.Errorf("round-trip %q: got %v", mp, got)
		}
	}
}

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

// TestApplySharesFromCmdlineRedirectsToLowerWhenOverlaid pins the
// agent-side handoff: when daft.workspace.overlay matches a
// daft.share's mountpoint, the share is mounted at <mp>.lower and
// an overlayfs follows at <mp>.
func TestApplySharesFromCmdlineRedirectsToLowerWhenOverlaid(t *testing.T) {
	f := newFakeShareDeps()
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "daft.share=workspace:" + overlayMergedTest + " daft.workspace.overlay=" + overlayMergedTest, nil
		},
		share: f.toShareDeps(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	// Expect: virtiofs mount at /workspace.lower, tmpfs at the
	// overlay root, overlayfs at /workspace.
	if len(f.mounts) != 3 {
		t.Fatalf("mounts = %+v, want 3 (virtiofs+tmpfs+overlay)", f.mounts)
	}
	if f.mounts[0].fstype != "virtiofs" || f.mounts[0].target != overlayMergedTest+".lower" {
		t.Errorf("share mount = %+v, want virtiofs at %q.lower", f.mounts[0], overlayMergedTest)
	}
	if f.mounts[2].fstype != fstypeOverlay || f.mounts[2].target != overlayMergedTest {
		t.Errorf("overlay mount = %+v, want overlay at %q", f.mounts[2], overlayMergedTest)
	}
}

// TestApplySharesFromCmdlineReportsOverlayCmdlineError ensures a
// malformed daft.workspace.overlay token surfaces on the console
// (without bringing the agent down or skipping the share mount).
func TestApplySharesFromCmdlineReportsOverlayCmdlineError(t *testing.T) {
	f := newFakeShareDeps()
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "daft.share=workspace:/workspace daft.workspace.overlay=relative", nil
		},
		share: f.toShareDeps(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if !strings.Contains(buf.String(), OverlayReportPrefix+"cmdline:") {
		t.Errorf("expected overlay cmdline error report, got: %q", buf.String())
	}
	// Share mount still happens against the raw mountpoint, since
	// we couldn't honour the overlay request — no overlayfs/tmpfs
	// should follow.
	if len(f.mounts) != 1 || f.mounts[0].target != "/workspace" {
		t.Errorf("mounts = %+v, want one virtiofs at /workspace", f.mounts)
	}
}

// TestApplySharesFromCmdlineReportsOverlayMountError pins the
// overlay: <err> console report path when MountOverlay fails after
// the share is in place. The same fake's mountErr fires for both
// share + overlay mounts; we still expect the overlay error to
// surface via the overlay reporter.
func TestApplySharesFromCmdlineReportsOverlayMountError(t *testing.T) {
	f := newFakeShareDeps()
	f.mountErr = errors.New("overlay mount bang")
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "daft.share=workspace:/workspace daft.workspace.overlay=/workspace", nil
		},
		share: f.toShareDeps(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if !strings.Contains(buf.String(), OverlayReportPrefix+"mount tmpfs: overlay mount bang") {
		t.Errorf("expected overlay mount error report, got: %q", buf.String())
	}
}

// FuzzParseWorkspaceOverlayCmdlines guards the cmdline parser
// against panics on arbitrary input. Same threat model as the
// share parser.
//
//	go test -run='^$' -fuzz=FuzzParseWorkspaceOverlayCmdlines -fuzztime=30s ./internal/agent
func FuzzParseWorkspaceOverlayCmdlines(f *testing.F) {
	seeds := []string{
		"",
		"daft.workspace.overlay=/workspace",
		"daft.workspace.overlay=/workspace daft.workspace.overlay=/data",
		"daft.workspace.overlay=",
		"daft.workspace.overlay=relative",
		"daft.workspace.overlay=/" + strings.Repeat("a", 4096),
		"daft.workspace.overlay=\x00",
		"daft.workspace.overlay=日本語",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = ParseWorkspaceOverlayCmdlines(raw)
	})
}
