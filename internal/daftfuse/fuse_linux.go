//go:build linux

package daftfuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"syscall"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

// Server is a running FUSE filesystem. Returned from Mount.Serve.
// Wait blocks until the kernel signals teardown (typically a
// fusermount -u or a defer Unmount call); Unmount issues that
// teardown explicitly.
type Server struct {
	srv *fuse.Server
}

// Wait blocks until the filesystem is unmounted.
func (s *Server) Wait() { s.srv.Wait() }

// Unmount issues a kernel unmount and returns the error from the
// underlying syscall. Safe to call after Wait has returned.
func (s *Server) Unmount() error { return s.srv.Unmount() }

// Serve mounts the filesystem at mountpoint and returns a Server.
// mountpoint must already exist and be empty. The caller is
// responsible for Unmount + cleanup; the kernel will not free the
// mountpoint dir on its own.
func (m *Mount) Serve(_ context.Context, mountpoint string) (*Server, error) {
	root := &fuseNode{mount: m, relPath: "", node: m.tree.root}
	opts := &gofuse.Options{
		MountOptions: fuse.MountOptions{
			Name:       "daftfuse",
			AllowOther: false,
		},
	}
	srv, err := gofuse.Mount(mountpoint, root, opts)
	if err != nil {
		return nil, fmt.Errorf("daftfuse: mount %s: %w", mountpoint, err)
	}
	return &Server{srv: srv}, nil
}

// fuseNode adapts one tree node to the go-fuse Node interface. The
// relPath field is the manifest-relative path (forward slashes, no
// leading slash, "" for root); kept here so chunk reads can build
// ReadEvent.Path without walking back up the parent chain.
type fuseNode struct {
	gofuse.Inode
	mount   *Mount
	relPath string
	node    *node
}

var (
	_ gofuse.NodeGetattrer  = (*fuseNode)(nil)
	_ gofuse.NodeLookuper   = (*fuseNode)(nil)
	_ gofuse.NodeReaddirer  = (*fuseNode)(nil)
	_ gofuse.NodeOpener     = (*fuseNode)(nil)
	_ gofuse.NodeReader     = (*fuseNode)(nil)
	_ gofuse.NodeReadlinker = (*fuseNode)(nil)
)

// attrCacheDuration is how long the kernel may treat an attr lookup
// as fresh before re-asking. The manifest is immutable for the
// mount's lifetime so this can be quite long; we cap at one minute
// to bound the staleness window if a future slice ever allows
// live-replacing the manifest.
const attrCacheDuration = 60 * time.Second

func (f *fuseNode) Getattr(_ context.Context, _ gofuse.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillAttr(&out.Attr, f.node)
	out.SetTimeout(attrCacheDuration)
	return 0
}

func (f *fuseNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofuse.Inode, syscall.Errno) {
	if f.node.kind != workspacev1.EntryKind_ENTRY_KIND_DIR {
		return nil, syscall.ENOTDIR
	}
	child, ok := f.node.entries[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	childPath := name
	if f.relPath != "" {
		childPath = path.Join(f.relPath, name)
	}
	stable := gofuse.StableAttr{Mode: modeBits(child.kind)}
	inode := f.NewInode(ctx, &fuseNode{
		mount: f.mount, relPath: childPath, node: child,
	}, stable)
	fillAttr(&out.Attr, child)
	out.SetEntryTimeout(attrCacheDuration)
	out.SetAttrTimeout(attrCacheDuration)
	return inode, 0
}

func (f *fuseNode) Readdir(_ context.Context) (gofuse.DirStream, syscall.Errno) {
	if f.node.kind != workspacev1.EntryKind_ENTRY_KIND_DIR {
		return nil, syscall.ENOTDIR
	}
	names := f.node.childNames()
	entries := make([]fuse.DirEntry, 0, len(names))
	for _, name := range names {
		child := f.node.entries[name]
		entries = append(entries, fuse.DirEntry{
			Name: name,
			Mode: modeBits(child.kind),
		})
	}
	return gofuse.NewListDirStream(entries), 0
}

func (f *fuseNode) Open(_ context.Context, _ uint32) (gofuse.FileHandle, uint32, syscall.Errno) {
	if f.node.kind != workspacev1.EntryKind_ENTRY_KIND_FILE {
		return nil, 0, syscall.EISDIR
	}
	// nil file handle: Read forwards to NodeReader.Read on the
	// node itself, which is what we want.
	return nil, 0, 0
}

func (f *fuseNode) Read(ctx context.Context, _ gofuse.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if f.node.kind != workspacev1.EntryKind_ENTRY_KIND_FILE {
		return nil, syscall.EISDIR
	}
	n, err := f.mount.ReadAt(ctx, f.relPath, dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, mapReadErr(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *fuseNode) Readlink(_ context.Context) ([]byte, syscall.Errno) {
	if f.node.kind != workspacev1.EntryKind_ENTRY_KIND_SYMLINK {
		return nil, syscall.EINVAL
	}
	return []byte(f.node.target), 0
}

func modeBits(kind workspacev1.EntryKind) uint32 {
	switch kind {
	case workspacev1.EntryKind_ENTRY_KIND_DIR:
		return syscall.S_IFDIR
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		return syscall.S_IFLNK
	default:
		return syscall.S_IFREG
	}
}

func fillAttr(out *fuse.Attr, n *node) {
	out.Mode = modeBits(n.kind) | (n.mode & 0o7777)
	out.Size = n.size
	if !n.mtime.IsZero() {
		out.Mtime = uint64(n.mtime.Unix())           //nolint:gosec // post-1970 mtimes fit in uint64.
		out.Mtimensec = uint32(n.mtime.Nanosecond()) //nolint:gosec // Nanosecond() returns [0,1e9).
	}
}

func mapReadErr(err error) syscall.Errno {
	switch {
	case errors.Is(err, ErrDenied):
		return syscall.EACCES
	case errors.Is(err, ErrFrozen):
		return syscall.EAGAIN
	case errors.Is(err, ErrNotRegular):
		return syscall.EISDIR
	case errors.Is(err, fs.ErrNotExist):
		return syscall.ENOENT
	default:
		return syscall.EIO
	}
}
