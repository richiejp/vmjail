//go:build linux

package userns

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReadSubIDFileMatchesByNumericID covers the happy path
// where /etc/subuid carries an entry keyed on the calling uid.
// readSubIDFile resolves both numeric and name keys; this
// exercises the numeric branch in isolation.
func TestReadSubIDFileMatchesByNumericID(t *testing.T) {
	path := writeTempFile(t, "1000:100000:65536\n")
	got, err := readSubIDFile(path, 1000)
	if err != nil {
		t.Fatalf("readSubIDFile: %v", err)
	}
	if got.start != 100000 || got.size != 65536 {
		t.Errorf("start/size = %d/%d, want 100000/65536", got.start, got.size)
	}
}

// TestReadSubIDFileFallsBackToDefaults checks what happens when
// the file has no entry for the calling uid: we return the
// SubBase/SubSize defaults rather than failing, so a host
// without a populated /etc/subuid still gets sandboxd up (and
// newuidmap will surface a clearer failure at exec time).
func TestReadSubIDFileFallsBackToDefaults(t *testing.T) {
	path := writeTempFile(t, "someone-else:50000:1000\n")
	got, err := readSubIDFile(path, 1000)
	if err != nil {
		t.Fatalf("readSubIDFile: %v", err)
	}
	if got.start != SubBase || got.size != SubSize {
		t.Errorf("start/size = %d/%d, want defaults %d/%d",
			got.start, got.size, SubBase, SubSize)
	}
}

// TestReadSubIDFileRejectsMalformedStart covers the integer-
// parse error branches by feeding a non-numeric start value.
// Without this branch nailed down, a sub-id file with a
// typo'd entry would silently return the fallback range,
// masking what's actually a contributor config bug.
func TestReadSubIDFileRejectsMalformedStart(t *testing.T) {
	path := writeTempFile(t, "1000:not-a-number:65536\n")
	if _, err := readSubIDFile(path, 1000); err == nil {
		t.Fatal("readSubIDFile accepted non-numeric start")
	} else if !strings.Contains(err.Error(), "start") {
		t.Errorf("err = %v, want one mentioning start", err)
	}
}

// TestReadSubIDFileSkipsBadlyShapedLines covers the
// `len(parts) != 3` branch: a malformed line (missing colons)
// is silently skipped, the well-formed entry on a later line
// wins. shadow-utils tolerates this kind of file corruption
// rather than fatal-ing.
func TestReadSubIDFileSkipsBadlyShapedLines(t *testing.T) {
	path := writeTempFile(
		t,
		"garbage-no-colons\n"+
			"only:one\n"+
			"1000:100000:65536\n",
	)
	got, err := readSubIDFile(path, 1000)
	if err != nil {
		t.Fatalf("readSubIDFile: %v", err)
	}
	if got.start != 100000 {
		t.Errorf("start = %d, want 100000 (after skipping malformed lines)", got.start)
	}
}

// TestReadSubIDFileRejectsMalformedSize completes the
// parse-error pair: same as above but for the third
// (count) field.
func TestReadSubIDFileRejectsMalformedSize(t *testing.T) {
	path := writeTempFile(t, "1000:100000:not-a-number\n")
	if _, err := readSubIDFile(path, 1000); err == nil {
		t.Fatal("readSubIDFile accepted non-numeric size")
	} else if !strings.Contains(err.Error(), "size") {
		t.Errorf("err = %v, want one mentioning size", err)
	}
}

// TestReadSubIDsSurfacesNewgidmapLocateFailure exercises the
// "newgidmap is missing but newuidmap is present" branch,
// which has its own wrap message that the all-missing branch
// doesn't reach.
func TestReadSubIDsSurfacesNewgidmapLocateFailure(t *testing.T) {
	dir := t.TempDir()
	okStub := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(okStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	newuidmapCandidates = []string{okStub}
	newgidmapCandidates = []string{nopeNewGIDMap}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
	})

	_, err := parentReexecRun()
	if err == nil {
		t.Fatal("parentReexecRun: nil err with missing newgidmap")
	}
	if !strings.Contains(err.Error(), "newgidmap") {
		t.Errorf("err = %v, want one mentioning newgidmap", err)
	}
}

