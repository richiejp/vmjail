package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// projRemote is the derived in-VM folder reused across the register
// tests; hoisted so goconst stops flagging the repeated literal.
const projRemote = "/workspaces/proj"

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
	registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out, m)

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

	registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out,
		devcontainerWorkspaceMount{Local: "/p", Remote: "/workspaces/p"})

	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Errorf("dir-local mounts = %+v, want one even on AlreadyExists", mounts)
	}
}

// TestRegisterDevcontainerWorkspaceMountAddMountError: a non-AlreadyExists
// AddMount failure warns and records nothing dir-local.
func TestRegisterDevcontainerWorkspaceMountAddMountError(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeCatalog{addMountErr: connect.NewError(connect.CodeInternal, errors.New("boom"))}
	c := fakeCatalogClient(t, f)
	var out bytes.Buffer

	registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out,
		devcontainerWorkspaceMount{Local: "/p", Remote: "/workspaces/p"})

	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("dir-local mounts = %+v, want none on AddMount failure", mounts)
	}
	if !strings.Contains(out.String(), "register workspace mount") {
		t.Errorf("output %q should warn about the failure", out.String())
	}
}

// TestRegisterDevcontainerWorkspaceMountDirLocalError: AddMount succeeds
// but the dir-local write fails (here: ./.daft is a regular file, so the
// config path can't be created). The failure is warned, not fatal.
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

	registerDevcontainerWorkspaceMount(context.Background(), c, handleByName("w"), &out,
		devcontainerWorkspaceMount{Local: "/p", Remote: projRemote})

	if !strings.Contains(out.String(), "record workspace mount dir-local") {
		t.Errorf("output %q should warn about the dir-local failure", out.String())
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
		[]string{workspaceCmdName, devcontainerSubName, ".devcontainer/devcontainer.json"},
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

// TestWorkspaceDevcontainerAutoMountMissingFileWarns: when the host
// devcontainer.json isn't locally readable, setting the path still
// succeeds (the orchestrator reads it from pushed content) and the
// auto-mount is skipped with a warning rather than failing.
func TestWorkspaceDevcontainerAutoMountMissingFileWarns(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "dc-missing"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Chdir(t.TempDir()) // no devcontainer.json on disk
	_, stderr, err := runDaft(t, append(
		[]string{workspaceCmdName, devcontainerSubName, ".devcontainer/devcontainer.json"},
		commonArgs...,
	)...)
	if err != nil {
		t.Fatalf("devcontainer should still set the path: %v", err)
	}
	if !strings.Contains(stderr, "skip auto-mount") {
		t.Errorf("stderr %q should warn that auto-mount was skipped", stderr)
	}
	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatalf("dirLocalMounts: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("dir-local mounts = %+v, want none when the json is unreadable", mounts)
	}
}
