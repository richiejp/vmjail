//go:build linux

package observability

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// cgroupV2Root is where unified cgroup v2 is mounted on every
// distribution we target. Embedded as a var (not const) only so
// tests can rebind it to a t.TempDir() via SwapCgroupRoot, for
// end-to-end coverage of the create/remove path without needing
// real cgroup delegation on the test host. Production callers
// must never write to it.
//
//nolint:gochecknoglobals // test-only knob, fixed in production.
var cgroupV2Root = "/sys/fs/cgroup"

// SwapCgroupRoot temporarily overrides the cgroup v2 mount root
// and returns the previous value. Test-only: lets tests in
// dependent packages (e.g. internal/sandbox) drive the cgroup
// create/remove paths against a tempdir instead of /sys/fs/cgroup.
// Callers must restore via t.Cleanup. Not safe for concurrent
// goroutines.
func SwapCgroupRoot(root string) string {
	prev := cgroupV2Root
	cgroupV2Root = root
	return prev
}

// errNoCgroupV2 is returned when /sys/fs/cgroup isn't a cgroup v2
// unified hierarchy. Callers fall back to /proc sampling.
var errNoCgroupV2 = errors.New("observability: /sys/fs/cgroup is not cgroup v2")

// CurrentCgroup returns the absolute filesystem path of the
// caller process's own cgroup v2 directory, e.g.
// /sys/fs/cgroup/user.slice/user-1000.slice/session-12.scope.
//
// Reads /proc/self/cgroup. The v2 line has format `0::<path>`; if
// any non-v2 lines appear (legacy hybrid hierarchies) we return
// errNoCgroupV2 so the caller knows not to attempt sub-cgroup
// creation.
func CurrentCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("observability: read /proc/self/cgroup: %w", err)
	}
	return parseSelfCgroup(b)
}

// parseSelfCgroup is the bytes-in side of currentCgroupPath, split
// for unit tests. Looks for the single `0::<path>` line — every
// process under a unified hierarchy has exactly one. Hybrid (v1+v2)
// systems would have additional lines we can't make sense of, so
// we return errNoCgroupV2 in that case.
func parseSelfCgroup(b []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		path := strings.TrimPrefix(line, "0::")
		if path == "" || !strings.HasPrefix(path, "/") {
			return "", fmt.Errorf("observability: bad cgroup line %q", line)
		}
		return filepath.Join(cgroupV2Root, path), nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("observability: scan /proc/self/cgroup: %w", err)
	}
	return "", errNoCgroupV2
}

// CreateSubCgroup creates `<parent>/<name>` as a cgroup v2 sub-
// directory and returns the absolute path. Caller is responsible
// for calling RemoveCgroup once the cgroup's processes have
// exited. Permission errors (the typical failure mode on user-
// session hosts without `Delegate=yes`) bubble up; callers should
// log and fall back to /proc-only mode — the per-VM cgroup wrap is
// a best-effort optimisation, not a correctness invariant.
func CreateSubCgroup(parent, name string) (string, error) {
	if !strings.HasPrefix(parent, cgroupV2Root) {
		return "", fmt.Errorf("observability: parent %q must be under %s", parent, cgroupV2Root)
	}
	if name == "" || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("observability: invalid cgroup name %q", name)
	}
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o750); err != nil {
		return "", fmt.Errorf("observability: mkdir %s: %w", path, err)
	}
	return path, nil
}