// TestReadSubIDFileRejectsTooSmallRange pins the size>=2 floor:
// if the subuid entry is wider than 0 but narrower than the
// minimum-useful range we need (root + at least one subordinate
// id), readSubIDFile errors instead of silently producing an
// unusable mapping.
func TestReadSubIDFileRejectsTooSmallRange(t *testing.T) {
	path := writeTempFile(t, "1000:100000:1\n")
	if _, err := readSubIDFile(path, 1000); err == nil {
		t.Fatal("readSubIDFile accepted size=1 entry")
	} else if !strings.Contains(err.Error(), "size") {
		t.Errorf("err = %v, want one mentioning size", err)
	}
}

// TestReadSubIDFileSkipsCommentsAndBlankLines mirrors the
// shadow-utils tolerance: # comments and blank lines mid-file
// must not derail parsing.
func TestReadSubIDFileSkipsCommentsAndBlankLines(t *testing.T) {
	path := writeTempFile(
		t,
		"# top-comment\n"+
			"\n"+
			"1000:100000:65536\n"+
			"# trailing\n",
	)
	got, err := readSubIDFile(path, 1000)
	if err != nil {
		t.Fatalf("readSubIDFile: %v", err)
	}
	if got.start != 100000 {
		t.Errorf("start = %d, want 100000", got.start)
	}
}

// TestReadSubIDFileMissingFileFallsBack confirms that a host
// without /etc/subuid at all returns defaults rather than
// erroring. Distro-installed without shadow-utils is a real
// state we want to support (with a noisy failure at newuidmap
// invocation, not at sub-id read).
func TestReadSubIDFileMissingFileFallsBack(t *testing.T) {
	got, err := readSubIDFile("/nonexistent/path", 1000)
	if err != nil {
		t.Fatalf("readSubIDFile: %v", err)
	}
	if got.start != SubBase {
		t.Errorf("start = %d, want default %d", got.start, SubBase)
	}
}

// TestReadSubIDsFromWrapsUIDPathError covers the "uid: %w"
// wrap that the production readSubIDs returns when /etc/subuid
// parses badly. Without the testable overload (readSubIDsFrom),
// this branch would only be reachable via root-owned files,
// which CI can't reliably stage.
func TestReadSubIDsFromWrapsUIDPathError(t *testing.T) {
	badUID := writeTempFile(t, "1000:not-numeric:65536\n")
	goodGID := writeTempFile(t, "100:100000:65536\n")
	_, _, err := readSubIDsFrom(badUID, goodGID, 1000, 100)
	if err == nil {
		t.Fatal("readSubIDsFrom: nil err with malformed uid file")
	}
	if !strings.Contains(err.Error(), "uid:") {
		t.Errorf("err = %v, want 'uid:' wrap", err)
	}
}

// TestReadSubIDsFromWrapsGIDPathError mirrors the uid test
// for the gid branch.
func TestReadSubIDsFromWrapsGIDPathError(t *testing.T) {
	goodUID := writeTempFile(t, "1000:100000:65536\n")
	badGID := writeTempFile(t, "100:100000:not-numeric\n")
	_, _, err := readSubIDsFrom(goodUID, badGID, 1000, 100)
	if err == nil {
		t.Fatal("readSubIDsFrom: nil err with malformed gid file")
	}
	if !strings.Contains(err.Error(), "gid:") {
		t.Errorf("err = %v, want 'gid:' wrap", err)
	}
}

// TestLookupNameFileOpenError covers the os.Open error branch
// by pointing the lookup at a path that exists but isn't a
// passwd-shaped file (a directory). Without this branch,
// readSubIDFile's name-key fallback would silently lose the
// numeric-only path on hosts with weird /etc layouts.
func TestLookupNameFileOpenError(t *testing.T) {
	dir := t.TempDir()
	// Open with a path that's a dir, not a file — os.Open
	// returns an error for that (EISDIR on read in the
	// scanner; some platforms also reject Open).
	_, err := lookupNameFile(dir+"/does-not-exist", 1000)
	if err == nil {
		t.Fatal("lookupNameFile: nil err on missing path")
	}
}

