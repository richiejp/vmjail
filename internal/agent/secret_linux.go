//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/richiejp/darkaifactory/internal/agentcredential"
)

const credentialFSType = "tmpfs"

func defaultSecretDeps() secretDeps {
	return secretDeps{listenVsock: vsockListenGuest, prepare: prepareLinuxCredentialStore}
}

func prepareLinuxCredentialStore(proc containerProcess) (credentialStore, error) {
	return prepareLinuxCredentialStoreAt(proc, agentcredential.AgentHomeRoot, agentcredential.CodexHome, unix.Mount, unix.Unmount)
}

func prepareLinuxCredentialStoreAt(proc containerProcess, mountRoot, codexHome string, mount func(string, string, string, uintptr, string) error, unmount func(string, int) error) (credentialStore, error) {
	uid, gid, err := parseContainerUser(proc.User)
	if err != nil {
		return nil, err
	}
	// #nosec G301 -- execute-only traversal is required for a non-root OCI user;
	// contents remain hidden by the child directory's 0700 mode.
	if err := os.MkdirAll(mountRoot, 0o711); err != nil {
		return nil, err
	}
	if err := mount(credentialFSType, mountRoot, credentialFSType, unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0711,size=2m,nr_inodes=32"); err != nil {
		return nil, fmt.Errorf("mount credential tmpfs: %w", err)
	}
	cleanup := func() error {
		auth := filepath.Join(codexHome, "auth.json")
		if raw, readErr := os.ReadFile(auth); readErr == nil { // #nosec G304 -- auth is a fixed internal path.
			clear(raw)
			if file, openErr := os.OpenFile(auth, os.O_WRONLY|os.O_TRUNC, 0); openErr == nil { // #nosec G304 -- fixed internal path.
				_ = file.Close()
			}
		}
		_ = os.Remove(auth)
		err := unmount(mountRoot, 0)
		if err == nil {
			_ = os.RemoveAll(mountRoot)
		}
		return err
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		_ = cleanup()
		return nil, err
	}
	if err := os.Chown(codexHome, uid, gid); err != nil {
		_ = cleanup()
		return nil, err
	}
	store := &secretReplayStore{now: time.Now, cleanup: cleanup}
	store.install = func(payload []byte) error { return atomicCredentialWriteAt(codexHome, payload, uid, gid) }
	return store, nil
}

func parseContainerUser(user string) (uid, gid int, err error) {
	if user == "" {
		return 0, 0, nil
	}
	parts := strings.Split(user, ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("invalid container user")
	}
	uid, err = strconv.Atoi(parts[0])
	if err != nil || uid < 0 {
		return 0, 0, errors.New("invalid container uid")
	}
	gid, err = strconv.Atoi(parts[1])
	if err != nil || gid < 0 {
		return 0, 0, errors.New("invalid container gid")
	}
	return uid, gid, nil
}

func atomicCredentialWriteAt(home string, payload []byte, uid, gid int) error {
	tmp := filepath.Join(home, ".auth.json.tmp")
	dst := filepath.Join(home, "auth.json")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600) // #nosec G304 -- tmp is under the fixed internal Codex home.
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	return nil
}
