//go:build linux

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type noStatFileInfo struct{}

func (noStatFileInfo) Name() string       { return "entry" }
func (noStatFileInfo) Size() int64        { return 0 }
func (noStatFileInfo) Mode() os.FileMode  { return 0 }
func (noStatFileInfo) ModTime() time.Time { return time.Time{} }
func (noStatFileInfo) IsDir() bool        { return false }
func (noStatFileInfo) Sys() any           { return nil }

func TestReadWorkspaceMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(path, []byte("XFSBpayload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if magic, err := readWorkspaceMagic(path); err != nil || string(magic[:]) != xfsMagicString {
		t.Fatalf("magic = %q, %v", magic, err)
	}
	if _, err := readWorkspaceMagic(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error = %v", err)
	}
	short := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(short, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceMagic(short); err == nil {
		t.Fatal("short device accepted")
	}
	deps := defaultWorkspaceDiskDeps()
	if deps.readMagic == nil || deps.mountXFS == nil || deps.bindMount == nil || deps.run == nil {
		t.Fatal("default workspace disk dependencies incomplete")
	}
	if err := deps.mountXFS("/missing-device", filepath.Join(t.TempDir(), "mount")); err == nil {
		t.Fatal("invalid XFS mount succeeded")
	}
	if err := deps.bindMount("/missing-source", filepath.Join(t.TempDir(), "bind")); err == nil {
		t.Fatal("invalid bind mount succeeded")
	}
	if err := deps.run("/missing-xfs-tool"); err == nil {
		t.Fatal("missing tool execution succeeded")
	}
}

func TestCopyWorkspaceTree(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	destination := filepath.Join(parent, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "file"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/file", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(source, destination); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- path is beneath the test-owned temporary directory.
	data, err := os.ReadFile(filepath.Join(destination, "nested", "file"))
	if err != nil || string(data) != "payload" {
		t.Fatalf("copied file = %q, %v", data, err)
	}
	if target, err := os.Readlink(filepath.Join(destination, "link")); err != nil || target != "nested/file" {
		t.Fatalf("copied link = %q, %v", target, err)
	}
}

func TestCopyWorkspaceTreeRejectsInvalidSources(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	destination := filepath.Join(parent, "destination")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(source, destination); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(filepath.Join(parent, "missing"), destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source error = %v", err)
	}
	if err := copyWorkspaceTree(source, destination); err == nil {
		t.Fatal("copy over existing files succeeded")
	}
	fifoSource := filepath.Join(parent, "fifo-source")
	fifoDestination := filepath.Join(parent, "fifo-destination")
	if err := os.Mkdir(fifoSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fifoDestination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fifoSource, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(fifoSource, fifoDestination); err == nil {
		t.Fatal("unsupported FIFO copied")
	}
}

func TestCopyWorkspaceTreeRejectsDestinationErrors(t *testing.T) {
	parent := t.TempDir()
	missingDestinationSource := filepath.Join(parent, "missing-destination-source")
	if err := os.Mkdir(missingDestinationSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(missingDestinationSource, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(missingDestinationSource, filepath.Join(parent, "missing-destination")); err == nil {
		t.Fatal("copy into missing destination succeeded")
	}
	symlinkSource := filepath.Join(parent, "symlink-source")
	symlinkDestination := filepath.Join(parent, "symlink-destination")
	if err := os.Mkdir(symlinkSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(symlinkDestination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(symlinkSource, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("existing", filepath.Join(symlinkDestination, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(symlinkSource, symlinkDestination); err == nil {
		t.Fatal("copy over existing symlink succeeded")
	}
	directorySource := filepath.Join(parent, "directory-source")
	directoryDestination := filepath.Join(parent, "directory-destination")
	if err := os.MkdirAll(filepath.Join(directorySource, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directoryDestination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(directoryDestination, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(directorySource, directoryDestination); err == nil {
		t.Fatal("directory copy through dangling destination symlink succeeded")
	}
	if err := chownWorkspaceEntry(filepath.Join(parent, "entry"), noStatFileInfo{}, false); err == nil {
		t.Fatal("file info without Linux stat metadata accepted")
	}
}

func TestCopyWorkspaceTreePermissionErrors(t *testing.T) {
	parent := t.TempDir()
	unreadableSource := filepath.Join(parent, "unreadable-source")
	unreadableDestination := filepath.Join(parent, "unreadable-destination")
	if err := os.Mkdir(unreadableSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(unreadableDestination, 0o700); err != nil {
		t.Fatal(err)
	}
	unreadableFile := filepath.Join(unreadableSource, "file")
	if err := os.WriteFile(unreadableFile, []byte("secret"), 0); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(unreadableSource, unreadableDestination); err == nil {
		t.Fatal("unreadable file copied")
	}
	if err := os.Chmod(unreadableFile, 0o600); err != nil {
		t.Fatal(err)
	}
	blockedSource := filepath.Join(parent, "blocked-source")
	blockedDestination := filepath.Join(parent, "blocked-destination")
	if err := os.MkdirAll(filepath.Join(blockedSource, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blockedDestination, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := copyWorkspaceTree(blockedSource, blockedDestination); err == nil {
		t.Fatal("directory creation in read-only destination succeeded")
	}
	// #nosec G302 -- restore traversal so t.TempDir cleanup can remove it.
	if err := os.Chmod(blockedDestination, 0o700); err != nil {
		t.Fatal(err)
	}
}