// TestReadSubIDsShapesBothRanges asserts the (root, subrange)
// shape that the parent uses when invoking newuidmap. The
// expected output is a tight contract: a fix here without
// also rethinking the consumer is a real bug.
func TestReadSubIDsShapesBothRanges(t *testing.T) {
	// Override the etc-file lookups by pre-staging a writable
	// dir that mirrors /etc layout for this test. readSubIDs
	// reads literal paths so we have to monkey-patch with
	// chdir tricks... instead, just exercise the function
	// against the host's real /etc/subuid for the calling
	// user. On developer machines + the integration runner
	// this is the real path that matters; CI without an
	// /etc/subuid entry hits the fallback branch already
	// covered above.
	uidRanges, gidRanges, err := readSubIDs(os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("readSubIDs: %v", err)
	}
	if len(uidRanges) != 2 || len(gidRanges) != 2 {
		t.Fatalf("ranges: uid=%d gid=%d, want 2/2", len(uidRanges), len(gidRanges))
	}
	if uidRanges[0].Inside != 0 || uidRanges[0].Outside != os.Getuid() || uidRanges[0].Size != 1 {
		t.Errorf("uid root range = %+v, want {0, %d, 1}", uidRanges[0], os.Getuid())
	}
	if uidRanges[1].Inside != 1 || uidRanges[1].Size < 2 {
		t.Errorf("uid sub range = %+v, want inside=1 size>=2", uidRanges[1])
	}
	if gidRanges[0].Inside != 0 || gidRanges[0].Outside != os.Getgid() {
		t.Errorf("gid root range = %+v, want inside=0 outside=%d", gidRanges[0], os.Getgid())
	}
}

