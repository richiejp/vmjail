package agent

import (
	"fmt"
	"strconv"
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

// VsockTracePortCmdlineKey names the kernel cmdline parameter that
// tells the agent which AF_VSOCK port to dial on the host (CID 2)
// when shipping its span buffer. Format: `daft.vsock.trace_port=N`.
const VsockTracePortCmdlineKey = "daft.vsock.trace_port"

// FormatVsockTracePortCmdline renders a port to the wire form. Used
// by the host to add `daft.vsock.trace_port=N` to the cmdline.
func FormatVsockTracePortCmdline(port uint32) string {
	return fmt.Sprintf("%s=%d", VsockTracePortCmdlineKey, port)
}

// ParseVsockTracePortCmdline returns the parsed trace port plus
// ok=true when the token is present. Absent token → ("",false,nil)
// is the "feature not requested" path. Malformed value (non-numeric,
// out of range) is an error so a typo on the host surfaces.
func ParseVsockTracePortCmdline(cmdline string) (port uint32, ok bool, err error) {
	return parseNonZeroUintCmdline(cmdline, VsockTracePortCmdlineKey)
}

// parseNonZeroUintCmdline parses `<key>=<uint32>` from cmdline.
// Absent token ⇒ (0, false, nil). Present-but-malformed (empty,
// non-numeric, zero) ⇒ (0, false, err). Shared by every cmdline
// parameter whose validation shape is "must be a non-zero uint32"
// (currently the trace and attach vsock-port keys).
func parseNonZeroUintCmdline(cmdline, key string) (value uint32, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, key)
	if !ok {
		return 0, false, nil
	}
	if val == "" {
		return 0, false, fmt.Errorf("agent: %s value is empty", key)
	}
	n, perr := strconv.ParseUint(val, 10, 32)
	if perr != nil {
		return 0, false, fmt.Errorf("agent: %s=%q: %w", key, val, perr)
	}
	if n == 0 {
		return 0, false, fmt.Errorf("agent: %s must be non-zero", key)
	}
	return uint32(n), true, nil
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

// findAllCmdlineValues returns every value of key=... tokens in
// cmdline, in order of appearance. nil when the key is absent.
// Plural counterpart to findCmdlineValue.
func findAllCmdlineValues(cmdline, key string) []string {
	prefix := key + "="
	var vals []string
	for tok := range strings.FieldsSeq(cmdline) {
		if v, ok := strings.CutPrefix(tok, prefix); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

// validateAbsoluteCmdlinePath rejects empty and relative values with
// the canonical error shape every absolute-path cmdline parser uses.
func validateAbsoluteCmdlinePath(key, val string) error {
	if val == "" {
		return fmt.Errorf("agent: %s value is empty", key)
	}
	if !strings.HasPrefix(val, "/") {
		return fmt.Errorf("agent: %s path %q must be absolute", key, val)
	}
	return nil
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
	if err := validateAbsoluteCmdlinePath(BundleCmdlineKey, val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// OpensnoopCmdlineKey names the kernel cmdline parameter that asks the
// agent to run the Tier-2 open-snoop eBPF gadget, writing its openat(2)
// trace to the given absolute guest path. Format:
// `daft.observe.opensnoop=<absolute-path>`. The path is usually on a
// writable virtiofs share so the host can read the trace after the VM
// exits. Absent ⇒ the gadget is not started.
const OpensnoopCmdlineKey = "daft.observe.opensnoop"

// FormatOpensnoopCmdline renders a guest log path to the wire form. Used
// by the host to add `daft.observe.opensnoop=<path>` to the cmdline.
func FormatOpensnoopCmdline(path string) string {
	return fmt.Sprintf("%s=%s", OpensnoopCmdlineKey, path)
}

// ParseOpensnoopCmdline scans a kernel cmdline for
// `daft.observe.opensnoop=<path>`. Returns (path, true, nil) on a
// well-formed absolute path, (_, false, err) on a malformed
// (empty/relative) value, and ("", false, nil) when the token is absent.
// The absolute-path invariant matches the other path-valued keys so a
// host-config typo surfaces here rather than as an obscure open() error
// inside the agent.
func ParseOpensnoopCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, OpensnoopCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if err := validateAbsoluteCmdlinePath(OpensnoopCmdlineKey, val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// ExecsnoopCmdlineKey names the kernel cmdline parameter that asks the
// agent to run the Tier-2 exec-snoop eBPF gadget, writing its execve(2)
// trace to the given absolute guest path. Same contract as
// OpensnoopCmdlineKey. Format: `daft.observe.execsnoop=<absolute-path>`.
const ExecsnoopCmdlineKey = "daft.observe.execsnoop"

// FormatExecsnoopCmdline renders a guest log path to the wire form.
func FormatExecsnoopCmdline(path string) string {
	return fmt.Sprintf("%s=%s", ExecsnoopCmdlineKey, path)
}

// ParseExecsnoopCmdline scans a kernel cmdline for
// `daft.observe.execsnoop=<path>`, with the same absolute-path contract as
// ParseOpensnoopCmdline.
func ParseExecsnoopCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, ExecsnoopCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if err := validateAbsoluteCmdlinePath(ExecsnoopCmdlineKey, val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// SleepsnoopCmdlineKey names the kernel cmdline parameter that asks the
// agent to run the Tier-2 sleep-snoop eBPF gadget, writing its nanosleep(2) /
// clock_nanosleep(2) trace to the given absolute guest path. Same contract as
// OpensnoopCmdlineKey. Format: `daft.observe.sleepsnoop=<absolute-path>`.
const SleepsnoopCmdlineKey = "daft.observe.sleepsnoop"

// FormatSleepsnoopCmdline renders a guest log path to the wire form.
func FormatSleepsnoopCmdline(path string) string {
	return fmt.Sprintf("%s=%s", SleepsnoopCmdlineKey, path)
}

// ParseSleepsnoopCmdline scans a kernel cmdline for
// `daft.observe.sleepsnoop=<path>`, with the same absolute-path contract as
// ParseOpensnoopCmdline.
func ParseSleepsnoopCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, SleepsnoopCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if err := validateAbsoluteCmdlinePath(SleepsnoopCmdlineKey, val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// IosnoopCmdlineKey names the kernel cmdline parameter that asks the agent to
// run the Tier-2 io-snoop eBPF gadget, writing its per-fd I/O trace to the
// given absolute guest path. Same contract as OpensnoopCmdlineKey. Format:
// `daft.observe.iosnoop=<absolute-path>`.
const IosnoopCmdlineKey = "daft.observe.iosnoop"

// FormatIosnoopCmdline renders a guest log path to the wire form.
func FormatIosnoopCmdline(path string) string {
	return fmt.Sprintf("%s=%s", IosnoopCmdlineKey, path)
}

// ParseIosnoopCmdline scans a kernel cmdline for `daft.observe.iosnoop=<path>`,
// with the same absolute-path contract as ParseOpensnoopCmdline.
func ParseIosnoopCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, IosnoopCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if err := validateAbsoluteCmdlinePath(IosnoopCmdlineKey, val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// SyscallsCmdlineKey names the kernel cmdline parameter that asks the agent to
// dump the guest kernel's available syscall list (enumerated from tracefs at
// boot) to the given absolute guest path. Same absolute-path contract as
// OpensnoopCmdlineKey. Format: `daft.observe.syscalls=<absolute-path>`. Usually
// a path on the observe share so the host can read back what is traceable
// inside the microvm without booting it interactively.
const SyscallsCmdlineKey = "daft.observe.syscalls"

// FormatSyscallsCmdline renders a guest path to the wire form.
func FormatSyscallsCmdline(path string) string {
	return fmt.Sprintf("%s=%s", SyscallsCmdlineKey, path)
}

// ParseSyscallsCmdline scans a kernel cmdline for
// `daft.observe.syscalls=<path>`, with the same absolute-path contract as
// ParseOpensnoopCmdline.
func ParseSyscallsCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, SyscallsCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if err := validateAbsoluteCmdlinePath(SyscallsCmdlineKey, val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// SummaryCmdlineKey names the kernel cmdline parameter that asks the agent
// to write the live build-progress summary (OBSERVABILITY.md #6) to the
// given absolute guest path, rewritten atomically ~1 Hz. Format:
// `daft.observe.summary=<absolute-path>`, usually a path on the writable
// observe share so the host-side `daft sandbox observe` can read it while
// the workload runs. Absent ⇒ no summary is written. The summary is fed by
// the open-snoop, exec-snoop, and sleep-snoop gadgets, so it only produces
// output when at least one of `daft.observe.opensnoop` /
// `daft.observe.execsnoop` / `daft.observe.sleepsnoop` is set.
const SummaryCmdlineKey = "daft.observe.summary"

// FormatSummaryCmdline renders a guest summary path to the wire form.
func FormatSummaryCmdline(path string) string {
	return fmt.Sprintf("%s=%s", SummaryCmdlineKey, path)
}

// ParseSummaryCmdline scans a kernel cmdline for
// `daft.observe.summary=<path>`, with the same absolute-path contract as
// ParseOpensnoopCmdline.
func ParseSummaryCmdline(cmdline string) (path string, ok bool, err error) {
	val, ok := findCmdlineValue(cmdline, SummaryCmdlineKey)
	if !ok {
		return "", false, nil
	}
	if err := validateAbsoluteCmdlinePath(SummaryCmdlineKey, val); err != nil {
		return "", false, err
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

// PortForwardCmdlineKey names the kernel cmdline parameter that
// tells the agent to vsock-listen on VsockPort and forward each
// accepted connection to the container at 127.0.0.1:ContainerPort.
// Format: `daft.port=<vsockPort>:<containerPort>`. Repeatable —
// one token per exposed PortMapping. See Slice C1 in PLAN.md.
const PortForwardCmdlineKey = "daft.port"

// PortForwardSpec is the parsed form of a `daft.port=...` cmdline
// parameter. VsockPort is the AF_VSOCK port the agent listens on;
// ContainerPort is the TCP port inside the VM the container is
// already bound to. Both are required and must be non-zero.
type PortForwardSpec struct {
	VsockPort     uint32
	ContainerPort uint32
}

// FormatPortForwardCmdline renders a PortForwardSpec to the wire
// form. Host-side wiring uses this to build the cmdline.
func FormatPortForwardCmdline(s PortForwardSpec) string {
	return fmt.Sprintf("%s=%d:%d", PortForwardCmdlineKey, s.VsockPort, s.ContainerPort)
}

// ParsePortForwardCmdlines walks every `daft.port=...` token and
// returns the parsed specs in order of appearance. Empty cmdline
// or no matching tokens ⇒ (nil, nil). The first malformed token
// aborts so a typo on the host doesn't leave a half-configured
// forwarder running.
func ParsePortForwardCmdlines(cmdline string) ([]PortForwardSpec, error) {
	prefix := PortForwardCmdlineKey + "="
	var specs []PortForwardSpec
	for tok := range strings.FieldsSeq(cmdline) {
		val, ok := strings.CutPrefix(tok, prefix)
		if !ok {
			continue
		}
		spec, err := parsePortForwardSpec(val)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// parsePortForwardSpec validates a single `<vsockPort>:<containerPort>`
// value. Empty either side, non-numeric, zero, or >65535 are all
// rejected — the host's port table feeds straight into this and a
// silent miss would mean a workload's port never gets exposed.
func parsePortForwardSpec(val string) (PortForwardSpec, error) {
	vsockStr, containerStr, ok := strings.Cut(val, ":")
	if !ok {
		return PortForwardSpec{}, fmt.Errorf("agent: %s value %q must be <vsockPort>:<containerPort>", PortForwardCmdlineKey, val)
	}
	if vsockStr == "" || containerStr == "" {
		return PortForwardSpec{}, fmt.Errorf("agent: %s value %q has empty vsock or container port", PortForwardCmdlineKey, val)
	}
	vsockPort, err := parsePortField(vsockStr, "vsock port")
	if err != nil {
		return PortForwardSpec{}, err
	}
	containerPort, err := parsePortField(containerStr, "container port")
	if err != nil {
		return PortForwardSpec{}, err
	}
	return PortForwardSpec{VsockPort: vsockPort, ContainerPort: containerPort}, nil
}

// parsePortField parses a uint32 port and rejects zero / >65535.
// The label is folded into the error so the user can tell which
// side of the `:` was bad.
func parsePortField(s, label string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("agent: %s: %s %q: %w", PortForwardCmdlineKey, label, s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("agent: %s: %s must be non-zero", PortForwardCmdlineKey, label)
	}
	if n > 65535 {
		return 0, fmt.Errorf("agent: %s: %s %d out of range", PortForwardCmdlineKey, label, n)
	}
	return uint32(n), nil
}
