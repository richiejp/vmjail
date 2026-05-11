package sandbox

import (
	"slices"
	"strings"
	"testing"
)

// testTagWorkspace is the virtiofs tag the args-builder tests use
// for the workspace share. Hoisted to a const because the string
// appears in three assertions.
const testTagWorkspace = "workspace"

// TestBuildCloudHypervisorArgsNoShares asserts the args list is
// well-formed and contains no --fs when no shares are configured.
func TestBuildCloudHypervisorArgsNoShares(t *testing.T) {
	cfg := validCfg()
	cfg = cfg.withDefaults()
	args := buildCloudHypervisorArgs(cfg, nil)
	if slices.Contains(args, "--fs") {
		t.Errorf("unexpected --fs in args: %v", args)
	}
	if !slices.Contains(args, "--kernel") {
		t.Errorf("missing --kernel: %v", args)
	}
}

// TestBuildCloudHypervisorArgsMultiShare proves --fs is emitted
// exactly once with N tokens for N shares. Catches the regression
// that landed before TestWorkspaceRoundTripE2E (which surfaced it):
// repeating --fs makes cloud-hypervisor reject the whole argv.
func TestBuildCloudHypervisorArgsMultiShare(t *testing.T) {
	cfg := validCfg()
	cfg = cfg.withDefaults()
	cfg.Shares = []*ShareConfig{
		{Tag: "bundles", Mountpoint: "/bundles"},
		{Tag: testTagWorkspace, Mountpoint: "/workspace"},
	}
	args := buildCloudHypervisorArgs(cfg,
		[]string{"/tmp/bundles.sock", "/tmp/workspace.sock"})

	fsCount := 0
	for _, a := range args {
		if a == "--fs" {
			fsCount++
		}
	}
	if fsCount != 1 {
		t.Errorf("--fs count = %d, want 1 (CH rejects --fs A --fs B): %v", fsCount, args)
	}

	// The two socket tokens follow --fs in order.
	idx := slices.Index(args, "--fs")
	if idx == -1 || idx+2 >= len(args) {
		t.Fatalf("args too short for --fs+2 tokens: %v", args)
	}
	if !strings.Contains(args[idx+1], "tag=bundles") {
		t.Errorf("first --fs token = %q, want tag=bundles", args[idx+1])
	}
	if !strings.Contains(args[idx+2], "tag=workspace") {
		t.Errorf("second --fs token = %q, want tag=workspace", args[idx+2])
	}
}

// TestBuildCloudHypervisorArgsSingleShare exercises the boundary
// between zero and many — one share still uses the variadic form
// (one --fs followed by one token), not two consecutive args.
func TestBuildCloudHypervisorArgsSingleShare(t *testing.T) {
	cfg := validCfg()
	cfg = cfg.withDefaults()
	cfg.Shares = []*ShareConfig{{Tag: "only", Mountpoint: "/only"}}
	args := buildCloudHypervisorArgs(cfg, []string{"/tmp/only.sock"})

	idx := slices.Index(args, "--fs")
	if idx == -1 {
		t.Fatalf("missing --fs: %v", args)
	}
	if idx+1 >= len(args) {
		t.Fatalf("--fs at end with no value: %v", args)
	}
	if !strings.Contains(args[idx+1], "tag=only,socket=/tmp/only.sock") {
		t.Errorf("--fs token = %q", args[idx+1])
	}
}
