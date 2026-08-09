package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// projRemote is the derived in-VM folder reused across the register
// tests; hoisted so goconst stops flagging the repeated literal.
const projRemote = "/workspaces/proj"

// dcConventionPath is the primary Dev Containers convention location,
// hoisted so goconst stops flagging the repeated literal across tests.
const dcConventionPath = ".devcontainer/devcontainer.json"

// writeDevcontainerJSON drops a devcontainer.json at dir/<rel> and
// returns its host-relative path from a cwd of base.
func writeDevcontainerJSON(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, "devcontainer.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}
	return p
}

// TestDeriveDevcontainerWorkspaceMount covers the folder/host-root
// derivation precedence: explicit workspaceFolder, explicit
// workspaceMount target, then the /workspaces/<basename> default; and
// the .devcontainer/ vs flat-layout host-root rule.
func TestDeriveDevcontainerWorkspaceMount(t *testing.T) {
	root := t.TempDir()
	projectName := filepath.Base(root)

	cases := []struct {
		name       string
		dir        string // dir holding devcontainer.json
		body       string
		wantLocal  string
		wantRemote string
	}{
		{
			name:       "explicit workspaceFolder in .devcontainer dir",
			dir:        filepath.Join(root, ".devcontainer"),
			body:       `{"image":"x","workspaceFolder":"/code"}`,
			wantLocal:  root,
			wantRemote: "/code",
		},
		{
			name:       "default folder, .devcontainer parent is the root",
			dir:        filepath.Join(root, ".devcontainer"),
			body:       `{"image":"x"}`,
			wantLocal:  root,
			wantRemote: "/workspaces/" + projectName,
		},
		{
			name:       "explicit workspaceMount target wins over default",
			dir:        filepath.Join(root, ".devcontainer"),
			body:       `{"image":"x","workspaceMount":"source=${localWorkspaceFolder},target=/srv/app,type=bind"}`,
			wantLocal:  root,
			wantRemote: "/srv/app",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeDevcontainerJSON(t, tc.dir, tc.body)
			m, ok, err := deriveDevcontainerWorkspaceMount(p)
			if err != nil {
				t.Fatalf("derive: %v", err)
			}
			if !ok {
				t.Fatal("ok = false, want true")
			}
			if m.Local != tc.wantLocal {
				t.Errorf("Local = %q, want %q", m.Local, tc.wantLocal)
			}
			if m.Remote != tc.wantRemote {
				t.Errorf("Remote = %q, want %q", m.Remote, tc.wantRemote)
			}
		})
	}
}

// TestDeriveDevcontainerWorkspaceMountFlatLayout: a devcontainer.json
// NOT under a .devcontainer/ dir roots at its own directory.
func TestDeriveDevcontainerWorkspaceMountFlatLayout(t *testing.T) {
	dir := t.TempDir()
	p := writeDevcontainerJSON(t, dir, `{"image":"x"}`)
	m, ok, err := deriveDevcontainerWorkspaceMount(p)
	if err != nil || !ok {
		t.Fatalf("derive: ok=%v err=%v", ok, err)
	}
	if m.Local != dir {
		t.Errorf("Local = %q, want %q", m.Local, dir)
	}
	if want := "/workspaces/" + filepath.Base(dir); m.Remote != want {
		t.Errorf("Remote = %q, want %q", m.Remote, want)
	}
}

// TestDeriveDevcontainerWorkspaceMountEmpty: an empty path (clearing the
// devcontainer) derives nothing.
func TestDeriveDevcontainerWorkspaceMountEmpty(t *testing.T) {
	_, ok, err := deriveDevcontainerWorkspaceMount("")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Error("ok = true, want false for empty path")
	}
}

// TestDeriveDevcontainerWorkspaceMountErrors: an unreadable path and a
// malformed json both error (so the command can warn and skip).
func TestDeriveDevcontainerWorkspaceMountErrors(t *testing.T) {
	if _, _, err := deriveDevcontainerWorkspaceMount(
		filepath.Join(t.TempDir(), "missing", "devcontainer.json"),
	); err == nil {
		t.Error("missing file: want error")
	}
	bad := writeDevcontainerJSON(t, t.TempDir(), `{not json`)
	if _, _, err := deriveDevcontainerWorkspaceMount(bad); err == nil {
		t.Error("malformed json: want error")
	}
}

// TestRegisterDevcontainerWorkspaceMount: the happy path registers the
// catalog mount and records it dir-local.
func TestRegisterDevcontainerWorkspaceMount(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeCatalog{}
	c := fakeCatalogClient(t, f)
	var out bytes.Buffer

	m := devcontainerWorkspaceMount{Local: "/home/me/proj", Remote: projRemote}
	if err := registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out, m); err != nil {
		t.Fatalf("register: %v", err)
	}

	if len(f.addMountTargets) != 1 || f.addMountTargets[0] != projRemote {
		t.Errorf("AddMount targets = %v, want [%s]", f.addMountTargets, projRemote)
	}
	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Local != "/home/me/proj" || mounts[0].Remote != projRemote {
		t.Errorf("dir-local mounts = %+v, want one /home/me/proj -> /workspaces/proj", mounts)
	}
	if !strings.Contains(out.String(), "registered") {
		t.Errorf("output %q should confirm registration", out.String())
	}
}

