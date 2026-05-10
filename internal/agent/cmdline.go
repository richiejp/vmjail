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

// FormatBundleCmdline renders a bundle path back to the wire form. Used
// by the host to add `daft.bundle=...` to the cmdline it hands to
// cloud-hypervisor.
func FormatBundleCmdline(path string) string {
	return fmt.Sprintf("%s=%s", BundleCmdlineKey, path)
}

// findCmdlineValue scans a kernel cmdline (whitespace-separated
// `key=value` tokens, mirroring the kernel's /proc/cmdline parser) for
// the first occurrence of key= and returns its value.
//
// ok=false means the key is absent — that's the "feature not requested"
// signal, not an error. Per-key validation is the caller's job.
func findCmdlineValue(cmdline, key string) (string, bool) {
	prefix := key + "="
	for tok := range strings.FieldsSeq(cmdline) {
		if v, ok := strings.CutPrefix(tok, prefix); ok {
			return v, true
		}
	}
	return "", false
}

// ParseBundleCmdline scans a kernel cmdline for `daft.bundle=<path>`.
// Returns (path, ok=true, nil) on a well-formed token, (_, false, err)
// on malformed input, and ("", false, nil) when the token is absent.
//
// The path must be absolute. The agent will pass it straight to crun
// as `--bundle <path>`, so any further validation is crun's problem;
// we only enforce the absolute-path invariant here so a typo in the
// host config doesn't surface as a crun error with no useful context.
func ParseBundleCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, BundleCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if val == "" {
		return "", false, fmt.Errorf("agent: %s value is empty", BundleCmdlineKey)
	}
	if !strings.HasPrefix(val, "/") {
		return "", false, fmt.Errorf(
			"agent: %s path %q must be absolute", BundleCmdlineKey, val,
		)
	}
	return val, true, nil
}

// parseShareSpec validates a single `<tag>:<mountpoint>` value (the
// part after `daft.share=`) and returns the parsed spec. Shared by
// ParseShareCmdline (singular, first match) and ParseShareCmdlines
// (plural, all matches).
//
// Validation:
//   - Tag must be non-empty and contain no whitespace.
//   - Mountpoint must be an absolute path (start with /).
//
// Both constraints are checked because the agent passes them straight
// to mount(2) — bogus values would surface as syscall errors with no
// useful context.
func parseShareSpec(val string) (ShareSpec, error) {
	tag, mp, ok := strings.Cut(val, ":")
	if !ok {
		return ShareSpec{}, fmt.Errorf(
			"agent: malformed %s=%q: want <tag>:<mountpoint>",
			ShareCmdlineKey, val,
		)
	}
	if tag == "" {
		return ShareSpec{}, fmt.Errorf("agent: %s tag is empty", ShareCmdlineKey)
	}
	if strings.ContainsAny(tag, " \t\n\r") {
		return ShareSpec{}, fmt.Errorf(
			"agent: %s tag %q contains whitespace", ShareCmdlineKey, tag,
		)
	}
	if !strings.HasPrefix(mp, "/") {
		return ShareSpec{}, fmt.Errorf(
			"agent: %s mountpoint %q must be absolute", ShareCmdlineKey, mp,
		)
	}
	return ShareSpec{Tag: tag, Mountpoint: mp}, nil
}

// ParseShareCmdline returns the first `daft.share=...` token's parsed
// form, or ok=false when no such token exists. Use ParseShareCmdlines
// when multiple shares are possible; this single-result variant is
// kept for v0 callers that only handle one share.
func ParseShareCmdline(cmdline string) (ShareSpec, bool, error) {
	val, ok := findCmdlineValue(cmdline, ShareCmdlineKey)
	if !ok {
		return ShareSpec{}, false, nil
	}
	spec, err := parseShareSpec(val)
	if err != nil {
		return ShareSpec{}, false, err
	}
	return spec, true, nil
}

// ParseShareCmdlines walks every `daft.share=...` token in cmdline
// and returns the parsed specs in order of appearance. Returns nil
// when no shares are configured. The first malformed token aborts —
// we don't silently ignore the others, because a stray malformed
// share usually means the host wired up the cmdline wrong and
// continuing past it would mask the bug.
func ParseShareCmdlines(cmdline string) ([]ShareSpec, error) {
	prefix := ShareCmdlineKey + "="
	var specs []ShareSpec
	for tok := range strings.FieldsSeq(cmdline) {
		val, ok := strings.CutPrefix(tok, prefix)
		if !ok {
			continue
		}
		spec, err := parseShareSpec(val)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}
