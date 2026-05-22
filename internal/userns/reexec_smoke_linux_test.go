//go:build linux

package userns_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/userns"
)

// helperEnvVar, when set, makes TestMain invoke
// userns.Reexec(os.Stderr) directly and then block on stdin
// (instead of running the test suite). The smoke test spawns
// itself with this var set; the spawned copy plays the role of
// a "real" sandboxd that the smoke test inspects.
const helperEnvVar = "DAFT_USERNS_REEXEC_HELPER"

// TestMain is the re-exec trampoline: the test binary plays
// two roles. With helperEnvVar set, it just runs Reexec and
// blocks — that's the "helper" the smoke test spawns. Without,
// it falls through to the normal test runner.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == "1" {
		if err := userns.Reexec(os.Stderr); err != nil {
			_, _ = os.Stderr.WriteString("helper reexec: " + err.Error() + "\n")
			os.Exit(2)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestReexecRealLandsInUserNSMappedFromSubID is the
// in-process smoke test for the parent-reexec path: it builds
// a tiny helper binary that invokes userns.Reexec(os.Stderr)
// and then sleeps, runs it, and reads /proc/<child>/uid_map
// to confirm the helper actually wound up inside a
// multi-range user namespace populated from the calling
// user's /etc/subuid.
//
// Skips quietly when /run/wrappers/bin/newuidmap (NixOS) or
// /usr/bin/newuidmap (most other distros) isn't present, since
// the parent path can't write a multi-range map without it.
func TestReexecRealLandsInUserNSMappedFromSubID(t *testing.T) {
	if os.Getenv(userns.EnvMarker) == "1" {
		t.Skipf("nested run: %s already set", userns.EnvMarker)
	}
	if !haveNewIDMap() {
		t.Skip("newuidmap/newgidmap not on host (neither /run/wrappers nor /usr/bin)")
	}

	// Spawn ourselves; TestMain detects helperEnvVar and runs
	// userns.Reexec instead of the test suite.
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	//nolint:gosec // G204: binary is os.Executable() — i.e. ourselves.
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(), helperEnvVar+"=1")
	// Pipe stdin: closing it lets the helper exit. We keep it
	// open until we've read uid_map so the helper stays alive.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if startErr := cmd.Start(); startErr != nil {
		t.Fatalf("start helper: %v", startErr)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	// The PID we have is the parent re-exec'er. Walk one
	// generation deeper to find the actual userns inhabitant.
	childPID := waitForChild(t, cmd.Process.Pid)

	uidMap := readMapFile(t, fmt.Sprintf("/proc/%d/uid_map", childPID))
	gidMap := readMapFile(t, fmt.Sprintf("/proc/%d/gid_map", childPID))
	if len(uidMap) < 2 {
		t.Errorf("uid_map has %d entries, want ≥2:\n%v\nstderr=%q",
			len(uidMap), uidMap, stderr.String())
	}
	if len(gidMap) < 2 {
		t.Errorf("gid_map has %d entries, want ≥2:\n%v\nstderr=%q",
			len(gidMap), gidMap, stderr.String())
	}
	// First entry should map the calling uid to ns 0.
	if len(uidMap) > 0 {
		got := uidMap[0]
		if got.inside != 0 || got.outside != os.Getuid() {
			t.Errorf("uid_map[0] = {inside=%d outside=%d}, want {0, %d}",
				got.inside, got.outside, os.Getuid())
		}
	}
}

// mapEntry is a single parsed line of /proc/<pid>/uid_map or
// /proc/<pid>/gid_map: `<inside> <outside> <count>`.
type mapEntry struct{ inside, outside, count int }

// readMapFile parses every entry of a uid_map/gid_map file.
// A real (non-disabled) re-exec populates at least two: one
// for the calling uid → ns 0, and one for the subuid range.
func readMapFile(t *testing.T, path string) []mapEntry {
	t.Helper()
	//nolint:gosec // path is constructed from a child pid we just spawned.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []mapEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		inside, _ := strconv.Atoi(fields[0])
		outside, _ := strconv.Atoi(fields[1])
		count, _ := strconv.Atoi(fields[2])
		out = append(out, mapEntry{inside, outside, count})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// haveNewIDMap returns true if at least one of the candidate
// paths for newuidmap exists. Mirrors firstExisting's logic
// for the test's skip decision.
func haveNewIDMap() bool {
	for _, p := range []string{"/run/wrappers/bin/newuidmap", "/usr/bin/newuidmap"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// waitForChild polls /proc/<parentPID>/task/<parentPID>/children
// until a single direct child shows up, then returns its PID.
// The userns re-exec parent has exactly one direct child (the
// re-exec'd binary); the test races against the spawn so we
// give it up to 2s before failing.
func waitForChild(t *testing.T, parentPID int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by deadline + t.Fatalf below"
	for {
		path := fmt.Sprintf("/proc/%d/task/%d/children", parentPID, parentPID)
		//nolint:gosec // path is constructed from a child pid we just spawned.
		raw, err := os.ReadFile(path)
		if err == nil {
			pids := strings.Fields(strings.TrimSpace(string(raw)))
			if len(pids) > 0 {
				pid, convErr := strconv.Atoi(pids[0])
				if convErr == nil {
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no child of parent %d within 2s (read err: %v)", parentPID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