// TestRegisterDevcontainerWorkspaceMountAlreadyExists: an AlreadyExists
// from AddMount is treated as success — the dir-local mapping is still
// recorded so push/pull works.
func TestRegisterDevcontainerWorkspaceMountAlreadyExists(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeCatalog{addMountErr: connect.NewError(connect.CodeAlreadyExists, errors.New("present"))}
	c := fakeCatalogClient(t, f)
	var out bytes.Buffer

	if err := registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out,
		devcontainerWorkspaceMount{Local: "/p", Remote: "/workspaces/p"}); err != nil {
		t.Fatalf("register existing: %v", err)
	}

	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Errorf("dir-local mounts = %+v, want one even on AlreadyExists", mounts)
	}
}

// TestRegisterDevcontainerWorkspaceMountAddMountError: a non-AlreadyExists
// AddMount failure is returned and records nothing dir-local.
func TestRegisterDevcontainerWorkspaceMountAddMountError(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeCatalog{addMountErr: connect.NewError(connect.CodeInternal, errors.New("boom"))}
	c := fakeCatalogClient(t, f)
	var out bytes.Buffer

	err := registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out,
		devcontainerWorkspaceMount{Local: "/p", Remote: "/workspaces/p"})
	if err == nil || !strings.Contains(err.Error(), "register workspace mount") {
		t.Fatalf("register error = %v", err)
	}

	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("dir-local mounts = %+v, want none on AddMount failure", mounts)
	}
}

// TestRegisterDevcontainerWorkspaceMountDirLocalError: AddMount succeeds
// but the dir-local write fails (here: ./.daft is a regular file, so the
// config path can't be created). The failure is returned and the newly added
// catalog mount is compensated.
func TestRegisterDevcontainerWorkspaceMountDirLocalError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// A file where the .daft config dir should be makes addDirLocalMount fail.
	if err := os.WriteFile(filepath.Join(dir, ".daft"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write .daft sentinel: %v", err)
	}
	f := &fakeCatalog{}
	c := fakeCatalogClient(t, f)
	var out bytes.Buffer

	err := registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out,
		devcontainerWorkspaceMount{Local: "/p", Remote: projRemote})
	if err == nil || !strings.Contains(err.Error(), "record workspace mount dir-local") {
		t.Fatalf("register error = %v", err)
	}
}

// TestWorkspaceDevcontainerAutoMount drives the real `daft workspace
// devcontainer` command against a live orchestrator and proves it
// auto-registers the implied workspace mount (catalog + dir-local), so
// no separate `daft workspace mount` is needed.
func TestWorkspaceDevcontainerAutoMount(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "dc-automount"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}

	projectDir := t.TempDir()
	t.Chdir(projectDir)
	writeDevcontainerJSON(t, filepath.Join(projectDir, ".devcontainer"),
		`{"image":"x","workspaceFolder":"/code"}`)

	out, _, err := runDaft(t, append(
		[]string{workspaceCmdName, devcontainerSubName, dcConventionPath},
		commonArgs...,
	)...)
	if err != nil {
		t.Fatalf("devcontainer: %v", err)
	}
	if !strings.Contains(out, "registered") {
		t.Errorf("output %q should confirm auto-mount registration", out)
	}
	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Local != projectDir || mounts[0].Remote != "/code" {
		t.Errorf("dir-local mounts = %+v, want one %s -> /code", mounts, projectDir)
	}
}

// TestWorkspaceDevcontainerAutoMountMissingFileWarns preserves the advanced
// case where content was registered from a different host source.
func TestWorkspaceDevcontainerAutoMountMissingFileWarns(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "dc-missing"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Chdir(t.TempDir()) // no devcontainer.json on disk
	_, stderr, err := runDaft(t, append(
		[]string{workspaceCmdName, devcontainerSubName, dcConventionPath},
		commonArgs...,
	)...)
	if err != nil {
		t.Fatalf("devcontainer should still set the server-side path: %v", err)
	}
	if !strings.Contains(stderr, "skip auto-mount") {
		t.Fatalf("stderr = %q, want skip auto-mount warning", stderr)
	}
	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("dir-local mounts = %+v, want none when the json is unreadable", mounts)
	}
}

func TestWorkspaceDevcontainerRejectsCorruptLocalConfigBeforeRPC(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".daft"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runDaft(t, workspaceCmdName, devcontainerSubName, "--clear")
	if err == nil {
		t.Fatal("devcontainer clear succeeded with corrupt local config")
	}
}

