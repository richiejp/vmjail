//go:build linux

package bundled

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExtractRejectsStubs(t *testing.T) {
	if IsBundled() {
		t.Fatal("expected stub assets in source build")
	}
	if _, err := Extract(t.TempDir()); !errors.Is(err, ErrNotBundled) {
		t.Fatalf("Extract: %v", err)
	}
	if _, err := loadAssets(nil, nil, nil, nil); !errors.Is(err, ErrNotBundled) {
		t.Fatalf("loadAssets: %v", err)
	}
}

func TestAssetsSealedAndClosed(t *testing.T) {
	data := bytes.Repeat([]byte("asset"), minBundledSize)
	p, err := loadAssets(data, data, data, data)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i, path := range []string{p.CloudHypervisor, p.Kernel, p.Initramfs, p.Virtiofsd} {
		got, err := os.ReadFile(path) //nolint:gosec // Path is an owned sealed memfd.
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read asset %d: %v", i, err)
		}
		f := p.Files[i]
		if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, unix.EPERM) {
			t.Fatalf("write: %v", err)
		}
		if err := f.Truncate(0); !errors.Is(err, unix.EPERM) {
			t.Fatalf("truncate: %v", err)
		}
		if err := f.Truncate(int64(len(data) + 1)); !errors.Is(err, unix.EPERM) {
			t.Fatalf("grow: %v", err)
		}
		stat, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Chmod(stat.Mode() ^ 0o100); !errors.Is(err, unix.EPERM) {
			t.Fatalf("change executable bit: %v", err)
		}
		reopened, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // Deliberately attempt to tamper with our own sealed memfd.
		if err == nil {
			_, err = reopened.WriteString("tampered")
			_ = reopened.Close()
			if !errors.Is(err, unix.EPERM) {
				t.Fatalf("reopened write: %v", err)
			}
		} else if !errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}
	}
	p.Close()
	for _, f := range p.Files {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("descriptor still open: %v", err)
		}
	}
}

func TestMemfdCreationFailure(t *testing.T) {
	if f, err := sealedAsset(strings.Repeat("x", 256), []byte("payload"), true); err == nil || f != nil {
		t.Fatalf("file=%v err=%v", f, err)
	}
}

func TestPartialLoadClosesDescriptors(t *testing.T) {
	if os.Getenv("VMJAIL_MEMFD_LIMIT_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPartialLoadClosesDescriptors$") //nolint:gosec // Reexec this test binary to isolate resource limits.
		cmd.Env = append(os.Environ(), "VMJAIL_MEMFD_LIMIT_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fd limit child: %v\n%s", err, out)
		}
		return
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	original := limit
	t.Cleanup(func() {
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Error(err)
		}
	})
	limit.Cur = 64
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	var held []*os.File
	defer func() {
		for _, f := range held {
			_ = f.Close()
		}
	}()
	for range 64 {
		f, err := os.Open("/dev/null")
		if errors.Is(err, unix.EMFILE) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	if len(held) < 2 {
		t.Fatal("not enough spare descriptors")
	}
	for range 2 {
		n := len(held) - 1
		_ = held[n].Close()
		held = held[:n]
	}
	data := bytes.Repeat([]byte("x"), minBundledSize)
	p, err := loadAssets(data, data, data, data)
	if !errors.Is(err, unix.EMFILE) {
		p.Close()
		t.Fatalf("expected partial allocation failure, got %v", err)
	}
	if p != (Paths{}) {
		t.Fatal("failed load returned live artifacts")
	}
	for range 2 {
		f, err := os.Open("/dev/null")
		if err != nil {
			t.Fatalf("partial load leaked descriptor: %v", err)
		}
		held = append(held, f)
	}
}

func TestMemfdWriteFailureClosesDescriptor(t *testing.T) {
	if os.Getenv("VMJAIL_MEMFD_SIZE_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMemfdWriteFailureClosesDescriptor$") //nolint:gosec // Reexec this test binary to isolate resource limits.
		cmd.Env = append(os.Environ(), "VMJAIL_MEMFD_SIZE_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("file size limit child: %v\n%s", err, out)
		}
		return
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	original := limit
	t.Cleanup(func() {
		if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
			t.Error(err)
		}
	})
	limit.Cur = 0
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	f, err := sealedAsset("oversize", []byte("payload"), false)
	if f != nil || !errors.Is(err, unix.EFBIG) {
		t.Fatalf("file=%v err=%v", f, err)
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("descriptor leak: before=%d after=%d", len(before), len(after))
	}
}

// A filesystem file cannot provide memfd sealing. Refuse to return it rather
// than silently accepting mutable bytes; the input descriptor must be closed.
func TestSealingFailureClosesUnsealedFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "unsealable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	result, err := sealAssetFile(f, []byte("untrusted without seals"), 0o400)
	if result != nil || err == nil {
		t.Fatalf("file=%v err=%v", result, err)
	}
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("unsealed descriptor leaked: %v", err)
	}
}

func TestSealedContentsMustMatchExactly(t *testing.T) {
	fd, err := unix.MemfdCreate("changed-contents", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "changed-contents") //nolint:gosec // Successful memfd creation returns a nonnegative descriptor.
	t.Cleanup(func() { _ = f.Close() })
	// Simulate unexpected extra bytes present when the descriptor is sealed.
	if _, err := f.WriteString("unexpected prefix"); err != nil {
		t.Fatal(err)
	}
	result, err := sealAssetFile(f, []byte("embedded payload"), 0o400)
	if result != nil || err == nil || !strings.Contains(err.Error(), "contents differ") {
		t.Fatalf("file=%v err=%v", result, err)
	}
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("mismatched descriptor leaked: %v", err)
	}
}
