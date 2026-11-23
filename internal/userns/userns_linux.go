//go:build linux

package userns

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// EnvMarker is set on the re-exec'd child so subsequent calls to
// Reexec recognise themselves as the child and skip the spawn.
// Exported so tests (and tools that need to fork sub-processes
// that should NOT re-enter userns logic) can set/inspect it.
const EnvMarker = "VMJAIL_IN_USERNS"

// EnvDisable, when set to "1" before sandboxd starts, skips the
// userns re-exec entirely. Intended for debugging and for
// container-host environments that already provide an equivalent
// userns to the calling process.
const EnvDisable = "VMJAIL_DISABLE_USERNS"

// SubBase / SubSize describe the host id range we expect to
// inherit from /etc/subuid + /etc/subgid for the calling user.
// 100000:65536 is the convention `useradd` allocates on most
// distros (and what NixOS's `subordinate-id` module defaults to);
// readSubIDs uses these as fallbacks if the etc-file parse turns
// up empty for the current user.
const (
	SubBase = 100000
	SubSize = 65536
)

// childSyncFD is the file descriptor the parent uses to release
// the re-exec'd child once newuidmap/newgidmap have populated
// the child's id maps. Fd 3 is the first slot above stdio/err
// and the only one we pass via cmd.ExtraFiles.
const childSyncFD = 3

// newuidmapCandidates / newgidmapCandidates list the paths
// Reexec searches for the setuid helpers. NixOS wraps them
// under /run/wrappers/bin; most other distros put them in
// /usr/bin. Ordered most-specific-first so the wrapped version
// wins on NixOS.
//
//nolint:gochecknoglobals // const-like; overridable for tests.
var (
	newuidmapCandidates = []string{
		"/run/wrappers/bin/newuidmap",
		"/usr/bin/newuidmap",
	}
	newgidmapCandidates = []string{
		"/run/wrappers/bin/newgidmap",
		"/usr/bin/newgidmap",
	}
)

// RootMappedMountNamespace configures a child with a private mount namespace
// and the current effective UID and GID mapped to root. Go's fork/exec path
// installs the maps and makes mounts recursively private before exec.
func RootMappedMountNamespace() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Unshareflags:               syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
}

// Reexec ensures the calling process runs inside a user
// namespace populated with a multi-range mapping derived from
// the current user's /etc/subuid + /etc/subgid entries.
//
// In the initial-userns call (no EnvMarker): spawns a child
// with CLONE_NEWUSER, writes the maps via newuidmap/newgidmap,
// forwards signals to the child, waits, and calls os.Exit with
// the child's exit code. Never returns to the caller on the
// happy path; returns an error only when the re-exec couldn't
// be set up (caller should treat as fatal).
//
// In the child call (EnvMarker == "1"): waits on the sync fd
// for the parent to finish populating the maps, then returns
// nil so the caller's main() can proceed.
//
// On Linux with EnvDisable == "1", returns nil immediately —
// the caller stays in the initial userns. Useful for tests and
// for nested-container deployments where the outer userns is
// already what we want.
func Reexec(stderr io.Writer) error {
	if os.Getenv(EnvDisable) == "1" {
		return nil
	}
	if os.Getenv(EnvMarker) == "1" {
		return childWaitForMaps()
	}
	_ = stderr // stderr is reserved for future warning paths.
	exitCode, err := parentReexecRun()
	if err != nil {
		return err
	}
	osExitFn(exitCode)
	// Unreachable in production; reachable from a unit test
	// that swaps osExitFn for a no-op so the parent-success
	// branch can be exercised.
	return nil
}

// osExitFn is the test seam Reexec uses to call os.Exit after a
// successful re-exec. Tests substitute a no-op so the parent-
// success branch can be exercised without terminating the
// test binary.
//
//nolint:gochecknoglobals // single test seam.
var osExitFn = os.Exit

// childWaitForMaps blocks until the parent finishes writing the
// child's uid_map/gid_map and signals via the sync pipe. Without
// the wait, child-side code would race the parent and see
// itself as uid 65534 (overflow) until the maps land.
func childWaitForMaps() error {
	f := os.NewFile(childSyncFD, "userns-sync")
	if f == nil {
		return errors.New("userns: child missing sync fd (env says we are the re-exec but fd 3 is unset)")
	}
	defer func() { _ = f.Close() }()
	var buf [1]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return fmt.Errorf("userns: read sync byte: %w", err)
	}
	return nil
}