// PrepareDelegatedSubtree enables the memory + cpu + io + pids controllers
// on the caller's delegated cgroup v2 scope so per-VM child cgroups can
// carry resource limits AND full accounting (memory/io/pids), and returns
// the scope path — the parent under which those per-VM cgroups must be
// created so they inherit the controllers.
//
// cgroup v2's "no internal processes" rule forbids enabling controllers in
// a cgroup's subtree_control while that cgroup holds processes directly.
// The delegated scope holds the sandbox-host process itself, so this first
// moves the caller into a leaf child (<scope>/<leaf>) — leaving the scope
// processless — then writes the controllers to <scope>/cgroup.subtree_control.
// Per-VM cgroups are then created as siblings of the leaf, under the scope.
//
// Idempotent via leaf detection: when the caller is ALREADY inside the leaf
// (e.g. a re-exec'd child that inherited it from a parent which already
// prepared the scope) the move + enable are skipped and the scope (the
// leaf's parent) is returned. MUST be called while a single process owns
// the scope — i.e. before any userns re-exec forks a sibling into it.
//
// On failure it returns a non-empty fallback vmParent (the caller's current
// cgroup) ALONGSIDE the error, so the caller can still create per-VM
// cgroups there — just without the controllers, i.e. without limits. The
// common failure is a SHARED scope (CI / a test harness, where sibling
// processes leave the scope non-empty so subtree_control can't be enabled):
// best-effort there rather than refusing to boot.
func PrepareDelegatedSubtree(leaf string) (vmParent string, err error) {
	cur, err := CurrentCgroup()
	if err != nil {
		return "", err
	}
	return prepareDelegatedSubtreeAt(cur, leaf)
}

// subtreeControllers is the controller set enabled on the delegated scope.
// memory + cpu back the per-VM limits; io + pids round out the accounting
// the per-VM cgroup can then report.
const subtreeControllers = "+memory +cpu +io +pids"

// prepareDelegatedSubtreeAt is PrepareDelegatedSubtree with the caller's
// cgroup passed in, so it can be unit-tested against a fake cgroup root.
func prepareDelegatedSubtreeAt(cur, leaf string) (string, error) {
	if leaf == "" || strings.ContainsAny(leaf, "/\x00") {
		return cur, fmt.Errorf("observability: invalid supervisor leaf %q", leaf)
	}
	if filepath.Base(cur) == leaf {
		// Already inside the leaf — a parent already prepared the scope.
		return filepath.Dir(cur), nil
	}
	// On failure return cur as the fallback parent (per-VM cgroups still get
	// created there, just uncontrolled) alongside the error.
	sup := filepath.Join(cur, leaf)
	if err := os.Mkdir(sup, 0o750); err != nil && !os.IsExist(err) {
		return cur, fmt.Errorf("observability: create supervisor cgroup: %w", err)
	}
	if err := WriteCgroupControl(sup, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return cur, fmt.Errorf("observability: move self into supervisor cgroup: %w", err)
	}
	if err := WriteCgroupControl(cur, "cgroup.subtree_control", subtreeControllers); err != nil {
		return cur, fmt.Errorf("observability: enable cgroup controllers: %w", err)
	}
	return cur, nil
}

// WriteCgroupControl writes value to <dir>/<file>, a cgroup v2 control
// file (e.g. memory.max, cpu.max, cgroup.subtree_control). dir must be
// under the cgroup v2 root; file must be a bare name.
func WriteCgroupControl(dir, file, value string) error {
	if !strings.HasPrefix(dir, cgroupV2Root) {
		return fmt.Errorf("observability: cgroup dir %q outside %s", dir, cgroupV2Root)
	}
	if file == "" || strings.ContainsAny(file, "/\x00") {
		return fmt.Errorf("observability: invalid cgroup control file %q", file)
	}
	// The perm only applies if the file must be created, which never
	// happens for a real cgroup control file (the kernel owns it); it
	// matters only under a fake tempdir root in tests.
	// #nosec G304 — dir is validated under the cgroup root and file is a
	// bare cgroup v2 control-file name; no untrusted path component.
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0o600); err != nil {
		return fmt.Errorf("observability: write %s/%s: %w", dir, file, err)
	}
	return nil
}