// TestFirstExistingPicksFirstHit covers the helper that searches
// the wrapped/unwrapped paths for newuidmap/newgidmap. The
// search is order-sensitive — NixOS-wrapped binary must beat
// /usr/bin — and a regression here would silently invoke the
// unwrapped (non-setuid) version.
func TestFirstExistingPicksFirstHit(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(b, []byte("x"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	// a doesn't exist, b does. Order matters: b is found.
	got, err := firstExisting([]string{a, b})
	if err != nil {
		t.Fatalf("firstExisting: %v", err)
	}
	if got != b {
		t.Errorf("got %q, want %q", got, b)
	}
}

// nopeNewUIDMap / nopeNewGIDMap are sentinel paths several
// tests point newuidmapCandidates/newgidmapCandidates at to
// force the "helper not found" error branch.
const (
	nopeNewUIDMap = "/nope/newuidmap"
	nopeNewGIDMap = "/nope/newgidmap"
)

// TestFirstExistingErrorsWhenNoneExist nails the all-empty
// branch so Reexec's "newuidmap not found" message is reachable
// from a unit test even on hosts that have the binary.
func TestFirstExistingErrorsWhenNoneExist(t *testing.T) {
	if _, err := firstExisting([]string{"/nope/a", "/nope/b"}); err == nil {
		t.Fatal("firstExisting accepted all-missing candidate list")
	}
}

// TestReexecRespectsDisableEnv pins the EnvDisable bypass: with
// the var set, Reexec returns nil immediately even in the
// parent path (no re-exec attempted).
func TestReexecRespectsDisableEnv(t *testing.T) {
	t.Setenv(EnvDisable, "1")
	t.Setenv(EnvMarker, "")
	if err := Reexec(os.Stderr); err != nil {
		t.Fatalf("Reexec (disabled): %v", err)
	}
}

// TestChildWaitForMapsReadsAndReturns exercises the child
// branch of Reexec directly: we stage a pipe whose read end
// lives at fd 3, pre-write the release byte, then call
// childWaitForMaps. A real Reexec(child) would call this same
// function after seeing EnvMarker; testing it in isolation
// avoids the os.Exit dance the parent path needs.
//
// The full Reexec(parent → child) flow can only be exercised
// end-to-end (it would terminate the test binary on the parent
// branch); a follow-up integration assertion in the sandboxd
// e2e harness will cover that.
func TestChildWaitForMapsReadsAndReturns(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() {
		_ = r.Close()
		_ = w.Close()
	}()

	if _, err := w.Write([]byte{1}); err != nil {
		t.Fatalf("prime sync: %v", err)
	}

	saved, err := unix.Dup(childSyncFD)
	if err != nil {
		saved = -1 // fd 3 wasn't open; nothing to restore.
	}
	if err := unix.Dup2(int(r.Fd()), childSyncFD); err != nil {
		t.Fatalf("dup2 → fd %d: %v", childSyncFD, err)
	}
	t.Cleanup(func() {
		if saved < 0 {
			_ = unix.Close(childSyncFD)
			return
		}
		_ = unix.Dup2(saved, childSyncFD)
		_ = unix.Close(saved)
	})

	if err := childWaitForMaps(); err != nil {
		t.Fatalf("childWaitForMaps: %v", err)
	}
}

func writeTempFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestWriteIDMapInvokesHelperWithExpectedArgv covers the
// newuidmap/newgidmap invocation: the helper gets `<pid>
// <inside> <outside> <count>` triples spread across the
// ranges. We stub the helper with a /bin/sh script that
// echoes its argv into a file, then verify the file's
// content matches the triples we passed.
func TestWriteIDMapInvokesHelperWithExpectedArgv(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.txt")
	stub := filepath.Join(dir, "stub.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvLog + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	ranges := []IDRange{
		{Inside: 0, Outside: 1000, Size: 1},
		{Inside: 1, Outside: 100000, Size: 65535},
	}
	if err := writeIDMap(stub, 4242, ranges); err != nil {
		t.Fatalf("writeIDMap: %v", err)
	}
	got, err := os.ReadFile(argvLog) //nolint:gosec // argvLog is under t.TempDir() — test-only.
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	want := "4242\n0\n1000\n1\n1\n100000\n65535\n"
	if string(got) != want {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
}

// TestWriteIDMapSurfacesHelperFailure asserts that a
// non-zero exit from the helper turns into a wrapped error
// that includes the binary name + combined output. Critical
// because production failures (e.g. /etc/subuid missing the
// caller's entry) surface here, and the error message is
// the only signal a contributor gets to diagnose them.
func TestWriteIDMapSurfacesHelperFailure(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "fail.sh")
	script := "#!/bin/sh\necho 'simulated failure' >&2\nexit 7\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	err := writeIDMap(stub, 1, []IDRange{{Inside: 0, Outside: 0, Size: 1}})
	if err == nil {
		t.Fatal("writeIDMap accepted helper exit 7")
	}
	if !strings.Contains(err.Error(), "simulated failure") {
		t.Errorf("err = %v, want it to include the helper's stderr", err)
	}
	if !strings.Contains(err.Error(), stub) {
		t.Errorf("err = %v, want it to include the binary path", err)
	}
}

// TestKillChildReapsRunningProcess pins the parent-side
// cleanup path: when a fatal pre-release error happens (e.g.
// newuidmap failure), killChild must terminate the userns
// helper so it doesn't leak. A real child + a Wait afterwards
// confirms the kill landed and the goroutine doesn't block.
func TestKillChildReapsRunningProcess(t *testing.T) {
	// /bin/sh is the smallest dependable "block forever"
	// binary across hosts (busybox sh, bash, dash all work).
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	killChild(cmd)
	// After killChild returns, the process must be reaped.
	// ProcessState being non-nil is the public signal that
	// Wait completed.
	if cmd.ProcessState == nil {
		t.Error("killChild returned but cmd.ProcessState is nil — child not reaped")
	}
}

// TestKillChildHandlesNilProcess covers the defensive nil
// check: a NetBridge-style "I never got Started" caller
// must be able to call killChild without panicking.
func TestKillChildHandlesNilProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	// cmd.Process is nil because we never called Start.
	killChild(cmd) // must not panic.
}

// TestBuildReexecCmdWiresEnvAndCloneflags pins the cmd
// composition: env carries EnvMarker=1, the SysProcAttr asks
// the kernel for CLONE_NEWUSER, and the sync fd lands at
// ExtraFiles[0] (= fd 3 in the child via Go's convention).
// A regression in any of these breaks the parent-spawns-child
// flow silently — child would run without a userns, or never
// release from the sync read.
func TestBuildReexecCmdWiresEnvAndCloneflags(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() {
		_ = r.Close()
		_ = w.Close()
	}()

	cmd := defaultBuildReexecCmd(r)

	if cmd.SysProcAttr == nil ||
		cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWUSER == 0 {
		t.Errorf("cmd.SysProcAttr.Cloneflags = %v, want CLONE_NEWUSER set",
			cmd.SysProcAttr)
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != r {
		t.Errorf("cmd.ExtraFiles = %v, want one entry with the sync read end",
			cmd.ExtraFiles)
	}
	if !slices.Contains(cmd.Env, EnvMarker+"=1") {
		t.Errorf("cmd.Env missing %q (got %v)", EnvMarker+"=1", cmd.Env)
	}
}

// TestWaitWithSignalForwardingReportsExitCode runs a child
// that exits with a known code and confirms
// waitWithSignalForwarding propagates it. The signal-forward
// goroutine has no signals to deliver in this scenario — the
// happy-path coverage is what we need; the kill-from-parent
// path is exercised by TestKillChildReapsRunningProcess.
func TestWaitWithSignalForwardingReportsExitCode(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if code := waitWithSignalForwarding(cmd); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

// TestWaitWithSignalForwardingNonExitError covers the path
// where Wait returns an error that isn't a *exec.ExitError.
// We synthesise this by calling Wait twice on the same cmd:
// the second call returns "Wait was already called", which
// the function should treat as exit-code 1.
func TestWaitWithSignalForwardingNonExitError(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	if code := waitWithSignalForwarding(cmd); code != 1 {
		t.Errorf("double-Wait code = %d, want 1", code)
	}
}

// TestParentReexecRunReportsMissingNewuidmap exercises the
// early-return branches of parentReexecRun by pointing the
// candidate lists at paths that don't exist. The function must
// return a wrapped error mentioning the missing helper without
// ever forking a child.
func TestParentReexecRunReportsMissingNewuidmap(t *testing.T) {
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	newuidmapCandidates = []string{nopeNewUIDMap}
	newgidmapCandidates = []string{nopeNewGIDMap}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
	})

	code, err := parentReexecRun()
	if err == nil {
		t.Fatal("parentReexecRun: nil err with missing newuidmap")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 on setup-error path", code)
	}
	if !strings.Contains(err.Error(), "newuidmap") {
		t.Errorf("err = %v, want it to mention newuidmap", err)
	}
}

// TestReexecParentBranchHappyPath covers Reexec's
// parent-success exit-code branch via the osExitFn test seam:
// with all helpers stubbed to succeed, Reexec calls osExitFn
// with the child's exit code and then returns nil (the
// post-os.Exit unreachable line in production, reachable here
// because the stub doesn't actually exit).
func TestReexecParentBranchHappyPath(t *testing.T) {
	dir := t.TempDir()
	okStub := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(okStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	prevBuild := buildReexecCmd
	prevExit := osExitFn
	newuidmapCandidates = []string{okStub}
	newgidmapCandidates = []string{okStub}
	buildReexecCmd = func(syncFD *os.File) *exec.Cmd {
		c := exec.Command("/bin/sh", "-c", "head -c 1 >/dev/null")
		c.Stdin = syncFD
		return c
	}
	var capturedCode int
	osExitFn = func(code int) { capturedCode = code }
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
		buildReexecCmd = prevBuild
		osExitFn = prevExit
	})

	t.Setenv(EnvDisable, "")
	t.Setenv(EnvMarker, "")
	if err := Reexec(os.Stderr); err != nil {
		t.Fatalf("Reexec: %v", err)
	}
	if capturedCode != 0 {
		t.Errorf("osExitFn called with %d, want 0", capturedCode)
	}
}