// parentReexecRun does the actual spawn-maps-wait dance and
// returns (childExitCode, nil) on the happy path or
// (0, err) when setup fails before the child can run. Test
// seams (mapsWriter, signalsWaiter) are package-level vars so
// a test can substitute in fakes that don't require real
// newuidmap/newgidmap.
func parentReexecRun() (int, error) {
	uidRanges, gidRanges, err := readSubIDs(os.Getuid(), os.Getgid())
	if err != nil {
		return 0, fmt.Errorf("userns: read sub id ranges: %w", err)
	}
	newuidmap, err := firstExisting(newuidmapCandidates)
	if err != nil {
		return 0, fmt.Errorf("userns: locate newuidmap: %w", err)
	}
	newgidmap, err := firstExisting(newgidmapCandidates)
	if err != nil {
		return 0, fmt.Errorf("userns: locate newgidmap: %w", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("userns: pipe: %w", err)
	}

	cmd := buildReexecCmd(r)
	if startErr := cmd.Start(); startErr != nil {
		_ = r.Close()
		_ = w.Close()
		return 0, fmt.Errorf("userns: start re-exec: %w", startErr)
	}
	// Parent doesn't read the sync fd.
	_ = r.Close()

	pid := cmd.Process.Pid
	if mapErr := writeIDMap(newuidmap, pid, uidRanges); mapErr != nil {
		_ = w.Close()
		killChild(cmd)
		return 0, mapErr
	}
	if mapErr := writeIDMap(newgidmap, pid, gidRanges); mapErr != nil {
		_ = w.Close()
		killChild(cmd)
		return 0, mapErr
	}

	// Release the child.
	if _, writeErr := w.Write([]byte{1}); writeErr != nil {
		_ = w.Close()
		killChild(cmd)
		return 0, fmt.Errorf("userns: release child: %w", writeErr)
	}
	_ = w.Close()

	return waitWithSignalForwarding(cmd), nil
}

// buildReexecCmd is the package-level seam parentReexecRun
// invokes to construct the child's *exec.Cmd. Defaults to
// defaultBuildReexecCmd; tests substitute a stub that returns
// a `/bin/sh -c exit 0` cmd so the spawn-maps-release flow can
// be exercised without recursing into the test binary.
//
//nolint:gochecknoglobals // single test seam.
var buildReexecCmd = defaultBuildReexecCmd

// defaultBuildReexecCmd composes the *exec.Cmd that
// parentReexec spawns: same binary, same argv, plus the
// EnvMarker env hint and an unshare-newuser SysProcAttr. The
// sync pipe's read end is plumbed in as ExtraFiles[0] (= fd 3
// in the child).
func defaultBuildReexecCmd(syncFD *os.File) *exec.Cmd {
	//nolint:gosec // /proc/self/exe is our own binary.
	//vmjail:lint:disable=commandcontext reason="the re-exec is the supervised workload itself; waitWithSignalForwarding provides lifetime and liveness control"
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Env = append(os.Environ(), EnvMarker+"=1")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{syncFD}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
	}
	return cmd
}

