package agent

import (
	"fmt"
	"strings"
)

// ShareCmdlineKey is the kernel cmdline parameter that tells the agent
// to mount a virtio-fs share. Format: `daft.share=<tag>:<mountpoint>`.
//
// Example: `daft.share=workspace:/share` mounts the virtiofs device with
// tag "workspace" at /share inside the guest.
//
// Stable so the host orchestrator (internal/sandbox) and the in-VM
// agent agree on the wire format without import dependencies.
const ShareCmdlineKey = "daft.share"

// ShareSpec is the parsed form of a `daft.share=...` cmdline parameter.
type ShareSpec struct {
	Tag        string
	Mountpoint string
}

// FormatShareCmdline renders a ShareSpec back to the wire form. Used by
// the host to build the cmdline it hands to cloud-hypervisor.
func FormatShareCmdline(s ShareSpec) string {
	return fmt.Sprintf("%s=%s:%s", ShareCmdlineKey, s.Tag, s.Mountpoint)
}

// ParseShareCmdline scans a kernel cmdline string for `daft.share=...`
// and returns its parsed form. Returns ok=false when the parameter is
// absent (not an error — sharing is optional).
//
// Validation:
//   - Tag must be non-empty and ASCII-printable, no whitespace.
//   - Mountpoint must be an absolute path (start with /).
//
// Both constraints are checked because the agent passes them straight
// to mount(2) — bogus values would surface as syscall errors with no
// useful context.
//
// The cmdline is whitespace-separated `key=value` tokens, mirroring how
// the kernel parses /proc/cmdline. Unknown tokens are ignored.
func ParseShareCmdline(cmdline string) (ShareSpec, bool, error) {
	prefix := ShareCmdlineKey + "="
	for tok := range strings.FieldsSeq(cmdline) {
		if !strings.HasPrefix(tok, prefix) {
			continue
		}
		val := tok[len(prefix):]
		tag, mp, ok := strings.Cut(val, ":")
		if !ok {
			return ShareSpec{}, false, fmt.Errorf(
				"agent: malformed %s=%q: want <tag>:<mountpoint>",
				ShareCmdlineKey, val,
			)
		}
		if tag == "" {
			return ShareSpec{}, false, fmt.Errorf(
				"agent: %s tag is empty", ShareCmdlineKey,
			)
		}
		if strings.ContainsAny(tag, " \t\n\r") {
			return ShareSpec{}, false, fmt.Errorf(
				"agent: %s tag %q contains whitespace", ShareCmdlineKey, tag,
			)
		}
		if !strings.HasPrefix(mp, "/") {
			return ShareSpec{}, false, fmt.Errorf(
				"agent: %s mountpoint %q must be absolute", ShareCmdlineKey, mp,
			)
		}
		return ShareSpec{Tag: tag, Mountpoint: mp}, true, nil
	}
	return ShareSpec{}, false, nil
}
