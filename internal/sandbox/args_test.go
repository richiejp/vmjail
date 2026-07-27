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

const (
	testDiskPath        = "/var/lib/daft/workspace.qcow2"
	testDiskID          = "wsdisk"
	testAPISocket       = "/run/daft/vm.sock"
	testErrMustAbsolute = "must be absolute"
	testWorkspaceMount  = "/workspace"
)

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

func TestBuildCloudHypervisorArgsWorkspaceDisk(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.APISocket = testAPISocket
	cfg.WorkspaceDisk = &DiskConfig{Path: testDiskPath, ID: testDiskID}
	args := buildCloudHypervisorArgs(cfg, nil)
	want := map[string]string{
		"--disk":       "path=/var/lib/daft/workspace.qcow2,id=wsdisk,image_type=qcow2",
		"--api-socket": "path=/run/daft/vm.sock",
	}
	for flag, value := range want {
		at := slices.Index(args, flag)
		if at < 0 {
			t.Errorf("args missing %q: %v", flag, args)
			continue
		}
		if at+1 >= len(args) || args[at+1] != value {
			t.Errorf("args missing %q %q: %v", flag, value, args)
		}
	}
}

func TestConfigValidatesWorkspaceDisk(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "missing api socket", cfg: Config{WorkspaceDisk: &DiskConfig{Path: testDiskPath, ID: testDiskID}}, want: "APISocket is required"},
		{name: "relative api socket", cfg: Config{APISocket: "api.sock"}, want: testErrMustAbsolute},
		{name: "relative disk", cfg: Config{APISocket: testAPISocket, WorkspaceDisk: &DiskConfig{Path: "disk", ID: testDiskID}}, want: testErrMustAbsolute},
		{name: "missing disk id", cfg: Config{APISocket: testAPISocket, WorkspaceDisk: &DiskConfig{Path: testDiskPath}}, want: "ID is required"},
		{name: "unsafe disk id", cfg: Config{APISocket: testAPISocket, WorkspaceDisk: &DiskConfig{Path: testDiskPath, ID: "bad,id"}}, want: "reserved character"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg().withDefaults()
			cfg.APISocket = tc.cfg.APISocket
			cfg.WorkspaceDisk = tc.cfg.WorkspaceDisk
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate error = %v, want substring %q", err, tc.want)
			}
		})
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
		{Tag: testTagWorkspace, Mountpoint: testWorkspaceMount},
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
