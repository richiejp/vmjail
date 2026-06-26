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
//	daft workspace launch        # CAS -> backing dir -> virtiofs (S3)
//	# inside the VM: read in.txt, then write out.txt
//	daft workspace pull          # re-manifest live backing dir (S4)
//
// It then asserts the VM-written out.txt landed back in the local
// workspace dir. Unlike the other testdaemons workspace tests (which
// use `mount add --source /abs`, a direct host bind that never touches
// the CAS), this is the only end-to-end exercise of the
// push → materialise → mutate-in-VM → pull cycle that S3/S4 implement.
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
		{Label: "push", Cmd: fmt.Sprintf(
			"%s workspace push --name %s %s", prefix, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 60*time.Second)

	// The nix-shell-image entrypoint prints this once crun has the
	// container up — the gate before exec can dial in.
	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 90*time.Second)

	// Read the pushed file and mutate the mount inside the VM, in one
	// `daft workspace exec` run *off the pane*: exec's stdin pump only
	// unblocks on EOF, and the tmux PTY never EOFs, so running it in the
	// pane would wedge the shell before the pull step. startDaftSubprocess
	// gives it a /dev/null stdin instead. The single command:
	//   - cat /workspace/in.txt  -> S3 proof (the pushed bytes are in the VM),
	//   - writes out.txt          -> the mutation S4 must surface,
	//   - sync                    -> flush the guest write through virtiofs to
	//                                the host backing dir before pull reads it,
	//   - echoes the write's exit -> S4-WROTE-0 success sentinel.
	execLog := filepath.Join(t.TempDir(), "exec.log")
	paneDump := func() string { return s.Capture() }
	execProc := startDaftSubprocess(t, execLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/dash", "-c",
		"cat /workspace/in.txt; echo "+s4MutatedMarker+
			" > /workspace/out.txt; rc=$?; sync; echo S4-WROTE-$rc")
	waitForFileContains(t, execProc, execLog, s4PushedMarker, 60*time.Second, paneDump)
	waitForFileContains(t, execProc, execLog, "S4-WROTE-0", 60*time.Second, paneDump)

	// S4 proof: pull re-manifests the *live* backing dir (reflecting the
	// VM's write) rather than the last-pushed snapshot, so out.txt comes
	// back to the local workspace dir. Runs in the pane (still at the
	// workspace dir from the cd above); the exec subprocess never touched
	// it, so the shell is at a clean prompt.
	testdaemons.RunWorkspaceSteps(t, s, "S4PULL", []testdaemons.WorkspaceStep{
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
