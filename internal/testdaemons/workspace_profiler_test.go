//go:build linux && integration

package testdaemons_test

import (
	"path/filepath"
	"testing"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
)

// workspaceProfilerScript samples the guest's process table from
// inside the workspace container every 15s, forever, until the
// exec is killed at test cleanup. It exists because diagnosing a
// stuck in-VM workload from the outside is guesswork: a readiness
// wait that times out can't tell you whether the workload is
// CPU-bound, blocked on IO, or already dead. This dumps the
// decisive signals — loadavg, every process in R (running) or D
// (uninterruptible sleep) with its comm and kernel wchan, and the
// /nix/store path count — so the answer is right there in the
// failing test's log.
//
// It earned its place: it's what showed `nix develop` wasn't
// hanging on virtiofs but had crashed on a missing /dev/ptmx
// (the process was simply gone, loadavg 0, store frozen). POSIX
// sh only (grep/cut/cat/ls/wc/date) — no awk/sed — so it runs
// under whatever minimal userland the image ships.
const workspaceProfilerScript = `while true; do
  echo "=== profiler $(date +%H:%M:%S) loadavg=$(cat /proc/loadavg 2>/dev/null) ==="
  for s in /proc/[0-9]*; do
    [ -r "$s/status" ] || continue
    st=$(grep '^State:' "$s/status" 2>/dev/null | cut -f2 | cut -d' ' -f1)
    case "$st" in
      R|D)
        comm=$(cat "$s/comm" 2>/dev/null)
        wch=$(cat "$s/wchan" 2>/dev/null)
        rss=$(grep '^VmRSS:' "$s/status" 2>/dev/null | tr -s ' ')
        echo "  pid=${s#/proc/} st=$st comm=$comm wchan=$wch $rss"
        ;;
    esac
  done
  echo "  store paths: $(ls /nix/store 2>/dev/null | wc -l)"
  sleep 15
done
`

// attachWorkspaceProfiler launches workspaceProfilerScript as a
// background exec into wsName. Output streams into the test log
// via startDaftSubprocess's tee, so the samples are invisible on
// a passing run (t.Log is only shown with -v or on failure) and
// right there when something stalls. The exec is killed by
// startDaftSubprocess's t.Cleanup, so callers attach and forget.
//
// Call it after the workspace is launched and the workload exec
// has started, so the first sample already captures the workload.
func attachWorkspaceProfiler(t *testing.T, c *testdaemons.Cluster, wsName string) {
	t.Helper()
	profileLog := filepath.Join(t.TempDir(), "profiler-"+wsName+".log")
	startDaftSubprocess(
		t, profileLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-lc", workspaceProfilerScript,
	)
}
