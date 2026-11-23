//go:build linux

package bundled

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func loadAssets(ch, kernel, initramfs, virtiofsd []byte) (p Paths, err error) {
	if len(ch) < minBundledSize || len(kernel) < minBundledSize || len(initramfs) < minBundledSize || len(virtiofsd) < minBundledSize {
		return Paths{}, ErrNotBundled
	}
	defer func() {
		if err != nil {
			p.Close()
			p = Paths{}
		}
	}()
	assets := []struct {
		name       string
		data       []byte
		executable bool
	}{
		{"cloud-hypervisor", ch, true},
		{"vmlinux", kernel, false},
		{"initramfs.cpio", initramfs, false},
		{"virtiofsd", virtiofsd, true},
	}
	paths := []*string{&p.CloudHypervisor, &p.Kernel, &p.Initramfs, &p.Virtiofsd}
	for i, asset := range assets {
		p.Files[i], err = sealedAsset(asset.name, asset.data, asset.executable)
		if err != nil {
			return p, err
		}
		*paths[i] = fmt.Sprintf("/proc/self/fd/%d", p.Files[i].Fd())
	}
	return p, nil
}

func sealedAsset(name string, data []byte, executable bool) (*os.File, error) {
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_NOEXEC_SEAL
	mode := os.FileMode(0o400)
	if executable {
		flags = unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING | unix.MFD_EXEC
		mode = 0o500
	}
	fd, err := unix.MemfdCreate(name, flags)
	if err != nil {
		return nil, fmt.Errorf("bundled: memfd %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name) //nolint:gosec // MemfdCreate succeeded, so fd is nonnegative.
	return sealAssetFile(f, data, mode)
}

// sealAssetFile takes ownership even on failure: an unsealed file must never
// escape to a caller that could accidentally launch from mutable contents.
func sealAssetFile(f *os.File, data []byte, mode os.FileMode) (*os.File, error) {
	fail := func(err error) (*os.File, error) {
		_ = f.Close()
		return nil, fmt.Errorf("bundled: seal %s: %w", f.Name(), err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_EXEC | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return fail(err)
	}
	// Verify only after sealing. A process able to open our descriptor via
	// /proc during population must not be able to race in different bytes.
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, math.MaxInt64)); err != nil {
		return fail(err)
	}
	want := sha256.Sum256(data)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		return fail(errors.New("sealed contents differ from embedded asset"))
	}
	return f, nil
}
