package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// clearOrchestratorEnv makes orchestrator resolution hermetic regardless
// of the developer's environment: it forces $DAFT_ORCHESTRATOR empty and
// points XDG_STATE_HOME at a throwaway dir so the local-cluster
// discovery layer can't latch onto a real `daft cluster up` the
// developer happens to have running.
func clearOrchestratorEnv(t *testing.T) {
	t.Helper()
	t.Setenv(orchestratorEnvVar, "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// chdirClean moves into a fresh temp dir so no stray ./.daft/config.json
// from the package dir leaks into resolution. t.Chdir bars t.Parallel.
func chdirClean(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// writeBadDirLocal drops a corrupt ./.daft/config.json so resolution
// fails on the dir-local read. Call after chdirClean.
func writeBadDirLocal(t *testing.T) {
	t.Helper()
	dlPath, err := dirLocalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dlPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dlPath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFileConfigMissingIsZero(t *testing.T) {
	c, err := loadFileConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("loadFileConfig(absent) err = %v, want nil", err)
	}
	if !reflect.DeepEqual(c, fileConfig{}) {
		t.Errorf("loadFileConfig(absent) = %+v, want zero value", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	want := fileConfig{
		Orchestrator: "https://h:9443", Name: "proj",
		Mounts: []mountMapping{{Local: "./src", Remote: testMountRemote, ReadOnly: true}},
	}
	if err := saveFileConfig(path, want); err != nil {
		t.Fatalf("saveFileConfig: %v", err)
	}
	got, err := loadFileConfig(path)
	if err != nil {
		t.Fatalf("loadFileConfig: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestLoadFileConfigBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileConfig(path); err == nil {
		t.Error("loadFileConfig(bad json) err = nil, want parse error")
	}
}

func TestFileConfigGetSet(t *testing.T) {
	var c fileConfig
	if ok := c.set("orchestrator", "https://x"); !ok {
		t.Fatal("set(orchestrator) returned false")
	}
	if ok := c.set("name", "n"); !ok {
		t.Fatal("set(name) returned false")
	}
	if ok := c.set("bogus", "v"); ok {
		t.Error("set(bogus) returned true, want false")
	}
	if v, ok := c.get("orchestrator"); !ok || v != "https://x" {
		t.Errorf("get(orchestrator) = %q,%v", v, ok)
	}
	if _, ok := c.get("bogus"); ok {
		t.Error("get(bogus) ok = true, want false")
	}
}

func TestResolveOrchestratorPrecedence(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	configDir := t.TempDir()

	// Nothing set anywhere → built-in default.
	got, err := resolveOrchestrator("", configDir)
	if err != nil {
		t.Fatalf("resolve(default): %v", err)
	}
	if got != defaultOrchestratorURL {
		t.Errorf("default = %q, want %q", got, defaultOrchestratorURL)
	}

	// Global config beats the default.
	if err := saveFileConfig(filepath.Join(configDir, globalConfigFile),
		fileConfig{Orchestrator: "https://global:1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveOrchestrator("", configDir); got != "https://global:1" {
		t.Errorf("global = %q, want https://global:1", got)
	}

	// Dir-local config beats global.
	dlPath, err := dirLocalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveFileConfig(dlPath, fileConfig{Orchestrator: "https://dir:2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveOrchestrator("", configDir); got != "https://dir:2" {
		t.Errorf("dir-local = %q, want https://dir:2", got)
	}

	// Env beats both config files.
	t.Setenv(orchestratorEnvVar, "https://env:3")
	if got, _ := resolveOrchestrator("", configDir); got != "https://env:3" {
		t.Errorf("env = %q, want https://env:3", got)
	}

	// Explicit flag beats everything.
	if got, _ := resolveOrchestrator("https://flag:4", configDir); got != "https://flag:4" {
		t.Errorf("flag = %q, want https://flag:4", got)
	}
}

func TestResolveWorkspaceNameDefault(t *testing.T) {
	dir := chdirClean(t)

	// No dir-local config → basename of cwd.
	got, err := resolveWorkspaceNameDefault()
	if err != nil {
		t.Fatalf("resolve(name): %v", err)
	}
	if got != filepath.Base(dir) {
		t.Errorf("name without config = %q, want %q", got, filepath.Base(dir))
	}

	// Dir-local name overrides the basename.
	dlPath, _ := dirLocalConfigPath()
	if err := saveFileConfig(dlPath, fileConfig{Name: "configured"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveWorkspaceNameDefault(); got != "configured" {
		t.Errorf("name with config = %q, want configured", got)
	}
}

func TestResolveErrorsOnBadDirLocal(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	dlPath, _ := dirLocalConfigPath()
	if err := os.MkdirAll(filepath.Dir(dlPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dlPath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveOrchestrator("", t.TempDir()); err == nil {
		t.Error("resolveOrchestrator with bad dir-local config: want error")
	}
	if _, err := resolveWorkspaceNameDefault(); err == nil {
		t.Error("resolveWorkspaceNameDefault with bad dir-local config: want error")
	}
}

func TestResolveErrorsOnBadGlobal(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, globalConfigFile), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveOrchestrator("", dir); err == nil {
		t.Error("resolveOrchestrator with bad global config: want error")
	}
}

func TestSaveFileConfigMkdirError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// blocker is a file, so MkdirAll of its "child" must fail.
	if err := saveFileConfig(filepath.Join(blocker, "config.json"), fileConfig{}); err == nil {
		t.Error("saveFileConfig under a regular file: want error")
	}
}

func TestWorkspaceSetSaveError(t *testing.T) {
	chdirClean(t)
	// A regular file named .daft blocks MkdirAll(./.daft).
	if err := os.WriteFile(dirLocalConfigDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, "workspace", "set", "name", "x"); err == nil {
		t.Error("workspace set with .daft as a file: want error")
	}
}

func TestSaveFileConfigWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	// The parent dir already exists (so MkdirAll is a no-op) but is
	// read-only, so writing the temp file inside it must fail.
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: dir needs owner-traverse for t.TempDir cleanup
	if err := saveFileConfig(filepath.Join(dir, "config.json"), fileConfig{Name: "x"}); err == nil {
		t.Error("saveFileConfig into a read-only dir: want write error")
	}
}

func TestSaveFileConfigRenameError(t *testing.T) {
	// The target path is an existing directory, so the atomic rename
	// of the temp file onto it must fail.
	target := filepath.Join(t.TempDir(), "config.json")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveFileConfig(target, fileConfig{Name: "x"}); err == nil {
		t.Error("saveFileConfig onto a directory: want rename error")
	}
}

func TestWorkspaceSetBadExistingConfig(t *testing.T) {
	chdirClean(t)
	dlPath, _ := dirLocalConfigPath()
	if err := os.MkdirAll(filepath.Dir(dlPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dlPath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	// set must read the existing file before merging; a corrupt one errors.
	if _, _, err := runDaft(t, "workspace", "set", "name", "x"); err == nil {
		t.Error("workspace set over corrupt config: want error")
	}
}

func TestGlobalsGetBadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, globalConfigFile), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, "globals", "get", "orchestrator", "--config-dir", dir); err == nil {
		t.Error("globals get over corrupt config: want error")
	}
}

func TestWorkspaceGetBadConfigErrors(t *testing.T) {
	chdirClean(t)
	dlPath, _ := dirLocalConfigPath()
	if err := os.MkdirAll(filepath.Dir(dlPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dlPath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, "workspace", "get", "name"); err == nil {
		t.Error("workspace get with bad config: want error")
	}
}

func TestDirLocalMountsAddListRemove(t *testing.T) {
	chdirClean(t)
	if m, err := dirLocalMounts(); err != nil || len(m) != 0 {
		t.Fatalf("dirLocalMounts (empty) = %v, %v", m, err)
	}
	if _, err := addDirLocalMount(mountMapping{Local: "./a", Remote: "/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := addDirLocalMount(mountMapping{Local: "./b", Remote: "/b", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	// Re-adding /a replaces rather than duplicates.
	if _, err := addDirLocalMount(mountMapping{Local: "./a2", Remote: "/a"}); err != nil {
		t.Fatal(err)
	}
	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %+v, want 2 entries", mounts)
	}
	var aLocal string
	for _, m := range mounts {
		if m.Remote == "/a" {
			aLocal = m.Local
		}
	}
	if aLocal != "./a2" {
		t.Errorf("/a local = %q, want ./a2 (replaced)", aLocal)
	}
	found, err := removeDirLocalMount("/b")
	if err != nil || !found {
		t.Fatalf("removeDirLocalMount(/b) = %v, %v", found, err)
	}
	if found, _ := removeDirLocalMount("/missing"); found {
		t.Error("removeDirLocalMount(/missing) = true, want false")
	}
	if mounts, _ := dirLocalMounts(); len(mounts) != 1 {
		t.Errorf("after remove = %+v, want 1 entry", mounts)
	}
}

func TestDirLocalMountHelpersSurfaceBadConfig(t *testing.T) {
	chdirClean(t)
	writeBadDirLocal(t)
	if _, err := dirLocalMounts(); err == nil {
		t.Error("dirLocalMounts over corrupt config: want error")
	}
	if _, err := addDirLocalMount(mountMapping{Local: "./x", Remote: "/x"}); err == nil {
		t.Error("addDirLocalMount over corrupt config: want error")
	}
	if _, err := removeDirLocalMount("/a"); err == nil {
		t.Error("removeDirLocalMount over corrupt config: want error")
	}
}

func TestListDirLocalMountsReadOnly(t *testing.T) {
	chdirClean(t)
	if _, err := addDirLocalMount(mountMapping{Local: "./ro", Remote: "/ro", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := listDirLocalMounts(&buf); err != nil {
		t.Fatalf("listDirLocalMounts: %v", err)
	}
	if !strings.Contains(buf.String(), "(read-only)") {
		t.Errorf("list = %q, want a (read-only) marker", buf.String())
	}
}

func TestWorkspaceMountOneArgErrors(t *testing.T) {
	chdirClean(t)
	if _, _, err := runDaft(t, "workspace", "mount", "only-one-arg"); err == nil {
		t.Error("single positional arg should error (need <local> <in-vm-target>)")
	}
}

func TestWorkspaceMountBareSurfacesBadConfig(t *testing.T) {
	chdirClean(t)
	writeBadDirLocal(t)
	if _, _, err := runDaft(t, "workspace", "mount"); err == nil {
		t.Error("bare mount over corrupt config: want error")
	}
}

func TestValidateConfigValue(t *testing.T) {
	if err := validateConfigValue("orchestrator", "https://ok:1"); err != nil {
		t.Errorf("https URL rejected: %v", err)
	}
	if err := validateConfigValue("orchestrator", "http://ok:1"); err != nil {
		t.Errorf("http URL rejected: %v", err)
	}
	if err := validateConfigValue("orchestrator", "host:1"); err == nil {
		t.Error("scheme-less orchestrator accepted, want error")
	}
	if err := validateConfigValue("name", "anything"); err != nil {
		t.Errorf("name value rejected: %v", err)
	}
}

// --- command-level tests ---

func TestGlobalsSetGet(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runDaft(t, "globals", "set", "orchestrator",
		"https://remote:8443", "--config-dir", dir); err != nil {
		t.Fatalf("globals set: %v", err)
	}
	out, _, err := runDaft(t, "globals", "get", "orchestrator", "--config-dir", dir)
	if err != nil {
		t.Fatalf("globals get: %v", err)
	}
	if strings.TrimSpace(out) != "https://remote:8443" {
		t.Errorf("globals get = %q, want https://remote:8443", out)
	}
}

func TestGlobalsGetBarePrintsAllKeys(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runDaft(t, "globals", "get", "--config-dir", dir)
	if err != nil {
		t.Fatalf("globals get (bare): %v", err)
	}
	if !strings.Contains(out, "orchestrator = ") {
		t.Errorf("bare get = %q, want an orchestrator line", out)
	}
}

func TestGlobalsSetRejectsBadKeyAndValue(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runDaft(t, "globals", "set", "bogus", "v", "--config-dir", dir); err == nil {
		t.Error("set bogus key err = nil, want unknown-key error")
	}
	if _, _, err := runDaft(t, "globals", "set", "orchestrator", "no-scheme", "--config-dir", dir); err == nil {
		t.Error("set scheme-less orchestrator err = nil, want validation error")
	}
}

func TestWorkspaceSetGetDirLocal(t *testing.T) {
	chdirClean(t)
	if _, _, err := runDaft(t, "workspace", "set", "name", "proj-x"); err != nil {
		t.Fatalf("workspace set: %v", err)
	}
	// The file landed at ./.daft/config.json.
	dlPath, _ := dirLocalConfigPath()
	if _, err := os.Stat(dlPath); err != nil {
		t.Fatalf("dir-local config not written: %v", err)
	}
	out, _, err := runDaft(t, "workspace", "get", "name")
	if err != nil {
		t.Fatalf("workspace get: %v", err)
	}
	if strings.TrimSpace(out) != "proj-x" {
		t.Errorf("workspace get name = %q, want proj-x", out)
	}
}

// TestWorkspaceCreateDialsResolvedOrchestrator proves --orchestrator
// is no longer a required up-front flag: with none supplied the
// command resolves a value and proceeds to dial, failing at the dead
// endpoint rather than at a flag guard.
func TestWorkspaceCreateDialsResolvedOrchestrator(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	dir := t.TempDir()
	_, _, err := runDaft(t, "workspace", "create", "--name", "demo",
		"--orchestrator", unreachableOrchestrator, "--config-dir", dir, "--timeout", "2s")
	if err == nil {
		t.Error("create against dead orchestrator: want dial error")
	}
}

// The remaining cases pin that a resolution failure (corrupt
// dir-local config) surfaces from each command path that newly
// resolves the orchestrator, rather than being swallowed.

func TestWorkspaceCreateSurfacesResolutionError(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	writeBadDirLocal(t)
	if _, _, err := runDaft(t, "workspace", "create", "--name", "demo",
		"--config-dir", t.TempDir(), "--timeout", "2s"); err == nil {
		t.Error("create with corrupt dir-local config: want resolution error")
	}
}

func TestNodeListSurfacesResolutionError(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	writeBadDirLocal(t)
	if _, _, err := runDaft(t, "node", nodeListSubName,
		"--config-dir", t.TempDir(), "--timeout", "2s"); err == nil {
		t.Error("node list with corrupt dir-local config: want resolution error")
	}
}

// A corrupt dir-local config must surface from exec whether the
// failing read is the workspace-name default (empty workspace) or the
// orchestrator resolution (workspace given). Neither reaches the
// (nil) dial dep.
func TestRunWorkspaceExecSurfacesResolutionError(t *testing.T) {
	for _, ws := range []string{"", "ws"} {
		t.Run("workspace="+ws, func(t *testing.T) {
			clearOrchestratorEnv(t)
			chdirClean(t)
			writeBadDirLocal(t)
			err := runWorkspaceExecWith(context.Background(), io.Discard, io.Discard,
				workspaceExecOpts{workspace: ws}, execDeps{})
			if err == nil {
				t.Error("exec with corrupt dir-local config: want resolution error")
			}
		})
	}
}

func TestRunWorkspaceForwardSurfacesResolutionError(t *testing.T) {
	for _, ws := range []string{"", "x"} {
		t.Run("workspace="+ws, func(t *testing.T) {
			clearOrchestratorEnv(t)
			chdirClean(t)
			writeBadDirLocal(t)
			err := runWorkspaceForwardWith(context.Background(), io.Discard,
				workspaceForwardOpts{workspace: ws, localPort: 1, containerPort: 1}, forwardDeps{})
			if err == nil {
				t.Error("forward with corrupt dir-local config: want resolution error")
			}
		})
	}
}

func TestRunWorkspaceAttachSurfacesResolutionError(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	writeBadDirLocal(t)
	err := runWorkspaceAttach(context.Background(), io.Discard,
		workspaceAttachOpts{workspace: "w", sandbox: "s"})
	if err == nil {
		t.Error("attach with corrupt dir-local config: want resolution error")
	}
}

func TestGlobalsResolveShowsValueAndSource(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	// Dir-local orchestrator + name win their respective chains.
	dlPath, _ := dirLocalConfigPath()
	if err := saveFileConfig(dlPath, fileConfig{Orchestrator: "https://dl:7", Name: "dlname"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runDaft(t, "globals", "resolve", "--config-dir", t.TempDir())
	if err != nil {
		t.Fatalf("globals resolve: %v", err)
	}
	if !strings.Contains(out, "https://dl:7") || !strings.Contains(out, string(sourceDirLocal)) {
		t.Errorf("resolve output missing orchestrator+source:\n%s", out)
	}
	if !strings.Contains(out, "dlname") {
		t.Errorf("resolve output missing name:\n%s", out)
	}
}

// When the orchestrator comes from the env (so the dir-local file is
// never read for it) but that file is corrupt, resolve still reports
// the orchestrator and degrades the name line to <unset> rather than
// failing the whole command.
func TestGlobalsResolveToleratesNameError(t *testing.T) {
	chdirClean(t)
	t.Setenv(orchestratorEnvVar, "https://env:9")
	writeBadDirLocal(t)
	out, _, err := runDaft(t, "globals", "resolve", "--config-dir", t.TempDir())
	if err != nil {
		t.Fatalf("globals resolve: %v", err)
	}
	if !strings.Contains(out, "https://env:9") {
		t.Errorf("resolve missing env orchestrator:\n%s", out)
	}
	if !strings.Contains(out, "<unset>") {
		t.Errorf("resolve should mark name <unset> on read error:\n%s", out)
	}
}

func TestGlobalsResolveSurfacesError(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	writeBadDirLocal(t)
	if _, _, err := runDaft(t, "globals", "resolve", "--config-dir", t.TempDir()); err == nil {
		t.Error("globals resolve with corrupt dir-local config: want error")
	}
}

func TestResolveWorkspaceNameDerivationError(t *testing.T) {
	clearOrchestratorEnv(t)
	// basename("/") is degenerate, so name derivation must error when
	// neither --name nor a dir-local name is available.
	t.Chdir("/")
	if _, err := resolveWorkspaceNameDefault(); err == nil {
		t.Error("name derivation from / should error")
	}
}

func TestGlobalsResolveDefaultOrchestrator(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	out, _, err := runDaft(t, "globals", "resolve", "--config-dir", t.TempDir())
	if err != nil {
		t.Fatalf("globals resolve: %v", err)
	}
	if !strings.Contains(out, defaultOrchestratorURL) || !strings.Contains(out, string(sourceDefault)) {
		t.Errorf("resolve should report built-in default:\n%s", out)
	}
}

func TestWorkspaceGetUnknownKey(t *testing.T) {
	chdirClean(t)
	if _, _, err := runDaft(t, "workspace", "get", "bogus"); err == nil {
		t.Error("workspace get bogus err = nil, want unknown-key error")
	}
}
