package agent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

// testMountpoint and testTag are the in-VM share parameters reused
// across share tests. Promoted to constants to satisfy goconst —
// repeating the literals in every table row hides the fact that
// they're all the same value.
const (
	testMountpoint = "/share"
	testTag        = "workspace"
	testBundlePath = "/share/bundle"
)

// fakeShareDeps is a ShareDeps that never touches the real filesystem.
// Mount writes are recorded for assertions; the file table backs
// ReadFile so tests can pre-populate "host" content.
type fakeShareDeps struct {
	mounts   []mountCall
	files    map[string][]byte
	mountErr error
	mkdirErr error
	readErr  error
	madeDirs []string
}

type mountCall struct{ source, target, fstype, data string }

func newFakeShareDeps() *fakeShareDeps {
	return &fakeShareDeps{files: map[string][]byte{}}
}

func (f *fakeShareDeps) toShareDeps() ShareDeps {
	return ShareDeps{
		Mount: func(source, target, fstype, data string) error {
			f.mounts = append(f.mounts, mountCall{source, target, fstype, data})
			return f.mountErr
		},
		MkdirAll: func(path string, _ os.FileMode) error {
			f.madeDirs = append(f.madeDirs, path)
			return f.mkdirErr
		},
		ReadFile: func(path string) ([]byte, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			b, ok := f.files[path]
			if !ok {
				return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
			}
			return b, nil
		},
	}
}

func TestApplyShareHappyPath(t *testing.T) {
	spec := ShareSpec{Tag: testTag, Mountpoint: testMountpoint}
	f := newFakeShareDeps()
	f.files[filepath.Join(spec.Mountpoint, ShareMarkerFile)] = []byte("hello from host\n")

	var buf bytes.Buffer
	ApplyShare(spec, f.toShareDeps(), &buf)

	want := ShareReportPrefix + "hello from host\n"
	if buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
	if len(f.mounts) != 1 || f.mounts[0] != (mountCall{spec.Tag, spec.Mountpoint, "virtiofs", ""}) {
		t.Errorf("mounts = %+v, want one virtiofs mount of %q at %q", f.mounts, spec.Tag, spec.Mountpoint)
	}
	if len(f.madeDirs) != 1 || f.madeDirs[0] != spec.Mountpoint {
		t.Errorf("madeDirs = %v, want [%q]", f.madeDirs, spec.Mountpoint)
	}
}

func TestMountShareReadOnly(t *testing.T) {
	f := newFakeShareDeps()
	if err := MountShare(ShareSpec{Tag: testTag, Mountpoint: testMountpoint, ReadOnly: true}, f.toShareDeps()); err != nil {
		t.Fatal(err)
	}
	if len(f.mounts) != 1 || f.mounts[0].data != "ro" {
		t.Fatalf("mounts = %+v, want read-only option", f.mounts)
	}
}

func TestApplyShareReportsMkdirError(t *testing.T) {
	f := newFakeShareDeps()
	f.mkdirErr = errors.New("mkdir bang")

	var buf bytes.Buffer
	ApplyShare(ShareSpec{Tag: "ws", Mountpoint: testMountpoint}, f.toShareDeps(), &buf)

	if !strings.Contains(buf.String(), ShareReportPrefix+"mkdir: mkdir bang") {
		t.Errorf("expected mkdir error in output, got: %q", buf.String())
	}
	if len(f.mounts) != 0 {
		t.Errorf("mount must not be called after mkdir failure, got %+v", f.mounts)
	}
}

func TestApplyShareReportsMountError(t *testing.T) {
	f := newFakeShareDeps()
	f.mountErr = errors.New("mount bang")

	var buf bytes.Buffer
	ApplyShare(ShareSpec{Tag: "ws", Mountpoint: testMountpoint}, f.toShareDeps(), &buf)

	if !strings.Contains(buf.String(), ShareReportPrefix+"mount: mount bang") {
		t.Errorf("expected mount error in output, got: %q", buf.String())
	}
}

func TestApplyShareReportsReadError(t *testing.T) {
	f := newFakeShareDeps()
	// No file populated → ReadFile returns ErrNotExist via fakeShareDeps.

	var buf bytes.Buffer
	ApplyShare(ShareSpec{Tag: "ws", Mountpoint: testMountpoint}, f.toShareDeps(), &buf)

	if !strings.Contains(buf.String(), ShareReportPrefix+"read:") {
		t.Errorf("expected read error in output, got: %q", buf.String())
	}
}

func TestApplyShareTrimsTrailingNewlines(t *testing.T) {
	spec := ShareSpec{Tag: "t", Mountpoint: testMountpoint}
	f := newFakeShareDeps()
	f.files[filepath.Join(spec.Mountpoint, ShareMarkerFile)] = []byte("payload\r\n")

	var buf bytes.Buffer
	ApplyShare(spec, f.toShareDeps(), &buf)

	if got := buf.String(); got != ShareReportPrefix+"payload\n" {
		t.Errorf("trimmed output = %q, want %q", got, ShareReportPrefix+"payload\n")
	}
}

// TestRunWithConfigApplySharePath wires the runWith deps to a fake
// control channel carrying one share and a fake share, exercising the
// full config → mount → report path without any kernel involvement.
func TestRunWithConfigApplySharePath(t *testing.T) {
	fake := newFakeShareDeps()
	fake.files["/share/"+ShareMarkerFile] = []byte("payload-from-host")

	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{
		Shares: []*agentv1.GuestConfig_Share{{Tag: testTag, Mountpoint: "/share"}},
	}}
	deps := runDeps{
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
		share:       fake.toShareDeps(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if !strings.Contains(buf.String(), Banner) {
		t.Errorf("output missing banner: %q", buf.String())
	}
	if !strings.Contains(buf.String(), ShareReportPrefix+"payload-from-host") {
		t.Errorf("output missing share payload: %q", buf.String())
	}
}

func TestRunWithNoConfigSharesIsSilent(t *testing.T) {
	// No control channel (the vsock-unreachable degrade) means no
	// config and no shares — the agent simply skips sharing, emitting
	// no share line.
	deps := runDeps{
		share: newFakeShareDeps().toShareDeps(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if strings.Contains(buf.String(), ShareReportPrefix) {
		t.Errorf("unexpected share report when no config shares present: %q", buf.String())
	}
}