// RemoveCgroup rmdir's a previously-created sub-cgroup. The
// kernel refuses rmdir while any process remains in the cgroup —
// callers should ensure the workload has exited before calling.
func RemoveCgroup(path string) error {
	if !strings.HasPrefix(path, cgroupV2Root+"/") {
		return fmt.Errorf("observability: refuse to rmdir %q (outside %s)", path, cgroupV2Root)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("observability: rmdir %s: %w", path, err)
	}
	return nil
}

// DelegationOK reports whether the caller can create child cgroups
// under its own cgroup v2 directory — i.e. whether its cgroup subtree
// is delegated and writable. It creates a uniquely-named probe
// sub-cgroup and immediately removes it; any failure (not cgroup v2,
// no delegation, the EPERM on an un-delegated `session.scope`) means
// the subtree cannot host the per-VM accounting cgroups the sandbox
// requires.
//
// Used as a startup precondition: the sandbox host self-wraps in a
// `systemd-run` scope (which delegates a fresh subtree) when this
// returns false, then re-checks. Per CONTRIBUTING "Probe preconditions
// at startup; no silent fallbacks", a host that still can't delegate
// after the wrap is a hard startup failure, not a /proc-only degrade.
func DelegationOK() bool {
	parent, err := CurrentCgroup()
	if err != nil {
		return false
	}
	dir, err := CreateSubCgroup(parent, fmt.Sprintf("daft-delegation-probe-%d", os.Getpid()))
	if err != nil {
		return false
	}
	// A leaked empty probe cgroup is harmless, but remove it so repeated
	// probes (e.g. the pre- and post-wrap checks) don't accumulate.
	_ = RemoveCgroup(dir)
	return true
}

// OpenCgroupFD returns an O_PATH fd for a cgroup directory,
// suitable for passing into syscall.SysProcAttr{UseCgroupFD: true,
// CgroupFD: fd} so the child is placed in the cgroup at the
// CLONE_INTO_CGROUP fork. Caller closes after Start.
func OpenCgroupFD(path string) (int, error) {
	fd, err := syscall.Open(path, unix.O_PATH|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("observability: open cgroup fd %s: %w", path, err)
	}
	return fd, nil
}

// readCgroupFile is the centralised file reader used by
// collectCgroupSample. Kept tiny on purpose: gosec G304 fires
// once here (with a single justification comment) instead of
// at every call site.
func readCgroupFile(dir, name string) ([]byte, error) {
	// #nosec G304 — dir is the absolute cgroup path validated by
	// CreateSubCgroup (or pinned by tests under t.TempDir); name
	// is a hard-coded cgroup v2 file like "cpu.stat". No untrusted
	// input feeds either component.
	return os.ReadFile(filepath.Join(dir, name))
}

// collectCgroupSample reads every interesting cgroup v2 file in
// path and returns a populated CgroupSample. Files that don't
// exist leave their sub-struct zero-valued — partial samples are
// more useful than no sample, since a cgroup whose parent didn't
// enable some controller still has a useful subset of files.
// Parse failures abort the whole sample (we don't want to silently
// hide kernel-version regressions in the schema). Returns a non-
// nil error only when the cgroup directory itself is missing.
func collectCgroupSample(path string) (*CgroupSample, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	out := &CgroupSample{Path: path}
	if data, err := readCgroupFile(path, "cpu.stat"); err == nil {
		if perr := parseCgroupCPU(data, &out.CPU); perr != nil {
			return nil, fmt.Errorf("observability: parse cpu.stat: %w", perr)
		}
	}
	if data, err := readCgroupFile(path, "memory.current"); err == nil {
		v, perr := parseUintLine(data)
		if perr != nil {
			return nil, fmt.Errorf("observability: parse memory.current: %w", perr)
		}
		out.Memory.CurrentBytes = v
	}
	if data, err := readCgroupFile(path, "memory.peak"); err == nil {
		v, perr := parseUintLine(data)
		if perr != nil {
			return nil, fmt.Errorf("observability: parse memory.peak: %w", perr)
		}
		out.Memory.PeakBytes = v
	}
	if data, err := readCgroupFile(path, "memory.max"); err == nil {
		v, perr := parseCgroupMax(data)
		if perr != nil {
			return nil, fmt.Errorf("observability: parse memory.max: %w", perr)
		}
		out.Memory.MaxBytes = v
	}
	if data, err := readCgroupFile(path, "memory.swap.current"); err == nil {
		v, perr := parseUintLine(data)
		if perr != nil {
			return nil, fmt.Errorf("observability: parse memory.swap.current: %w", perr)
		}
		out.Memory.SwapCurrentBytes = v
	}
	if data, err := readCgroupFile(path, "io.stat"); err == nil {
		devs, perr := parseCgroupIO(data)
		if perr != nil {
			return nil, fmt.Errorf("observability: parse io.stat: %w", perr)
		}
		out.IO = devs
	}
	if data, err := readCgroupFile(path, "pids.current"); err == nil {
		v, perr := parseUintLine(data)
		if perr != nil {
			return nil, fmt.Errorf("observability: parse pids.current: %w", perr)
		}
		out.Pids.Current = v
	}
	return out, nil
}

