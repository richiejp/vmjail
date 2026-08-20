package sandboxdcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// runSandboxd is a small variant of the cli package's runDaft —
// drives daft-sandboxd's full cobra tree and returns captured
// stdout / stderr.
func runSandboxd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	err = Execute(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), err
}

func TestAuthShowPrintsFingerprintAndDir(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := runSandboxd(t, authCmdName, "show", "--config-dir", dir)
	if err != nil {
		t.Fatalf("auth show: %v", err)
	}
	if !strings.Contains(stdout, "fingerprint:") {
		t.Errorf("stdout missing fingerprint line: %q", stdout)
	}
	if !strings.Contains(stdout, dir) {
		t.Errorf("stdout missing config-dir %q: %q", dir, stdout)
	}
}

func TestAuthListEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	stdout, _, err := runSandboxd(t, authCmdName, listSubName, "--config-dir", dir)
	if err != nil {
		t.Fatalf("auth list: %v", err)
	}
	if !strings.Contains(stdout, "no peers approved") {
		t.Errorf("stdout = %q, want empty marker", stdout)
	}
}

func TestAuthApproveAndList(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	fp := other.Fingerprint().String()
	if _, _, err := runSandboxd(t, authCmdName, "approve", fp,
		"--config-dir", dir, "--label", "user-laptop", "--role", "client"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	stdout, _, err := runSandboxd(t, authCmdName, listSubName, "--config-dir", dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, fp) {
		t.Errorf("list missing fp %q: %q", fp, stdout)
	}
	if !strings.Contains(stdout, "user-laptop") {
		t.Errorf("list missing label: %q", stdout)
	}
}

func TestAuthApproveRejectsBadFingerprint(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runSandboxd(t, authCmdName, "approve", "not-a-fingerprint", "--config-dir", dir)
	if err == nil {
		t.Fatal("expected error for invalid fingerprint")
	}
}

func TestAuthApproveRejectsBadRole(t *testing.T) {
	dir := t.TempDir()
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _, err = runSandboxd(t, authCmdName, "approve", other.Fingerprint().String(),
		"--config-dir", dir, "--role", "not-a-role")
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestAuthRevokeAfterApprove(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	fp := other.Fingerprint().String()
	if _, _, err := runSandboxd(t, authCmdName, "approve", fp, "--config-dir", dir); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, _, err := runSandboxd(t, authCmdName, "revoke", fp, "--config-dir", dir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	stdout, _, err := runSandboxd(t, authCmdName, listSubName, "--config-dir", dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "no peers approved") {
		t.Errorf("expected empty after revoke, got %q", stdout)
	}
}

func TestAuthRevokeRejectsBadFingerprint(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runSandboxd(t, authCmdName, "revoke", "not-a-fingerprint", "--config-dir", dir)
	if err == nil {
		t.Fatal("expected error for invalid fingerprint")
	}
}

func TestAuthRemoveVerbIsRejected(t *testing.T) {
	if _, _, err := runSandboxd(t, authCmdName, "remove", "unused"); err == nil {
		t.Fatal("legacy auth remove command unexpectedly succeeded")
	}
}

func TestAuthShowPropagatesIdentityLoadError(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(t, filepath.Join(dir, "cert.pem"), "garbage"); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := writeFile(t, filepath.Join(dir, "key.pem"), "garbage"); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, _, err := runSandboxd(t, authCmdName, "show", "--config-dir", dir)
	if err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

func writeFile(t *testing.T, path, contents string) error {
	t.Helper()
	return os.WriteFile(path, []byte(contents), 0o600)
}

// TestAuthListPropagatesPeerStoreError covers the list-side
// corrupt-peer-store branch.
func TestAuthListPropagatesPeerStoreError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := writeFile(t, filepath.Join(dir, peersFilename), "not json"); err != nil {
		t.Fatalf("write peers: %v", err)
	}
	_, _, err := runSandboxd(t, authCmdName, listSubName, "--config-dir", dir)
	if err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}

func TestAuthApprovePropagatesPeerStoreError(t *testing.T) {
	assertCorruptPeerStoreFailsCmd(t, "approve")
}

func TestAuthRevokePropagatesPeerStoreError(t *testing.T) {
	assertCorruptPeerStoreFailsCmd(t, "revoke")
}

// assertCorruptPeerStoreFailsCmd plants a corrupt peers.json and
// drives `daft-sandboxd auth <verb> <fingerprint>` against it,
// asserting the verb propagates the parse error rather than
// silently overwriting a corrupt store.
func assertCorruptPeerStoreFailsCmd(t *testing.T, verb string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := writeFile(t, filepath.Join(dir, peersFilename), "not json"); err != nil {
		t.Fatalf("write peers: %v", err)
	}
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _, err = runSandboxd(t, authCmdName, verb, other.Fingerprint().String(),
		"--config-dir", dir)
	if err == nil {
		t.Fatalf("expected error for corrupt peer store on %s", verb)
	}
}

// TestAuthShowConfigDirError exercises the config-dir resolution
// failure shared across all four auth subcommands.
func TestAuthShowConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	_, _, err := runSandboxd(t, authCmdName, "show")
	if err == nil {
		t.Fatal("expected error when UserConfigDir is unresolvable")
	}
}

func TestAuthListConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	_, _, err := runSandboxd(t, authCmdName, listSubName)
	if err == nil {
		t.Fatal("expected error when UserConfigDir is unresolvable")
	}
}

func TestAuthApproveConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _, err = runSandboxd(t, authCmdName, "approve", other.Fingerprint().String())
	if err == nil {
		t.Fatal("expected error when UserConfigDir is unresolvable")
	}
}

// TestAuthListUnlabeledPeer covers the label==""/<unlabeled> and
// role=="" / <no-role> render branches in newAuthListCmd by approving
// a peer without --label or --role.
func TestAuthListUnlabeledPeer(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := runSandboxd(t, authCmdName, "approve", other.Fingerprint().String(),
		"--config-dir", dir); err != nil {
		t.Fatalf("approve: %v", err)
	}
	stdout, _, err := runSandboxd(t, authCmdName, listSubName, "--config-dir", dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "<unlabeled>") {
		t.Errorf("list missing <unlabeled> render: %q", stdout)
	}
	if !strings.Contains(stdout, "<no-role>") {
		t.Errorf("list missing <no-role> render: %q", stdout)
	}
}

func TestAuthRevokeConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	other, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, _, err = runSandboxd(t, authCmdName, "revoke", other.Fingerprint().String())
	if err == nil {
		t.Fatal("expected error when UserConfigDir is unresolvable")
	}
}
