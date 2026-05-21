//go:build linux

package daftfuse

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
)

// White-box tests for the FUSE adapter's pure helpers. These run
// without an actual mount so they're fast and reach branches that
// the real-mount tests can't easily trigger (Getattr is normally
// answered from the Lookup-populated attr cache; mapReadErr's EIO
// fallback is asymmetric to surface from userspace).

func TestFillAttr(t *testing.T) {
	t.Run("file_with_mtime", func(t *testing.T) {
		n := &node{
			kind: workspacev1.EntryKind_ENTRY_KIND_FILE, mode: 0o644, size: 42,
			mtime: time.Unix(1700000000, 7),
		}
		var out fuse.Attr
		fillAttr(&out, n)
		if out.Size != 42 || out.Mtime != 1700000000 || out.Mtimensec != 7 {
			t.Errorf("got %+v", out)
		}
		if out.Mode&syscall.S_IFREG == 0 {
			t.Errorf("mode missing S_IFREG: %o", out.Mode)
		}
	})
	t.Run("file_no_mtime", func(t *testing.T) {
		n := &node{kind: workspacev1.EntryKind_ENTRY_KIND_FILE, mode: 0o644}
		// time.Time zero value triggers the IsZero short-circuit
		// so out.Mtime stays zero.
		n.mtime = time.Time{}
		var out fuse.Attr
		fillAttr(&out, n)
		if out.Mtime != 0 || out.Mtimensec != 0 {
			t.Errorf("mtime should be unset: %+v", out)
		}
	})
}

func TestFuseNodeGetattr(t *testing.T) {
	f := &fuseNode{node: &node{
		kind: workspacev1.EntryKind_ENTRY_KIND_FILE, mode: 0o755, size: 99,
		mtime: time.Unix(1, 0),
	}}
	var out fuse.AttrOut
	if errno := f.Getattr(t.Context(), nil, &out); errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	if out.Size != 99 {
		t.Errorf("size = %d", out.Size)
	}
}

func TestFuseNodeReadlinkNonSymlink(t *testing.T) {
	f := &fuseNode{node: &node{kind: workspacev1.EntryKind_ENTRY_KIND_FILE}}
	if _, errno := f.Readlink(t.Context()); errno != syscall.EINVAL {
		t.Errorf("errno = %v, want EINVAL", errno)
	}
}

func TestFuseNodeReadNonFile(t *testing.T) {
	f := &fuseNode{node: &node{kind: workspacev1.EntryKind_ENTRY_KIND_DIR}}
	if _, errno := f.Read(t.Context(), nil, nil, 0); errno != syscall.EISDIR {
		t.Errorf("errno = %v, want EISDIR", errno)
	}
}

func TestModeBits(t *testing.T) {
	cases := []struct {
		kind workspacev1.EntryKind
		want uint32
	}{
		{workspacev1.EntryKind_ENTRY_KIND_DIR, syscall.S_IFDIR},
		{workspacev1.EntryKind_ENTRY_KIND_SYMLINK, syscall.S_IFLNK},
		{workspacev1.EntryKind_ENTRY_KIND_FILE, syscall.S_IFREG},
		{workspacev1.EntryKind_ENTRY_KIND_UNSPECIFIED, syscall.S_IFREG},
	}
	for _, tc := range cases {
		if got := modeBits(tc.kind); got != tc.want {
			t.Errorf("modeBits(%v) = %o, want %o", tc.kind, got, tc.want)
		}
	}
}

func TestMapReadErr(t *testing.T) {
	cases := []struct {
		err  error
		want syscall.Errno
	}{
		{ErrDenied, syscall.EACCES},
		{ErrFrozen, syscall.EAGAIN},
		{ErrNotRegular, syscall.EISDIR},
		{fs.ErrNotExist, syscall.ENOENT},
		{errors.New("anything else"), syscall.EIO},
	}
	for _, tc := range cases {
		if got := mapReadErr(tc.err); got != tc.want {
			t.Errorf("mapReadErr(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