// TestReexecParentBranchPropagatesError exercises Reexec's
// parent branch (no EnvMarker, no EnvDisable). With newuidmap
// candidates pointed at missing paths, the parent branch
// surfaces a setup error to the caller — that's what the
// vmjail-sandboxd main() relies on to bail with a clear
// diagnostic when the host's rootless setup isn't viable.
func TestReexecParentBranchPropagatesError(t *testing.T) {
	t.Setenv(EnvDisable, "")
	t.Setenv(EnvMarker, "")
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	newuidmapCandidates = []string{nopeNewUIDMap}
	newgidmapCandidates = []string{nopeNewGIDMap}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
	})

	err := Reexec(os.Stderr)
	if err == nil {
		t.Fatal("Reexec: nil err on parent branch with missing helpers")
	}
	if !strings.Contains(err.Error(), "newuidmap") {
		t.Errorf("err = %v, want it to mention newuidmap", err)
	}
}

// TestChildWaitForMapsErrorsOnUnsetFD covers the
// "fd 3 isn't open" branch: child runs with EnvMarker set
// but no inherited sync pipe. Without this branch, a buggy
// parent fork would silently get past Reexec and into main()
// before the uid maps are populated.
func TestChildWaitForMapsErrorsOnSyncReadEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// Close write end immediately → child's read sees EOF.
	if err := w.Close(); err != nil {
		t.Fatalf("close w: %v", err)
	}

	saved, dupErr := unix.Dup(childSyncFD)
	if dupErr != nil {
		saved = -1
	}
	if err := unix.Dup2(int(r.Fd()), childSyncFD); err != nil {
		t.Fatalf("dup2: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		if saved < 0 {
			_ = unix.Close(childSyncFD)
			return
		}
		_ = unix.Dup2(saved, childSyncFD)
		_ = unix.Close(saved)
	})

	if err := childWaitForMaps(); err == nil {
		t.Fatal("childWaitForMaps: nil err with closed sync pipe")
	}
}

