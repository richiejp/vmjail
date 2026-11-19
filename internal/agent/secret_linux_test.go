//go:build linux

package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
)

func TestPrepareLinuxCredentialStoreInstallsAndCleansTmpfs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	home := filepath.Join(root, "codex")
	mounted, unmounted := false, false
	mount := func(source, target, fsType string, flags uintptr, data string) error {
		mounted = source == credentialFSType && target == root && fsType == credentialFSType && flags != 0 && data != ""
		return nil
	}
	unmount := func(target string, flags int) error { unmounted = target == root && flags == 0; return nil }
	user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	store, err := prepareLinuxCredentialStoreAt(containerProcess{User: user}, root, home, mount, unmount)
	if err != nil {
		t.Fatal(err)
	}
	if !mounted {
		t.Fatal("tmpfs mount was not requested")
	}
	req := &agentv1.SecretInstallRequest{DeliveryId: "id", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Kind: agentv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: []byte(`{"token":"secret"}`)}
	if err := store.Install(req); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, "auth.json")
	got, err := os.ReadFile(auth) // #nosec G304 -- test-owned temporary path.
	if err != nil || !bytes.Equal(got, req.Payload) {
		t.Fatalf("auth=%q err=%v", got, err)
	}
	info, err := os.Stat(auth)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
	if err := store.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if !unmounted {
		t.Fatal("tmpfs was not unmounted")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root remains: %v", err)
	}
}

func TestLinuxCredentialStoreRejectsSetupAndWriteFailures(t *testing.T) {
	for _, user := range []string{"bad", "x:1", "1:x", "-1:0", "0:-1"} {
		if _, _, err := parseContainerUser(user); err == nil {
			t.Fatalf("parse %q succeeded", user)
		}
	}
	if uid, gid, err := parseContainerUser("12:34"); err != nil || uid != 12 || gid != 34 {
		t.Fatalf("parse=%d:%d %v", uid, gid, err)
	}
	want := errors.New("mount")
	root := filepath.Join(t.TempDir(), "root")
	if _, err := prepareLinuxCredentialStoreAt(containerProcess{}, root, filepath.Join(root, "codex"), func(string, string, string, uintptr, string) error { return want }, func(string, int) error { return nil }); !errors.Is(err, want) {
		t.Fatalf("mount error=%v", err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".auth.json.tmp"), []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicCredentialWriteAt(home, []byte(`{}`), os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("existing temporary file was overwritten")
	}
	root2 := filepath.Join(t.TempDir(), "root")
	home2 := filepath.Join(root2, "codex")
	store, err := prepareLinuxCredentialStoreAt(containerProcess{User: strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}, root2, home2, func(string, string, string, uintptr, string) error { return nil }, func(string, int) error { return errors.New("unmount") })
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(); err == nil {
		t.Fatal("unmount failure was lost")
	}
	root3 := filepath.Join(t.TempDir(), "root")
	if _, err := prepareLinuxCredentialStoreAt(containerProcess{}, root3, filepath.Join(root3, "codex"), func(string, string, string, uintptr, string) error { return nil }, func(string, int) error { return nil }); err == nil {
		t.Fatal("chown failure was not returned")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLinuxCredentialStoreAt(containerProcess{}, filepath.Join(blocked, "child"), filepath.Join(blocked, "child", "codex"), func(string, string, string, uintptr, string) error { return nil }, func(string, int) error { return nil }); err == nil {
		t.Fatal("mkdir failure was not returned")
	}
	root4 := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root4, 0o700); err != nil {
		t.Fatal(err)
	}
	homeFile := filepath.Join(root4, "codex")
	if err := os.WriteFile(homeFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLinuxCredentialStoreAt(containerProcess{}, root4, homeFile, func(string, string, string, uintptr, string) error { return nil }, func(string, int) error { return nil }); err == nil {
		t.Fatal("home mkdir failure was not returned")
	}
}
