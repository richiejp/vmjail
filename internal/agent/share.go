package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ShareSpec names a virtio-fs share to mount: Tag is the virtio-fs tag
// the host exported, Mountpoint the in-guest path to mount it at. The
// agent receives these in the GuestConfig (see configShareSpecs).
type ShareSpec struct {
	Tag        string
	Mountpoint string
}

// ShareMarkerFile is the file the agent reads from the mounted share to
// prove file sharing actually works end-to-end. The host writes this
// before booting; the agent reads it and reports the contents on the
// console. Stable so the host and agent agree without an import path.
const ShareMarkerFile = "hello.txt"

// ShareReportPrefix is the prefix the agent prints on the console when
// reporting share state. Anything after the prefix is treated by the
// host as opaque payload (the marker-file contents on success, or an
// error string on failure). Includes a trailing space so the payload
// reads cleanly.
const ShareReportPrefix = "share: "

// ShareDeps abstracts the file-system / syscall surface the share +
// overlay paths share with mount(2). data is the comma-separated
// fs-options string mount(2) takes — empty for virtiofs/tmpfs,
// non-empty for overlayfs (lowerdir=,upperdir=,workdir=).
type ShareDeps struct {
	Mount    func(source, target, fstype, data string) error
	MkdirAll func(path string, perm os.FileMode) error
	ReadFile func(path string) ([]byte, error)
}

// MountShare mkdirs the mountpoint and mounts the virtio-fs share.
// Returns the first error encountered; nil on success. Side-effect-
// free with respect to w — for the v0 marker-demo behaviour use
// ApplyShare instead.
func MountShare(spec ShareSpec, deps ShareDeps) error {
	if err := deps.MkdirAll(spec.Mountpoint, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := deps.Mount(spec.Tag, spec.Mountpoint, "virtiofs", ""); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	return nil
}

// ApplyShare mounts the virtio-fs share, reads ShareMarkerFile from
// it, and writes a single ShareReportPrefix line to w. Failures are
// reported the same way (`share: <error>`) so the host can scan one
// prefix to learn the outcome — sharing failure is not fatal here,
// the agent keeps running.
//
// Used by the v0 marker-demo path. The run-with-image path uses
// MountShare instead, since the proof-of-life is the container's
// stdout, not a marker file.
func ApplyShare(spec ShareSpec, deps ShareDeps, w io.Writer) {
	if err := MountShare(spec, deps); err != nil {
		writeReport(w, ShareReportPrefix, err.Error())
		return
	}
	data, err := deps.ReadFile(filepath.Join(spec.Mountpoint, ShareMarkerFile))
	if err != nil {
		writeReport(w, ShareReportPrefix, "read: "+err.Error())
		return
	}
	writeReport(w, ShareReportPrefix, strings.TrimRight(string(data), "\r\n"))
}