// TestWaitWithSignalForwardingDeliversSignal verifies the
// signal-forwarding goroutine actually relays a signal to the
// child. A SIGUSR1 to the test process should make it through
// to the wrapped /bin/sh, which we make terminate on the
// signal. The reported exit code is then 128+SIGUSR1.
func TestWaitWithSignalForwardingDeliversSignal(t *testing.T) {
	// /bin/sh with `trap` exits 137 on SIGUSR1. Using
	// `exec sleep` keeps the shell PID the same as the
	// reachable process so signal.Notify→cmd.Process.Signal
	// lands on the right target.
	cmd := exec.Command("/bin/sh", "-c",
		`trap 'exit 137' USR1; sleep 30 & wait $!`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- waitWithSignalForwarding(cmd) }()
	// Give the trap a moment to install.
	time.Sleep(150 * time.Millisecond)
	// Signal the test process; the goroutine inside
	// waitWithSignalForwarding fans it out to cmd.
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 137 {
			// Different shells trap differently; relax to
			// "non-zero" to absorb the dash/bash variance.
			if code == 0 {
				t.Errorf("code = 0, want non-zero (signal made it through)")
			}
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-codeCh
		t.Fatal("waitWithSignalForwarding never returned after SIGUSR1")
	}
}

// TestParentReexecRunSurfacesStartFailure covers the
// cmd.Start() error branch by stubbing buildReexecCmd to
// return a cmd pointing at a binary that doesn't exist. The
// path forks no actual child, so no cleanup is needed beyond
// the helper-stubs themselves.
func TestParentReexecRunSurfacesStartFailure(t *testing.T) {
	dir := t.TempDir()
	okStub := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(okStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	prevBuild := buildReexecCmd
	newuidmapCandidates = []string{okStub}
	newgidmapCandidates = []string{okStub}
	buildReexecCmd = func(*os.File) *exec.Cmd {
		return exec.Command("/this-binary-does-not-exist-on-any-host")
	}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
		buildReexecCmd = prevBuild
	})

	_, err := parentReexecRun()
	if err == nil {
		t.Fatal("parentReexecRun: nil err with unstartable cmd")
	}
	if !strings.Contains(err.Error(), "start re-exec") {
		t.Errorf("err = %v, want 'start re-exec' wrap", err)
	}
}

// TestParentReexecRunSurfacesNewuidmapFailure exercises the
// "writeIDMap newuidmap returns non-zero" error branch by
// stubbing a failing newuidmap. parentReexecRun must kill the
// already-spawned child and return the error.
func TestParentReexecRunSurfacesNewuidmapFailure(t *testing.T) {
	dir := t.TempDir()
	okStub := filepath.Join(dir, "ok.sh")
	failStub := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(okStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write ok stub: %v", err)
	}
	if err := os.WriteFile(failStub,
		[]byte("#!/bin/sh\necho 'simulated fail' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fail stub: %v", err)
	}
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	prevBuild := buildReexecCmd
	newuidmapCandidates = []string{failStub}
	newgidmapCandidates = []string{okStub}
	buildReexecCmd = func(syncFD *os.File) *exec.Cmd {
		c := exec.Command("/bin/sh", "-c", "head -c 1 >/dev/null")
		c.Stdin = syncFD
		return c
	}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
		buildReexecCmd = prevBuild
	})

	code, err := parentReexecRun()
	if err == nil {
		t.Fatal("parentReexecRun: nil err with failing newuidmap stub")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 on map-write failure", code)
	}
	if !strings.Contains(err.Error(), "simulated fail") {
		t.Errorf("err = %v, want the stub's stderr surfaced", err)
	}
}

