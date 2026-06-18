package localcluster

import (
	"os"
	"path/filepath"
	"testing"
)

// testClusterURL is the loopback orchestrator URL reused across these
// tests (hoisted to satisfy goconst).
const testClusterURL = "https://127.0.0.1:8443"

func TestDefaultStateDirXDG(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	got, err := DefaultStateDir()
	if err != nil {
		t.Fatalf("DefaultStateDir: %v", err)
	}
	if want := filepath.Join(base, "daft", "cluster"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDefaultStateDirHomeFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	got, err := DefaultStateDir()
	if err != nil {
		t.Fatalf("DefaultStateDir: %v", err)
	}
	if want := filepath.Join(home, ".local", "state", "daft", "cluster"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLoadAbsentIsNilNoError(t *testing.T) {
	c, err := Load(t.TempDir()) // no cluster.json under a fresh dir
	if err != nil {
		t.Fatalf("Load absent: unexpected error %v", err)
	}
	if c != nil {
		t.Fatalf("Load absent: want nil cluster, got %+v", c)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Cluster{
		URL: testClusterURL, ClientConfigDir: filepath.Join(dir, "client"),
		OrchestratorFP: "sha256:orch", SandboxFP: "sha256:sbx",
		Addr: "127.0.0.1:8443", StateDir: dir, PIDs: []int{os.Getpid()},
	}
	if err := writeInfo(dir, want); err != nil {
		t.Fatalf("writeInfo: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.URL != want.URL || got.ClientConfigDir != want.ClientConfigDir ||
		got.SandboxFP != want.SandboxFP || got.Addr != want.Addr || len(got.PIDs) != 1 {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

func TestLoadMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want parse error on malformed cluster.json, got nil")
	}
}

func TestLoadReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory at the cluster.json path makes ReadFile fail with a
	// non-NotExist error (EISDIR), exercising Load's read-error branch.
	if err := os.Mkdir(filepath.Join(dir, "cluster.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load: want read error when cluster.json is a directory")
	}
}

func TestAlive(t *testing.T) {
	// Our own pid is always alive; an unused-high pid is (almost certainly) not.
	if !(&Cluster{PIDs: []int{os.Getpid()}}).Alive() {
		t.Errorf("Alive() should report the current process as alive")
	}
	if (&Cluster{PIDs: []int{0}}).Alive() {
		t.Errorf("Alive() should ignore pid 0")
	}
	// A reaped/never-existing pid in the far range: Kill(pid,0) → ESRCH.
	if (&Cluster{PIDs: []int{1 << 30}}).Alive() {
		t.Errorf("Alive() should report a dead pid as not alive")
	}
}
