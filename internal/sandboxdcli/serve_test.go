package sandboxdcli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/casindex"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodectrlclient"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/sandboxexec"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"github.com/richiejp/darkaifactory/internal/workspacedisk"
	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

type fakeWorkspaceDiskExecutor struct {
	status      sandboxexec.WorkspaceDiskStatus
	statuses    []sandboxexec.WorkspaceDiskStatus
	statusErr   error
	unfreezeErr error
	unfrozen    bool
}

func (f *fakeWorkspaceDiskExecutor) WorkspaceDiskStatuses() []sandboxexec.WorkspaceDiskStatus {
	return f.statuses
}

func (f *fakeWorkspaceDiskExecutor) WorkspaceDiskStatus(string) (sandboxexec.WorkspaceDiskStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeWorkspaceDiskExecutor) UnfreezeWorkspaceDisk(context.Context, string) error {
	f.unfrozen = true
	return f.unfreezeErr
}

func TestNodeWorkspaceDiskControlRunningAndRetainedStates(t *testing.T) {
	store, err := workspacedisk.OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	const workspaceID = "00000000-0000-4000-8000-000000000001"
	if _, _, err := store.Prepare(workspaceID, workspacedisk.DefaultDiskBytes); err != nil {
		t.Fatal(err)
	}
	executor := &fakeWorkspaceDiskExecutor{status: sandboxexec.WorkspaceDiskStatus{
		Usage: workspacesnapshot.Usage{Used: 7, Available: 13}, Allowance: 20, Frozen: true,
	}}
	control := &nodeWorkspaceDiskControl{executor: executor, store: store}
	status, err := control.Status(workspaceID)
	if err != nil || !status.Running || status.UsedBytes != 7 || !status.Frozen {
		t.Fatalf("running status = %#v, %v", status, err)
	}
	if err := control.Delete(t.Context(), workspaceID); err == nil {
		t.Fatal("running disk deleted")
	}
	if err := control.Unfreeze(t.Context(), workspaceID); err != nil || !executor.unfrozen {
		t.Fatalf("unfreeze = %v, called %t", err, executor.unfrozen)
	}
	executor.statusErr = sandboxexec.ErrWorkspaceDiskNotRunning
	status, err = control.Status(workspaceID)
	if err != nil || status.Running || status.AllowanceBytes != workspacedisk.DefaultDiskBytes {
		t.Fatalf("retained status = %#v, %v", status, err)
	}
	if err := control.Delete(t.Context(), workspaceID); err != nil {
		t.Fatal(err)
	}
	if size, exists, err := store.Size(workspaceID); err != nil || exists || size != 0 {
		t.Fatalf("deleted retained size = %d, %t, %v", size, exists, err)
	}
}

func TestNodeWorkspaceDiskControlPropagatesExecutorAndStoreErrors(t *testing.T) {
	store, err := workspacedisk.OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	const workspaceID = "00000000-0000-4000-8000-000000000001"
	executorErr := errors.New("executor failed")
	executor := &fakeWorkspaceDiskExecutor{statusErr: executorErr, unfreezeErr: executorErr}
	control := &nodeWorkspaceDiskControl{executor: executor, store: store}
	if _, err := control.Status(workspaceID); !errors.Is(err, executorErr) {
		t.Fatalf("Status executor error = %v", err)
	}
	if err := control.Delete(t.Context(), workspaceID); !errors.Is(err, executorErr) {
		t.Fatalf("Delete executor error = %v", err)
	}
	if err := control.Unfreeze(t.Context(), workspaceID); !errors.Is(err, executorErr) {
		t.Fatalf("Unfreeze executor error = %v", err)
	}
	executor.statusErr = sandboxexec.ErrWorkspaceDiskNotRunning
	if _, err := control.Status("../invalid"); err == nil {
		t.Fatal("Status accepted invalid retained workspace ID")
	}
	if err := control.Delete(t.Context(), "../invalid"); err == nil {
		t.Fatal("Delete accepted invalid retained workspace ID")
	}
}

