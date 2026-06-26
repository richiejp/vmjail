//go:build linux && integration

package testdaemons_test

import (
	"path/filepath"
	"testing"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
)

// workspaceProfilerScript samples the guest's resource state from
// inside the workspace container every 5s, forever, until the
// exec is killed at test cleanup. It exists because diagnosing a
// stuck in-VM workload from the outside is guesswork: a readiness
// wait that times out can't tell you whether the workload is
// CPU-bound, blocked on IO, swapping, throttled, or already dead.
// This dumps every signal that distinguishes those modes so the
// answer is right there in the failing test's log.
//
// Per sample it prints (one block per line, key-prefixed for grep):
//   - loadavg + cpu: raw /proc/loadavg and /proc/stat cpu line
//     (cumulative jiffies — deltas reconstructed in post)
//   - mem: MemTotal / MemAvailable / Dirty / Writeback /
//     SwapFree from /proc/meminfo
//   - psi-cpu, psi-mem, psi-io: PSI averages from
//     /proc/pressure/{cpu,memory,io} if the kernel exposes them
//   - procstate: a histogram of R / D / S / Z / T / I — total tasks
//     by state, so "system is idle" is visible as one number
//   - cgroup: cpu.stat / memory.current / memory.peak / memory.max
//     / io.stat / pids.current for the workload's own cgroup v2
//     dir (looked up via /proc/self/cgroup). These are the
//     accounting counters the kernel keeps for THIS test's VM,
//     uncontested by any other tenant — most direct measure of
//     "what is the workload itself consuming"
//   - diskstats + netdev: raw counters per device / interface;
//     deltas reconstructed in post
//   - active-procs: every R/D process with comm + wchan + RSS,
//     same as the prior version — kept because seeing the wchan
//     of a stuck process is what tells you which kernel path is
//     blocking
//   - storepaths: /nix/store entry count, tracks substitution
//     progress
//
// It earned its place: the prior version (loadavg + R/D procs +
// store-path count) is what showed `nix develop` had crashed on a
// missing /dev/ptmx; the enriched version was needed when a
// GOCACHE-cold typecheck race inside `make lint` produced "system
// is barely doing anything" with no other diagnostic. POSIX sh
// only (grep/cut/cat/head/tail/ls/wc/date/tr) — no awk/sed — so
// it runs under whatever minimal userland the image ships.
const workspaceProfilerScript = `INTERVAL=5
while true; do
  echo "=== profiler $(date +%H:%M:%S) ==="
  echo "  loadavg: $(cat /proc/loadavg 2>/dev/null)"
  echo "  cpu: $(head -n 1 /proc/stat 2>/dev/null)"
  if [ -r /proc/meminfo ]; then
    echo "  mem: $(grep -E '^(MemTotal|MemAvailable|MemFree|Dirty|Writeback|SReclaimable|SwapFree|SwapCached|Cached|Buffers):' /proc/meminfo | tr '\n' ' ')"
  fi
  for r in cpu memory io; do
    if [ -r /proc/pressure/$r ]; then
      echo "  psi-$r: $(head -n 1 /proc/pressure/$r)"
    fi
  done
  n_r=0; n_d=0; n_s=0; n_z=0; n_t=0; n_i=0
  for s in /proc/[0-9]*/status; do
    [ -r "$s" ] || continue
    st=$(grep '^State:' "$s" 2>/dev/null | cut -f2 | cut -d' ' -f1)
    case "$st" in
      R) n_r=$((n_r+1)) ;;
      D) n_d=$((n_d+1)) ;;
      S) n_s=$((n_s+1)) ;;
      Z) n_z=$((n_z+1)) ;;
      T) n_t=$((n_t+1)) ;;
      I) n_i=$((n_i+1)) ;;
    esac
  done
  echo "  procstate: R=$n_r D=$n_d S=$n_s Z=$n_z T=$n_t I=$n_i"
  cg=$(head -n 1 /proc/self/cgroup 2>/dev/null | cut -d: -f3)
  if [ -n "$cg" ] && [ -d /sys/fs/cgroup"$cg" ]; then
    cgdir=/sys/fs/cgroup"$cg"
    echo "  cgroup: $cg"
    for f in cpu.stat memory.current memory.peak memory.max memory.swap.current io.stat pids.current pids.max; do
      if [ -r "$cgdir/$f" ]; then
        # io.stat / cpu.stat are multi-line; collapse to one
        echo "    $f: $(tr '\n' ' ' < "$cgdir/$f")"
      fi
    done
  fi
  if [ -r /proc/diskstats ]; then
    cat /proc/diskstats | while read line; do echo "    diskstat: $line"; done
  fi
  if [ -r /proc/net/dev ]; then
    tail -n +3 /proc/net/dev | while read line; do echo "    netdev: $line"; done
  fi
  for s in /proc/[0-9]*; do
    [ -r "$s/status" ] || continue
    st=$(grep '^State:' "$s/status" 2>/dev/null | cut -f2 | cut -d' ' -f1)
    case "$st" in
      R|D)
        comm=$(cat "$s/comm" 2>/dev/null)
        wch=$(cat "$s/wchan" 2>/dev/null)
        rss=$(grep '^VmRSS:' "$s/status" 2>/dev/null | tr -s ' ')
        echo "    active-proc: pid=${s#/proc/} st=$st comm=$comm wchan=$wch $rss"
        ;;
    esac
  done
  echo "  storepaths: $(ls /nix/store 2>/dev/null | wc -l)"
  sleep $INTERVAL
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

// attachWorkspaceObserve streams the Tier-2 eBPF gadget summary
// (`daft sandbox observe <ws> --follow`) into the test log as a
// background subprocess, killed at cleanup. This is the gadget-based
// counterpart to attachWorkspaceProfiler: where the in-container
// /proc sampler answers "is the VM OOM/CPU-bound/blocked", the
// gadgets answer "what is the workload *doing*" — exec-snoop shows
// the build's process tree + failed execs, io-snoop shows whether
// it's actually fetching bytes or wedged on a socket, sleep-snoop
// shows poll/retry loops, and open-snoop's ENOENT tallies are
// exactly how the /dev/ptmx bug first surfaced. The two are
// complementary and both invisible on a passing run; per
// [[feedback-observability]] the host-side gadget view is the
// preferred surface and the in-guest sampler is transitional.
//
// Unlike the profiler this runs entirely host-side over the
// orchestrator's ObserveSummary RPC — it does not exec into the
// container, so it adds no in-VM load to whatever it's measuring.
// Call it after launch so the first poll already covers the workload.
func attachWorkspaceObserve(t *testing.T, c *testdaemons.Cluster, wsName string) {
	t.Helper()
	observeLog := filepath.Join(t.TempDir(), "observe-"+wsName+".log")
	startDaftSubprocess(
		t, observeLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"sandbox", "observe", wsName,
		"--follow", "--interval", "5s",
		"--orchestrator", c.OrchestratorURL,
	)
}