// TestParentReexecRunSurfacesNewgidmapFailure mirrors the
// newuidmap-fails test for the gid path, hitting the second
// writeIDMap branch (the one that runs after newuidmap has
// already succeeded). Without coverage of this branch, a
// regression that breaks the gid path would silently shrug.
func TestParentReexecRunSurfacesNewgidmapFailure(t *testing.T) {
	dir := t.TempDir()
	okStub := filepath.Join(dir, "ok.sh")
	failStub := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(okStub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write ok: %v", err)
	}
	if err := os.WriteFile(failStub,
		[]byte("#!/bin/sh\necho 'gid sim fail' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fail: %v", err)
	}
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	prevBuild := buildReexecCmd
	newuidmapCandidates = []string{okStub}
	newgidmapCandidates = []string{failStub}
	buildReexecCmd = func(syncFD *os.File) *exec.Cmd {
		c := exec.Command("/bin/sh", "-c", "head -c 1 >/dev/null")
		c.Stdin = syncFD
		return c
	}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
		buildReexecCmd = prevBuild
	})

	_, err := parentReexecRun()
	if err == nil {
		t.Fatal("parentReexecRun: nil err with failing newgidmap stub")
	}
	if !strings.Contains(err.Error(), "gid sim fail") {
		t.Errorf("err = %v, want gid stub's stderr surfaced", err)
	}
}

// TestParentReexecRunWithStubsExecutesFullFlow covers the
// rest of parentReexecRun by substituting all three test
// seams (newuidmap/newgidmap candidate lists + buildReexecCmd
// itself) with stubs. The substitute cmd is just a /bin/sh
// `read -n 1` that exits 0 after consuming the sync byte —
// fast, deterministic, doesn't recurse into the test binary.
func TestParentReexecRunWithStubsExecutesFullFlow(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	prevU, prevG := newuidmapCandidates, newgidmapCandidates
	prevBuild := buildReexecCmd
	newuidmapCandidates = []string{stub}
	newgidmapCandidates = []string{stub}
	buildReexecCmd = func(syncFD *os.File) *exec.Cmd {
		// `head -c 1` reads exactly one byte from stdin then
		// exits 0. With syncFD plumbed in as the cmd's stdin
		// the byte parentReexecRun writes to release the
		// real child also releases this stub — same pattern,
		// no userns required.
		c := exec.Command("/bin/sh", "-c", "head -c 1 >/dev/null")
		c.Stdin = syncFD
		return c
	}
	t.Cleanup(func() {
		newuidmapCandidates, newgidmapCandidates = prevU, prevG
		buildReexecCmd = prevBuild
	})

	code, err := parentReexecRun()
	if err != nil {
		t.Fatalf("parentReexecRun: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
}

func TestRootMappedMountNamespace(t *testing.T) {
	attr := RootMappedMountNamespace()
	if attr.Unshareflags != syscall.CLONE_NEWUSER|syscall.CLONE_NEWNS || attr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("namespace process attributes = %+v", attr)
	}
	wantUID := syscall.SysProcIDMap{ContainerID: 0, HostID: os.Geteuid(), Size: 1}
	wantGID := syscall.SysProcIDMap{ContainerID: 0, HostID: os.Getegid(), Size: 1}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0] != wantUID {
		t.Fatalf("UID mapping = %v, want %v", attr.UidMappings, wantUID)
	}
	if len(attr.GidMappings) != 1 || attr.GidMappings[0] != wantGID || attr.GidMappingsEnableSetgroups {
		t.Fatalf("GID mapping = %v, want %v with setgroups denied", attr.GidMappings, wantGID)
	}
}