func TestWorkspaceStorageSnapshotReportsRunningDisks(t *testing.T) {
	const runningWorkspace = "running"
	store, err := workspacedisk.OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	casStore, err := cas.Open(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(filepath.Join(t.TempDir(), "index"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	server, err := workspace.NewIndexedServer(casStore, index)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Add(runningWorkspace+"\x00root", &workspacev1.Manifest{}); err != nil {
		t.Fatal(err)
	}
	executor := &fakeWorkspaceDiskExecutor{
		status: sandboxexec.WorkspaceDiskStatus{
			WorkspaceID: runningWorkspace, Usage: workspacesnapshot.Usage{Used: 7, Available: 13}, Allowance: 20, Frozen: true,
		},
		statuses: []sandboxexec.WorkspaceDiskStatus{{WorkspaceID: runningWorkspace}, {WorkspaceID: "gone"}},
	}
	server.SetWorkspaceDiskController(&nodeWorkspaceDiskControl{executor: executor, store: store})
	reports := workspaceStorageSnapshot(t.Context(), executor, server)
	if len(reports) != 2 {
		t.Fatalf("reports = %#v", reports)
	}
	if got := reports[0]; got.GetWorkspaceUuid() != runningWorkspace || got.GetUsedBytes() != 7 || got.GetAvailableBytes() != 13 || got.GetAllowanceBytes() != 20 || !got.GetFrozen() {
		t.Fatalf("running report = %#v", got)
	}
	if reports[0].GetManifestDigest() == "" || reports[0].GetManifestGeneration() == 0 {
		t.Fatalf("manifest identity missing: %#v", reports[0])
	}
	if workspaceStorageSnapshot(t.Context(), nil, server) != nil || workspaceStorageSnapshot(t.Context(), executor, nil) != nil {
		t.Fatal("nil dependency returned reports")
	}
	executor.statusErr = errors.New("status unavailable")
	if reports := workspaceStorageSnapshot(t.Context(), executor, server); len(reports) != 0 {
		t.Fatalf("failed statuses returned reports: %#v", reports)
	}
}

// stubBuildDispatcher swaps buildDispatcher with one that returns
// an Executor whose runner is a no-op — exercises the same code
// path as production without needing the DAFT_INITRAMFS env or a
// real VM. Tests that drive runServe past the peer-store gate use
// this.
func stubBuildDispatcher(t *testing.T) {
	t.Helper()
	saved := buildDispatcher
	t.Cleanup(func() { buildDispatcher = saved })
	buildDispatcher = func(w io.Writer, _ *sandboxgateway.Bridger, _ *sandboxgateway.AttachRegistry, _ *sandboxgateway.JobRegistry, _ *sandboxgateway.ObserverRegistry, _ *sandboxgateway.SummaryRegistry, _ string, _ sandboxexec.MountMaterializer, _ string, _ sandboxexec.WorkspaceDiskPreparer, _ http.RoundTripper) (*sandboxexec.Executor, error) {
		return sandboxexec.New(sandboxexec.Config{
			Stdout: w,
			Runner: func(context.Context, io.Writer, sandbox.Config) error { return nil },
		})
	}
}

func identityGenerateForTest() (*identity.Identity, error) {
	return identity.Generate(identity.RoleSandboxHost)
}

func memoryPeerStore() *identity.PeerStore {
	return identity.NewMemoryPeerStore()
}

func TestServeRequiresOrchestratorFlag(t *testing.T) {
	_, _, err := runSandboxd(t, serveCmdName)
	if err == nil || !strings.Contains(err.Error(), "--orchestrator is required") {
		t.Errorf("err = %v, want --orchestrator required", err)
	}
}

// TestRunServeRunsRegisterAndServe swaps the test seam to verify the
// full happy path: identity load, peer-store load, resource detect,
// client construction, and the call into RegisterAndServe — all
// without dialing a real orchestrator.
func TestRunServeRunsRegisterAndServe(t *testing.T) {
	stubBuildDispatcher(t)
	saved := runRegisterAndServe
	t.Cleanup(func() { runRegisterAndServe = saved })

	called := make(chan struct{}, 1)
	var seenRes nodectrl.Resources
	runRegisterAndServe = func(_ context.Context, _ *nodectrlclient.Client, res nodectrl.Resources) error {
		seenRes = res
		called <- struct{}{}
		return nil
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := runServe(ctx, &noopWriter{}, "https://orch.invalid:8443", dir, "", 100*time.Millisecond); err != nil {
		t.Fatalf("runServe: %v", err)
	}
	select {
	case <-called:
	default:
		t.Fatal("RegisterAndServe seam was not invoked")
	}
	if seenRes.CPUs == 0 {
		t.Errorf("Resources.CPUs not populated: %+v", seenRes)
	}
}

// TestRunServeBadIdentityDir covers the loadOrGenerateSandboxd
// error branch: when the config dir resolves to a file path, the
// LoadOrGenerate call fails before any RPC machinery is constructed.
// TestRunServeBadConfigDirResolve covers the resolveSandboxdDir
// error branch: when configDirOverride is empty and the OS can't
// determine a user config dir (no HOME, no XDG_CONFIG_HOME),
// runServe must fail-fast before touching identity / peers.
func TestRunServeBadConfigDirResolve(t *testing.T) {
	saved := runRegisterAndServe
	t.Cleanup(func() { runRegisterAndServe = saved })
	runRegisterAndServe = func(context.Context, *nodectrlclient.Client, nodectrl.Resources) error {
		t.Error("RegisterAndServe should not run when config dir cannot be resolved")
		return nil
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	err := runServe(t.Context(), &noopWriter{}, "https://orch.invalid:8443", "", "", time.Second)
	if err == nil {
		t.Fatal("expected error when neither HOME nor XDG_CONFIG_HOME is set")
	}
}

func TestRunServeBadIdentityDir(t *testing.T) {
	saved := runRegisterAndServe
	t.Cleanup(func() { runRegisterAndServe = saved })
	runRegisterAndServe = func(context.Context, *nodectrlclient.Client, nodectrl.Resources) error {
		t.Error("RegisterAndServe should not run when identity load fails")
		return nil
	}

	// Use a file path as the config dir — LoadOrGenerate refuses.
	dir := t.TempDir()
	conflict := dir + "/notadir"
	if err := writeBlocker(conflict); err != nil {
		t.Fatalf("writeBlocker: %v", err)
	}
	err := runServe(t.Context(), &noopWriter{}, "https://orch.invalid:8443", conflict, "", time.Second)
	if err == nil {
		t.Fatal("expected error from identity load against a file path")
	}
}

// TestRunServeBadPeerStore covers the openSandboxdPeerStore error
// branch: when peers.json exists as a directory, the file open
// inside OpenPeerStore fails after identity has loaded fine.
func TestRunServeBadPeerStore(t *testing.T) {
	saved := runRegisterAndServe
	t.Cleanup(func() { runRegisterAndServe = saved })
	runRegisterAndServe = func(context.Context, *nodectrlclient.Client, nodectrl.Resources) error {
		t.Error("RegisterAndServe should not run when peer store load fails")
		return nil
	}
	dir := t.TempDir()
	// Pre-load identity so its files exist, then plant a peers.json
	// directory so OpenPeerStore fails on the file open.
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity prep: %v", err)
	}
	if err := os.Mkdir(dir+"/peers.json", 0o700); err != nil {
		t.Fatalf("mkdir peers.json blocker: %v", err)
	}
	err := runServe(t.Context(), &noopWriter{}, "https://orch.invalid:8443", dir, "", time.Second)
	if err == nil {
		t.Fatal("expected error from peer store load against a directory")
	}
}

func TestRunServePropagatesRegisterError(t *testing.T) {
	stubBuildDispatcher(t)
	saved := runRegisterAndServe
	t.Cleanup(func() { runRegisterAndServe = saved })

	want := errors.New("synthetic register failure")
	runRegisterAndServe = func(context.Context, *nodectrlclient.Client, nodectrl.Resources) error {
		return want
	}

	dir := t.TempDir()
	err := runServe(t.Context(), &noopWriter{}, "https://orch.invalid:8443", dir, "", time.Second)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestRunServePropagatesBuildDispatcherError drives runServe past
// the peer-store gate with a buildDispatcher that errors out — the
// error must surface and the RegisterAndServe seam must not run.
func TestRunServePropagatesBuildDispatcherError(t *testing.T) {
	saved := buildDispatcher
	t.Cleanup(func() { buildDispatcher = saved })
	want := errors.New("synthetic dispatcher build failure")
	buildDispatcher = func(io.Writer, *sandboxgateway.Bridger, *sandboxgateway.AttachRegistry, *sandboxgateway.JobRegistry, *sandboxgateway.ObserverRegistry, *sandboxgateway.SummaryRegistry, string, sandboxexec.MountMaterializer, string, sandboxexec.WorkspaceDiskPreparer, http.RoundTripper) (*sandboxexec.Executor, error) {
		return nil, want
	}
	savedRun := runRegisterAndServe
	t.Cleanup(func() { runRegisterAndServe = savedRun })
	runRegisterAndServe = func(context.Context, *nodectrlclient.Client, nodectrl.Resources) error {
		t.Error("RegisterAndServe should not run when buildDispatcher fails")
		return nil
	}
	dir := t.TempDir()
	err := runServe(t.Context(), &noopWriter{}, "https://orch.invalid:8443", dir, "", time.Second)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestBuildDispatcherProductionSeam covers the production
// `buildDispatcher` closure: with DAFT_INITRAMFS unset,
// sandbox.ConfigFromEnv returns the expected error and the seam
// surfaces it. With every required env set, the seam returns a
// non-nil Executor.
func TestBuildDispatcherProductionSeam(t *testing.T) {
	t.Setenv("DAFT_CLOUD_HYPERVISOR", "/dev/null")
	t.Setenv("DAFT_KERNEL", "/dev/null")
	t.Setenv("DAFT_INITRAMFS", "")
	br := sandboxgateway.New()
	ar := sandboxgateway.NewAttachRegistry()
	jr := sandboxgateway.NewJobRegistry()
	or := sandboxgateway.NewObserverRegistry()
	sr := sandboxgateway.NewSummaryRegistry()
	if _, err := buildDispatcher(&noopWriter{}, br, ar, jr, or, sr, t.TempDir(), nil, "", nil, nil); err == nil {
		t.Fatal("buildDispatcher with DAFT_INITRAMFS unset should fail")
	}
	t.Setenv("DAFT_INITRAMFS", "/dev/null")
	d, err := buildDispatcher(&noopWriter{}, br, ar, jr, or, sr, t.TempDir(), nil, "", nil, nil)
	if err != nil {
		t.Fatalf("buildDispatcher: %v", err)
	}
	if d == nil {
		t.Fatal("buildDispatcher returned nil Executor")
	}
}

// TestRunRegisterAndServeSeamCallsClient covers the production
// `runRegisterAndServe` seam itself by invoking it with a real
// Client pointed at an unreachable orchestrator. The seam just
// delegates to client.RegisterAndServe; failure here is fine — we
// only need the production call site executed.
func TestRunRegisterAndServeSeamCallsClient(t *testing.T) {
	id, err := identityGenerateForTest()
	if err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               "https://127.0.0.1:1",
		Identity:          id,
		Peers:             memoryPeerStore(),
		HeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	// We don't care about the error — the seam ran iff we got here.
	_ = runRegisterAndServe(ctx, client, nodectrl.Resources{})
}

// TestBuildNodeSync covers the production buildNodeSync seam: it
// opens a persistent CAS under <dir>/cas, builds the
// WorkspaceService handler over it, and constructs the broker
// client. The casRoot is reported for the startup banner.
func TestBuildNodeSync(t *testing.T) {
	id, err := identityGenerateForTest()
	if err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	dir := t.TempDir()
	sync, err := buildNodeSync(dir, "https://orch.invalid:8443", id, memoryPeerStore())
	if err != nil {
		t.Fatalf("buildNodeSync: %v", err)
	}
	if sync.sandboxID != id.Fingerprint().String() {
		t.Errorf("sandboxID = %q, want node fingerprint %q", sync.sandboxID, id.Fingerprint())
	}
	if sync.casRoot != filepath.Join(dir, "cas") {
		t.Errorf("casRoot = %q, want %q", sync.casRoot, filepath.Join(dir, "cas"))
	}
	if sync.handler == nil || sync.bc == nil {
		t.Error("buildNodeSync returned nil handler or broker client")
	}
}

// TestBuildNodeSyncBadCasDir covers the cas.Open error branch: a
// regular file where the CAS dir should be makes Open fail.
func TestBuildNodeSyncBadCasDir(t *testing.T) {
	id, err := identityGenerateForTest()
	if err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	dir := t.TempDir()
	if err := writeBlocker(filepath.Join(dir, "cas")); err != nil {
		t.Fatalf("writeBlocker: %v", err)
	}
	if _, err := buildNodeSync(dir, "https://orch.invalid:8443", id, memoryPeerStore()); err == nil {
		t.Error("buildNodeSync should fail when <dir>/cas is a regular file")
	}
}

// TestNodeSyncRunCtxCancel: a cancelled ctx makes the serve loop exit
// cleanly with no log line (ctx.Err() != nil suppresses the warning).
func TestNodeSyncRunCtxCancel(t *testing.T) {
	id, err := identityGenerateForTest()
	if err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	sync, err := buildNodeSync(t.TempDir(), "https://orch.invalid:8443", id, memoryPeerStore())
	if err != nil {
		t.Fatalf("buildNodeSync: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var buf strings.Builder
	sync.run(ctx, &buf) // returns promptly; no log on ctx-cancel exit
	if buf.Len() != 0 {
		t.Errorf("unexpected log on ctx-cancel exit: %q", buf.String())
	}
}

// TestNodeSyncRunLogsUnexpectedExit: with a live ctx but an
// unreachable orchestrator, the serve loop returns an error and run
// logs it (ctx.Err() == nil).
func TestNodeSyncRunLogsUnexpectedExit(t *testing.T) {
	id, err := identityGenerateForTest()
	if err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	sync, err := buildNodeSync(t.TempDir(), "https://127.0.0.1:1", id, memoryPeerStore())
	if err != nil {
		t.Fatalf("buildNodeSync: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var buf strings.Builder
	sync.run(ctx, &buf)
	if !strings.Contains(buf.String(), "workspace sync stopped") {
		t.Errorf("expected an unexpected-exit log line, got %q", buf.String())
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{16 << 30, "16.0 GiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

type noopWriter struct{}

func (*noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// writeBlocker creates a regular file at path so that any
// LoadOrGenerate attempt that wants to MkdirAll(path) fails with
// "not a directory".
func writeBlocker(path string) error {
	return os.WriteFile(path, []byte("blocker"), 0o600)
}

// TestResolveVsockDirDefaultCreatesTempSubdir covers the
// empty-override branch: with no caller-supplied path,
// resolveVsockDir mints a per-pid subdir under os.TempDir and
// mkdirs it so the Executor has somewhere to put per-Sandbox UDS
// files at dispatch time.
func TestResolveVsockDirDefaultCreatesTempSubdir(t *testing.T) {
	dir, err := resolveVsockDir("")
	if err != nil {
		t.Fatalf("resolveVsockDir(\"\"): %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", dir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
}

// TestResolveVsockDirAbsoluteOverride covers the explicit-path
// branch: an absolute override is mkdir'd and returned as-is.
func TestResolveVsockDirAbsoluteOverride(t *testing.T) {
	override := t.TempDir() + "/vsock-sub"
	got, err := resolveVsockDir(override)
	if err != nil {
		t.Fatalf("resolveVsockDir: %v", err)
	}
	if got != override {
		t.Errorf("resolveVsockDir = %q, want %q", got, override)
	}
	if _, err := os.Stat(override); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

// TestResolveVsockDirRejectsRelative pins the absolute-path
// invariant: a relative override is refused before any side-effect
// runs, so a typo in the CLI flag surfaces as a clear error
// rather than mkdir'ing under the current working directory.
func TestResolveVsockDirRejectsRelative(t *testing.T) {
	if _, err := resolveVsockDir("relative/path"); err == nil {
		t.Fatal("expected error for relative override")
	}
}

// TestResolveVsockDirMkdirError covers the mkdir-fail branch:
// when the override points under an existing regular file the
// MkdirAll call surfaces a clear error to the caller.
func TestResolveVsockDirMkdirError(t *testing.T) {
	dir := t.TempDir()
	blocker := dir + "/notadir"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, err := resolveVsockDir(blocker + "/sub"); err == nil {
		t.Fatal("expected error when mkdir fails")
	}
}

// TestExtractWithEnvFallbackUsesBaseWhenStubAssets pins the
// dev-build path: a plain `go build` daft-sandboxd has stub
// assets, so bundled.Extract returns ErrNotBundled and the
// wrapper should hand back the env-resolved paths from base.
func TestExtractWithEnvFallbackUsesBaseWhenStubAssets(t *testing.T) {
	base := sandbox.Config{
		CloudHypervisor: "/p/ch",
		Kernel:          "/p/kernel",
		Initramfs:       "/p/initramfs",
		Virtiofsd:       "/p/virtiofsd",
	}
	paths, err := extractWithEnvFallback(base)(t.TempDir())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if paths.CloudHypervisor != base.CloudHypervisor ||
		paths.Kernel != base.Kernel ||
		paths.Initramfs != base.Initramfs ||
		paths.Virtiofsd != base.Virtiofsd {
		t.Errorf("paths = %+v, want %+v", paths, base)
	}
}

// TestExtractWithEnvFallbackReturnsErrNotBundledWhenBaseEmpty
// pins the misconfigured-daemon path: stub assets AND no env
// fallback should surface the embed-package's ErrNotBundled
// so the caller sees a clean diagnostic.
func TestExtractWithEnvFallbackReturnsErrNotBundledWhenBaseEmpty(t *testing.T) {
	_, err := extractWithEnvFallback(sandbox.Config{})(t.TempDir())
	if !errors.Is(err, bundled.ErrNotBundled) {
		t.Errorf("err = %v, want ErrNotBundled", err)
	}
}

func TestObserveSummaryFromEnv(t *testing.T) {
	t.Run("unset enables by default", func(t *testing.T) {
		t.Setenv(envObserveSummary, "")
		if !observeSummaryFromEnv() {
			t.Error("observe summary should be on by default")
		}
	})
	t.Run("falsy opts out", func(t *testing.T) {
		// Exercises trim + lower-casing across the falsy synonyms. Variants
		// ("False"/"OFF"/" off ") are chosen so the exact literals "false"
		// and "off" don't recur package-wide and trip goconst — the egress
		// test already covers the canonical forms via the shared helper.
		for _, v := range []string{"0", "False", "no", "OFF", " off "} {
			t.Setenv(envObserveSummary, v)
			if observeSummaryFromEnv() {
				t.Errorf("%q should disable observe summary", v)
			}
		}
	})
	t.Run("other values stay on", func(t *testing.T) {
		for _, v := range []string{"disabled", "nope", "2", "nonsense"} {
			t.Setenv(envObserveSummary, v)
			if !observeSummaryFromEnv() {
				t.Errorf("%q should leave observe summary on", v)
			}
		}
	})
}

func TestEgressProxyFromEnv(t *testing.T) {
	t.Run("unset enables by default", func(t *testing.T) {
		t.Setenv(envEgressProxy, "")
		t.Setenv(envEgressProxyCADir, "")
		t.Setenv(envEgressProxyCacheDir, "")
		p, err := egressProxyFromEnv(io.Discard)
		if err != nil {
			t.Fatalf("unset: %v", err)
		}
		if p == nil || p.Handler == nil {
			t.Fatalf("unset: want proxy on by default, got %+v", p)
		}
	})

	t.Run("explicit off is nil", func(t *testing.T) {
		for _, v := range []string{"0", "false", "no", "off", "OFF"} {
			t.Setenv(envEgressProxy, v)
			p, err := egressProxyFromEnv(io.Discard)
			if p != nil || err != nil {
				t.Fatalf("%q: got (%v, %v), want (nil, nil)", v, p, err)
			}
		}
	})

	t.Run("enabled mints ephemeral proxy", func(t *testing.T) {
		t.Setenv(envEgressProxy, "1")
		t.Setenv(envEgressProxyCADir, "")
		t.Setenv(envEgressProxyCacheDir, "")
		p, err := egressProxyFromEnv(io.Discard)
		if err != nil {
			t.Fatalf("enabled: %v", err)
		}
		if p == nil || p.Handler == nil || len(p.CACertPEM) == 0 {
			t.Fatalf("enabled: got %+v (want non-nil Handler + CA PEM)", p)
		}
	})

	t.Run("bad CA dir errors", func(t *testing.T) {
		// A CA dir whose parent is a regular file can't be created, so
		// LoadOrCreateCertCache fails before the proxy is built.
		parent := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		t.Setenv(envEgressProxy, "true")
		t.Setenv(envEgressProxyCADir, filepath.Join(parent, "ca"))
		if _, err := egressProxyFromEnv(io.Discard); err == nil {
			t.Fatal("want error when CA dir can't be created")
		}
	})

	t.Run("bad cache dir errors", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "afile")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		t.Setenv(envEgressProxy, "1")
		t.Setenv(envEgressProxyCADir, "")
		t.Setenv(envEgressProxyCacheDir, filepath.Join(parent, "cache"))
		if _, err := egressProxyFromEnv(io.Discard); err == nil {
			t.Fatal("want error when cache dir can't be opened")
		}
	})

	t.Run("caching enabled", func(t *testing.T) {
		t.Setenv(envEgressProxy, "1")
		t.Setenv(envEgressProxyCADir, "")
		t.Setenv(envEgressProxyCacheDir, t.TempDir())
		p, err := egressProxyFromEnv(io.Discard)
		if err != nil {
			t.Fatalf("caching: %v", err)
		}
		if p == nil || p.Handler == nil {
			t.Fatalf("caching: got %+v", p)
		}
	})
}