func TestWorkspaceDevcontainerRestoresLocalPathWhenCatalogMutationFails(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "dc-rollback"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}
	workspaces := rig.catalog.List(nil)
	if len(workspaces) != 1 {
		t.Fatalf("workspaces = %d, want 1", len(workspaces))
	}
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.catalog.Update(workspaces[0].Metadata.UUID, workspacecatalog.MutateStatus, func(ws *workspacecatalog.Workspace) error {
		ws.Status.Lock = workspacecatalog.Lock{OwnerFingerprint: other.Fingerprint(), ExpiresAt: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatalf("lock workspace: %v", err)
	}

	projectDir := t.TempDir()
	t.Chdir(projectDir)
	writeDevcontainerJSON(t, filepath.Join(projectDir, ".devcontainer"), `{"image":"x"}`)
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, devcontainerSubName, dcConventionPath}, commonArgs...)...); err == nil {
		t.Fatal("devcontainer unexpectedly mutated a workspace locked by another client")
	}
	got, err := rig.catalog.Get(workspaces[0].Metadata.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.DevcontainerPath != "" {
		t.Fatalf("server devcontainer path = %q, want rollback to empty", got.Spec.DevcontainerPath)
	}
	if localPath, err := dirLocalDevcontainer(); err != nil || localPath != "" {
		t.Fatalf("local devcontainer path = %q, %v; want rollback to empty", localPath, err)
	}
}

// TestAutodetectDevcontainerPath: the .devcontainer/ location wins over
// the root-level variant, the root variant is the fallback, and a project
// with neither errors with a message naming both searched paths.
func TestAutodetectDevcontainerPath(t *testing.T) {
	t.Run("prefers .devcontainer dir", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		writeDevcontainerJSON(t, filepath.Join(dir, devcontainerDirName), `{}`)
		if err := os.WriteFile(filepath.Join(dir, ".devcontainer.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write root variant: %v", err)
		}
		got, err := autodetectDevcontainerPath()
		if err != nil {
			t.Fatalf("autodetect: %v", err)
		}
		if got != dcConventionPath {
			t.Errorf("path = %q, want %q", got, dcConventionPath)
		}
	})

	t.Run("falls back to root variant", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.WriteFile(filepath.Join(dir, ".devcontainer.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write root variant: %v", err)
		}
		got, err := autodetectDevcontainerPath()
		if err != nil {
			t.Fatalf("autodetect: %v", err)
		}
		if got != ".devcontainer.json" {
			t.Errorf("path = %q, want .devcontainer.json", got)
		}
	})

	t.Run("none found errors", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := autodetectDevcontainerPath()
		if err == nil {
			t.Fatal("want error when no devcontainer.json exists")
		}
		if !strings.Contains(err.Error(), dcConventionPath) || !strings.Contains(err.Error(), ".devcontainer.json") {
			t.Errorf("error %q should name both searched paths", err)
		}
	})

	t.Run("a directory at the path is not matched", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		// A bare `.devcontainer.json` directory must not satisfy detection.
		if err := os.Mkdir(filepath.Join(dir, ".devcontainer.json"), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if _, err := autodetectDevcontainerPath(); err == nil {
			t.Error("a directory named .devcontainer.json should not count as a match")
		}
	})
}

// TestResolveDevcontainerArg covers the arg/flag → path mapping: explicit
// arg passthrough, --clear yielding "", --clear with an arg as a usage
// error, and the no-arg auto-detect + its not-found error.
func TestResolveDevcontainerArg(t *testing.T) {
	t.Run("explicit arg wins", func(t *testing.T) {
		got, err := resolveDevcontainerArg([]string{"custom/devcontainer.json"}, false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != "custom/devcontainer.json" {
			t.Errorf("path = %q, want custom/devcontainer.json", got)
		}
	})

	t.Run("clear yields empty", func(t *testing.T) {
		got, err := resolveDevcontainerArg(nil, true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != "" {
			t.Errorf("path = %q, want empty for --clear", got)
		}
	})

	t.Run("clear with arg errors", func(t *testing.T) {
		if _, err := resolveDevcontainerArg([]string{"x"}, true); err == nil {
			t.Error("--clear with a path arg should error")
		}
	})

	t.Run("no arg auto-detects", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		writeDevcontainerJSON(t, filepath.Join(dir, devcontainerDirName), `{}`)
		got, err := resolveDevcontainerArg(nil, false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != dcConventionPath {
			t.Errorf("path = %q, want %q", got, dcConventionPath)
		}
	})

	t.Run("no arg, nothing to detect, errors", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if _, err := resolveDevcontainerArg(nil, false); err == nil {
			t.Error("no arg with no devcontainer.json should error")
		}
	})
}
