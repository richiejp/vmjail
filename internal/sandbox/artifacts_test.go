//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInheritedArtifactExec(t *testing.T) {
	if os.Getenv("VMJAIL_MEMFD_CHILD") == "1" {
		data, err := os.ReadFile("/proc/self/fd/4")
		if err != nil || string(data) != "sealed payload" {
			t.Fatalf("inherited data: %q %v", data, err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe) //nolint:gosec // Read our own test executable.
	if err != nil {
		t.Fatal(err)
	}
	makeFile := func(data []byte) *os.File {
		fd, err := unix.MemfdCreate("artifact-test", unix.MFD_ALLOW_SEALING|unix.MFD_CLOEXEC)
		if err != nil {
			t.Fatal(err)
		}
		f := os.NewFile(uintptr(fd), "artifact-test") //nolint:gosec // successful memfd call.
		t.Cleanup(func() { _ = f.Close() })
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
			t.Fatal(err)
		}
		return f
	}
	binary, payload := makeFile(data), makeFile([]byte("sealed payload"))
	cmd := exec.Command(fmt.Sprintf("/proc/self/fd/%d", binary.Fd()), "-test.run=^TestInheritedArtifactExec$") //nolint:gosec // Execute our own test binary from a sealed memfd.
	cmd.Env = append(os.Environ(), "VMJAIL_MEMFD_CHILD=1")
	inheritArtifacts(cmd, [4]*os.File{binary, payload})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exec sealed binary: %v\n%s", err, out)
	}
}
