package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

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

// ShareDeps abstracts the file-system / syscall surface ApplyShare uses
// so tests can swap real mount/read for fakes without touching the
// host's actual file system.
type ShareDeps struct {
	// Mount calls mount(2) — equivalent to `mount -t <fstype> <source> <target>`.
	Mount func(source, target, fstype string) error
	// MkdirAll, ReadFile mirror os.MkdirAll / os.ReadFile.
	MkdirAll func(path string, perm os.FileMode) error
	ReadFile func(path string) ([]byte, error)
}

// ApplyShare mounts the virtio-fs share described by spec, reads
// ShareMarkerFile from it, and writes a single ShareReportPrefix line
// to w. Failures are reported the same way (`share: <error>`) so the
// host can scan one prefix to learn the outcome — sharing failure is
// not fatal here, the agent keeps running.
func ApplyShare(spec ShareSpec, deps ShareDeps, w io.Writer) {
	if err := deps.MkdirAll(spec.Mountpoint, 0o755); err != nil {
		_, _ = fmt.Fprintf(w, "%s%s\n", ShareReportPrefix, "mkdir: "+err.Error())
		return
	}
	if err := deps.Mount(spec.Tag, spec.Mountpoint, "virtiofs"); err != nil {
		_, _ = fmt.Fprintf(w, "%s%s\n", ShareReportPrefix, "mount: "+err.Error())
		return
	}
	data, err := deps.ReadFile(filepath.Join(spec.Mountpoint, ShareMarkerFile))
	if err != nil {
		_, _ = fmt.Fprintf(w, "%s%s\n", ShareReportPrefix, "read: "+err.Error())
		return
	}
	_, _ = fmt.Fprintf(w, "%s%s\n", ShareReportPrefix, strings.TrimRight(string(data), "\r\n"))
}