// parseCgroupCPU parses cpu.stat — a sequence of `<key> <uint>`
// lines. Unknown keys are skipped silently because the set of
// emitted keys grows across kernel versions and we don't want a
// new key to fail the whole sample.
func parseCgroupCPU(b []byte, out *CgroupCPU) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return fmt.Errorf("cpu.stat field %s: %w", key, err)
		}
		switch key {
		case "usage_usec":
			out.UsageUsec = v
		case "user_usec":
			out.UserUsec = v
		case "system_usec":
			out.SystemUsec = v
		case "nr_periods":
			out.NRPeriods = v
		case "nr_throttled":
			out.NRThrottled = v
		case "throttled_usec":
			out.ThrottledUsec = v
		}
	}
	return sc.Err()
}

// parseCgroupIO parses io.stat. Each line is
// `<major>:<minor> <pairs>` where pairs is a sequence of
// `name=value` tokens (rbytes, wbytes, rios, wios, dbytes, dios,
// plus future per-controller additions we ignore).
func parseCgroupIO(b []byte) ([]CgroupIODev, error) {
	var out []CgroupIODev
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		dev := CgroupIODev{Device: fields[0]}
		for _, f := range fields[1:] {
			key, val, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			v, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("io.stat %s on %s: %w", key, dev.Device, err)
			}
			switch key {
			case "rbytes":
				dev.RBytes = v
			case "wbytes":
				dev.WBytes = v
			case "rios":
				dev.RIOs = v
			case "wios":
				dev.WIOs = v
			case "dbytes":
				dev.DBytes = v
			case "dios":
				dev.DIOs = v
			}
		}
		out = append(out, dev)
	}
	return out, sc.Err()
}

// parseUintLine parses a single-uint cgroup file like
// memory.current / memory.peak / pids.current. The file content
// is one decimal integer, optionally with a trailing newline.
func parseUintLine(b []byte) (uint64, error) {
	s := strings.TrimSpace(string(b))
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// cgroupMaxSentinel is the literal cgroup v2 file content
// representing "no limit" for memory.max / pids.max / etc.
const cgroupMaxSentinel = "max"

// parseCgroupMax handles memory.max / pids.max etc. — files that
// hold either a decimal uint or the literal "max". The sentinel
// means no limit; we encode it as -1 so the JSON consumer doesn't
// have to special-case a string.
func parseCgroupMax(b []byte) (int64, error) {
	s := strings.TrimSpace(string(b))
	if s == cgroupMaxSentinel {
		return -1, nil
	}
	v, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return 0, err
	}
	return int64(v), nil
}

// Compile-time assertion that io.Reader is reachable from this
// file — it's used via bytes.NewReader. Keeps the import live
// even if a future edit removes the only user.
var _ io.Reader = (*bytes.Reader)(nil)
