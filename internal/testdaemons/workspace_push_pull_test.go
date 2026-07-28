//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// Sentinels carried through the workspace so each assertion matches an
// unambiguous substring. The pushed marker proves S3 (the bytes a user
// pushed materialise into the VM at launch); the mutated marker proves
// S4 (a write the VM made surfaces back through `daft workspace pull`).
const (
	s4PushedMarker  = "S4-PUSHED-MARKER"
	s4MutatedMarker = "S4-MUTATED-IN-VM"
	s4XFSSentinel   = "S4-XFS-REFLINK-OK"
)

// TestClusterWorkspacePushMutatePull is the full-stack proof of the
// dir-local push/pull workflow (PLAN.md S-series), driven through the
// real `daft` CLI against a real booted VM exactly as a user would:
//
//	cd <workspace dir>          # holds in.txt the user wants in the VM
//	daft workspace create
//	daft workspace image <ref>
//	daft workspace mount . /workspace   # dir-local rw mapping
//	daft workspace target <node>
//	daft workspace push          # in.txt -> node CAS (S2c)
//	daft workspace launch        # CAS -> retained XFS disk (S3)
//	# inside the VM: read in.txt, then write out.txt
//	daft workspace stop          # snapshot guest XFS into protected CAS
//	daft workspace pull          # materialise the durable snapshot (S4)
//
// It then asserts the VM-written out.txt landed back in the local
// workspace dir. Unlike the other testdaemons workspace tests (which
// use `mount add --source /abs`, a direct host bind that never touches
// the CAS), this exercises push → XFS materialisation → mutate → stop-time
// protected snapshot → pull. It also pins XFS and reflink semantics.
func TestClusterWorkspacePushMutatePull(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/s4-roundtrip:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	// The "user's" local workspace directory, seeded with a file they
	// want available inside the VM. push reads from here; pull writes
	// back here. It lives on the same host the pane runs on, so the
	// test can read the pulled file directly afterwards.
	wsDir := t.TempDir()
	mustWriteFile(t, filepath.Join(wsDir, "in.txt"), s4PushedMarker+"\n")

	wsName := "s4-roundtrip"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	// cd into the workspace dir first: the dir-local `mount`, `push`,
	// and `pull` all read/write ./.daft/config.json and resolve the
	// local mount path relative to the cwd. The remaining verbs address
	// the workspace by --name, so they're cwd-independent but run here
	// too. push precedes launch so the bytes are in the node CAS before
	// the executor materialises them.
	testdaemons.RunWorkspaceSteps(t, s, "S4RT", []testdaemons.WorkspaceStep{
		{Label: "cd", Cmd: fmt.Sprintf("cd '%s'", wsDir)},
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount . /workspace --name %s %s", prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --disk 512M --name %s %s", prefix, wsName, orch,
		)},
		{Label: "push", Cmd: fmt.Sprintf(
			"%s workspace push --name %s %s", prefix, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 60*time.Second)

	// The nix-shell-image entrypoint prints this once crun has the
	// container up — the gate before exec can dial in.
	readyOutput := c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 90*time.Second)
	readyCount := strings.Count(readyOutput, "daft-nix-shell ready")

	// Read the pushed file and mutate the mount inside the VM, in one
	// `daft workspace exec` run *off the pane*: exec's stdin pump only
	// unblocks on EOF, and the tmux PTY never EOFs, so running it in the
	// pane would wedge the shell before the pull step. startDaftSubprocess
	// gives it a /dev/null stdin instead. The single command:
	//   - cat /workspace/in.txt  -> S3 proof (the pushed bytes are in the VM),
	//   - proves the mount is XFS and supports an explicit reflink clone,
	//   - writes out.txt          -> the mutation S4 must surface,
	//   - sync                    -> flush the guest write before stop snapshots,
	//   - echoes the write's exit -> S4-WROTE-0 success sentinel.
	execLog := filepath.Join(t.TempDir(), "exec.log")
	paneDump := func() string { return s.Capture() }
	execProc := startDaftSubprocess(t, execLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/dash", "-c",
		"cat /workspace/in.txt; test \"$(stat -f -c %T /workspace)\" = xfs && "+
			"cp --reflink=always /workspace/in.txt /workspace/reflink.txt && "+
			"echo "+s4XFSSentinel+" && echo "+s4MutatedMarker+
			" > /workspace/out.txt; rc=$?; sync; echo S4-WROTE-$rc")
	waitForFileContains(t, execProc, execLog, s4PushedMarker, 60*time.Second, paneDump)
	waitForFileContains(t, execProc, execLog, s4XFSSentinel, 60*time.Second, paneDump)
	waitForFileContains(t, execProc, execLog, "S4-WROTE-0", 60*time.Second, paneDump)

	// Grow through the public resource command while the VM is running. The
	// command returns only after qcow2, virtio-blk, and guest XFS growth all
	// succeed. Verify the mounted filesystem gained substantial capacity.
	testdaemons.RunWorkspaceSteps(t, s, "S4GROW", []testdaemons.WorkspaceStep{{
		Label: "grow", Cmd: fmt.Sprintf(
			"%s workspace resources --disk 1G --name %s %s", prefix, wsName, orch,
		),
	}}, 60*time.Second)
	growLog := filepath.Join(t.TempDir(), "grow.log")
	growProc := startDaftSubprocess(t, growLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false", "--orchestrator", c.OrchestratorURL,
		"--", "/bin/dash", "-c",
		"test $(df -B1 --output=size /workspace | tail -1) -gt 800000000 && echo S4-GROWN")
	waitForFileContains(t, growProc, growLog, "S4-GROWN", 60*time.Second, paneDump)

	// Stop and relaunch before pulling. The second boot must reopen the retained
	// disk without reseeding it, preserving the guest mutation exactly.
	testdaemons.RunWorkspaceSteps(t, s, "S4REUSE", []testdaemons.WorkspaceStep{
		{Label: "stop", Cmd: fmt.Sprintf("%s workspace stop --name %s %s", prefix, wsName, orch)},
		{Label: "launch", Cmd: fmt.Sprintf("%s workspace launch --name %s %s", prefix, wsName, orch)},
	}, 60*time.Second)
	deadline := time.Now().Add(90 * time.Second)
	for {
		output := c.WaitForSandboxdOutput(t, "daft-nix-shell ready", time.Second)
		if strings.Count(output, "daft-nix-shell ready") > readyCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second sandbox did not become ready\n%s", output)
		}
		time.Sleep(100 * time.Millisecond)
	}
	reuseLog := filepath.Join(t.TempDir(), "reuse.log")
	reuseProc := startDaftSubprocess(t, reuseLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false", "--orchestrator", c.OrchestratorURL,
		"--", "/bin/dash", "-c", "cat /workspace/out.txt")
	waitForFileContains(t, reuseProc, reuseLog, s4MutatedMarker, 60*time.Second, paneDump)

	// Cross 90% of the configured allowance and let the Node's periodic usage
	// monitor pause the VM. Storage inspection remains available while paused.
	fillLog := filepath.Join(t.TempDir(), "fill.log")
	fillProc := startDaftSubprocess(t, fillLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false", "--orchestrator", c.OrchestratorURL,
		"--", "/bin/dash", "-c",
		"dd if=/dev/zero of=/workspace/fill.bin bs=1M count=940; rc=$?; sync; "+
			"test $(stat -c %s /workspace/fill.bin) -gt 900000000; size_rc=$?; "+
			"rm /workspace/fill.bin; sync; dd if=/dev/zero of=/workspace/fill.bin bs=1M count=875; "+
			"refill_rc=$?; sync; echo S4-FILL-$rc-$size_rc-$refill_rc")
	// The qcow2/XFS allowance is the final boundary: the oversized write must
	// hit ENOSPC. Recreating a clean 875 MiB filler leaves deterministic XFS
	// metadata headroom while keeping measured usage above the 90% threshold.
	waitForFileContains(t, fillProc, fillLog, "S4-FILL-1-0-0", 60*time.Second, paneDump)
	testdaemons.RunWorkspaceSteps(t, s, "S4FREEZE", []testdaemons.WorkspaceStep{{
		Label: "wait-frozen", Cmd: fmt.Sprintf(
			"frozen=; for i in $(seq 1 30); do out=$(%s workspace storage status --name %s %s); echo \"$out\"; echo \"$out\" | grep -q 'storage_frozen=true' && frozen=yes && break; sleep 1; done; test \"$frozen\" = yes",
			prefix, wsName, orch,
		),
	}}, 45*time.Second)

	// A frozen resize must grow qcow2, the VMM block device, and guest XFS,
	// then re-pause. Only the explicit public unfreeze operation resumes work.
	testdaemons.RunWorkspaceSteps(t, s, "S4UNFREEZE", []testdaemons.WorkspaceStep{
		{Label: "grow-frozen", Cmd: fmt.Sprintf(
			"%s workspace resources --disk 2G --name %s %s", prefix, wsName, orch,
		)},
		{Label: "still-frozen", Cmd: fmt.Sprintf(
			"%s workspace storage status --name %s %s | grep -q 'storage_frozen=true'", prefix, wsName, orch,
		)},
		{Label: "unfreeze", Cmd: fmt.Sprintf(
			"%s workspace unfreeze --name %s %s", prefix, wsName, orch,
		)},
	}, 60*time.Second)
	testdaemons.RunWorkspaceSteps(t, s, "S4RESUMED", []testdaemons.WorkspaceStep{{
		Label: "status-after-unfreeze", Cmd: fmt.Sprintf(
			"%s workspace storage status --name %s %s | grep -q 'storage_frozen=false'",
			prefix, wsName, orch,
		),
	}}, 30*time.Second)

	// Stop first so pull cannot use the live guest refresher. A successful pull
	// therefore proves explicit unfreeze resumed guest services and the stop
	// path committed final XFS contents into the durable protected manifest.
	testdaemons.RunWorkspaceSteps(t, s, "S4PULL", []testdaemons.WorkspaceStep{
		{Label: "stop", Cmd: fmt.Sprintf(
			"%s workspace stop --name %s %s", prefix, wsName, orch,
		)},
		{Label: "pull", Cmd: fmt.Sprintf(
			"%s workspace pull --name %s %s", prefix, wsName, orch,
		)},
	}, 60*time.Second)

	got, err := os.ReadFile(filepath.Join(wsDir, "out.txt")) //nolint:gosec // G304: test-controlled path under t.TempDir
	if err != nil {
		t.Fatalf("pulled workspace missing out.txt: %v\n--- pane ---\n%s", err, s.Capture())
	}
	if !strings.Contains(string(got), s4MutatedMarker) {
		t.Errorf("out.txt = %q, want it to contain %q", got, s4MutatedMarker)
	}
}

// mustWriteFile writes data to path, creating parent dirs, fataling on
// error. A local helper so the test reads top-to-bottom without pulling
// in a shared fixtures file.
func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil { //nolint:gosec // G306: test fixture file
		t.Fatalf("write %s: %v", path, err)
	}
}
