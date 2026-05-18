//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterDaftNodeListShowsSandboxd is the D6b.1 smoke. It
// proves the full subprocess control plane end to end:
//
//   - orchestrator + sandboxd are spawned as real subprocesses
//     with Setpgid + Pdeathsig.
//   - Mutual mTLS approval (orch ↔ sbx, orch ↔ client) is
//     bootstrapped via internal/identity APIs before any daemon
//     starts.
//   - The orchestrator's listener becomes reachable.
//   - Sandboxd dials in, mTLS-authenticates, and registers.
//   - `daft node list`, run inside a tmux pane against the
//     orchestrator URL, prints the sandboxd's fingerprint.
//   - Cleanup tears down both daemons via PG-kill; no procs
//     escape.
//
// No VM is launched in this slice. D6b.2 adds the workspace +
// real-VM layer on top of this rig.
func TestClusterDaftNodeListShowsSandboxd(t *testing.T) {
	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	cmd := fmt.Sprintf("%s --config-dir %s node list --orchestrator %s",
		c.DaftBin, c.ClientConfigDir, c.OrchestratorURL)
	s.Enter(cmd)
	// Registration is asynchronous on the sandboxd side; give it
	// a generous window. The Capture-poll loop in WaitFor reruns
	// every 100ms so the typical case completes in ~1s.
	s.WaitFor(c.SandboxdFP.String(), 15*time.Second)
}