// waitWithSignalForwarding installs a signal handler that
// forwards every catchable signal to the child, then blocks on
// cmd.Wait(). Returns the child's exit code (or 1 if Wait
// produced a non-ExitError failure).
func waitWithSignalForwarding(cmd *exec.Cmd) int {
	sigs := make(chan os.Signal, 1)
	// signal.Notify with no signals listed catches every catchable
	// signal — i.e. everything except SIGKILL / SIGSTOP. That's
	// the right scope: anything the parent sees we want the child
	// to see too.
	signal.Notify(sigs)
	defer signal.Stop(sigs)

	done := make(chan struct{})
	go func() {
		//vmjail:lint:disable=forloopbound reason="bounded by close(done) when cmd.Wait returns"
		for {
			select {
			case s := <-sigs:
				if s == nil {
					return
				}
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	close(done)
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

// killChild reaps the child after a fatal parent-side error.
// Best-effort: a successfully-killed child contributes the
// SIGKILL exit status to cmd.Wait, which we discard.
func killChild(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// writeIDMap invokes newuidmap or newgidmap with the
// (pid, inside, outside, count) triples drawn from ranges. One
// invocation writes the entire map atomically.
func writeIDMap(binary string, pid int, ranges []IDRange) error {
	args := make([]string, 0, 1+3*len(ranges))
	args = append(args, strconv.Itoa(pid))
	for _, r := range ranges {
		args = append(
			args,
			strconv.Itoa(r.Inside),
			strconv.Itoa(r.Outside),
			strconv.Itoa(r.Size),
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	//nolint:gosec // binary is one of the const-vetted helper paths above.
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("userns: %s %v: %w: %s",
			binary, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IDRange is a single (inside, outside, count) entry for a
// uid_map or gid_map. Same shape newuidmap accepts on the CLI.
type IDRange struct {
	Inside  int
	Outside int
	Size    int
}

// readSubIDs builds the multi-range mapping we want for sandboxd:
//
//   - Inside 0 ↔ Outside hostUID/hostGID (size 1): the calling
//     user maps to root in the new namespace. Bundle files
//     unpacked as host uid 1000 still appear as ns uid 0 inside,
//     which is what every subsequent caller (virtiofsd, crun)
//     expects to see for "the owner".
//   - Inside 1 ↔ Outside subBase (size subSize): the host
//     subuid/subgid range maps to non-root namespace ids. This
//     is what gives sandboxd the room to chown bundle contents
//     into the subrange, run nested userns helpers, etc.
//
// subBase + subSize default to SubBase/SubSize but can shrink
// to the actual /etc/subuid + /etc/subgid entry for the calling
// user when one is present. An entry must be at least 2 wide
// (the calling user's id maps to ns 1; the rest of the
// subrange covers ns 2+).
func readSubIDs(hostUID, hostGID int) (uidRanges, gidRanges []IDRange, err error) {
	return readSubIDsFrom("/etc/subuid", "/etc/subgid", hostUID, hostGID)
}

func readSubIDsFrom(subUIDPath, subGIDPath string, hostUID, hostGID int) (uidRanges, gidRanges []IDRange, err error) {
	subUID, err := readSubIDFile(subUIDPath, hostUID)
	if err != nil {
		return nil, nil, fmt.Errorf("uid: %w", err)
	}
	subGID, err := readSubIDFile(subGIDPath, hostGID)
	if err != nil {
		return nil, nil, fmt.Errorf("gid: %w", err)
	}
	uidRanges = []IDRange{
		{Inside: 0, Outside: hostUID, Size: 1},
		{Inside: 1, Outside: subUID.start, Size: subUID.size},
	}
	gidRanges = []IDRange{
		{Inside: 0, Outside: hostGID, Size: 1},
		{Inside: 1, Outside: subGID.start, Size: subGID.size},
	}
	return uidRanges, gidRanges, nil
}

// subEntry holds the parsed `<name>:<start>:<size>` line from
// /etc/subuid or /etc/subgid for the current user.
type subEntry struct {
	start, size int
}

// readSubIDFile picks the entry for hostID — matched either by
// numeric uid or by the resolved username via the password
// database. Returns the parsed range, or the SubBase/SubSize
// defaults if no entry is present (a noisy warning would be
// kinder, but most callers want a sensible default to keep
// going).
func readSubIDFile(path string, hostID int) (subEntry, error) {
	f, err := os.Open(path) //nolint:gosec // const-path: /etc/subuid or /etc/subgid.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return subEntry{start: SubBase, size: SubSize}, nil
		}
		return subEntry{}, err
	}
	defer func() { _ = f.Close() }()

	wantNum := strconv.Itoa(hostID)
	wantName, _ := lookupName(hostID, path == "/etc/subgid")
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		name, startStr, sizeStr := parts[0], parts[1], parts[2]
		if name != wantNum && name != wantName {
			continue
		}
		start, err := strconv.Atoi(startStr)
		if err != nil {
			return subEntry{}, fmt.Errorf("%s: bad start %q: %w", path, startStr, err)
		}
		size, err := strconv.Atoi(sizeStr)
		if err != nil {
			return subEntry{}, fmt.Errorf("%s: bad size %q: %w", path, sizeStr, err)
		}
		if size < 2 {
			return subEntry{}, fmt.Errorf(
				"%s: entry for %s has size %d < 2 (need at least 2 ns ids)",
				path, name, size,
			)
		}
		return subEntry{start: start, size: size}, nil
	}
	if err := scanner.Err(); err != nil {
		return subEntry{}, err
	}
	// No entry — fall back to convention. newuidmap will fail
	// with a clear message at re-exec time if the kernel
	// refuses the mapping.
	return subEntry{start: SubBase, size: SubSize}, nil
}

// lookupName resolves a uid or gid to its name via the system's
// passwd/group database. Best-effort: an error returns the
// empty string, which makes the numeric match the only path in
// readSubIDFile's loop.
func lookupName(id int, isGroup bool) (string, error) {
	if isGroup {
		return lookupNameFile("/etc/group", id)
	}
	return lookupNameFile("/etc/passwd", id)
}

func lookupNameFile(path string, id int) (string, error) {
	f, err := os.Open(path) //nolint:gosec // const-path: /etc/passwd or /etc/group.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	target := strconv.Itoa(id)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, ":")
		if len(parts) >= 3 && parts[2] == target {
			return parts[0], nil
		}
	}
	return "", scanner.Err()
}

// firstExisting returns the first existing path in candidates,
// or an error if none exist.
func firstExisting(candidates []string) (string, error) {
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %v exist", candidates)
}
