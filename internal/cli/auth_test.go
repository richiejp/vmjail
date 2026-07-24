package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/identity"
)

const configDirArg = "--config-dir"

func runDaft(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	err = Execute(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), err
}

func TestResolveClientDirHonoursOverride(t *testing.T) {
	got, err := resolveClientDir("/tmp/explicit")
	if err != nil {
		t.Fatalf("resolveClientDir: %v", err)
	}
	if got != "/tmp/explicit" {
		t.Errorf("got %q, want /tmp/explicit", got)
	}
}

func TestConfigDirFromFlagRequiresRegisteredFlag(t *testing.T) {
	if _, err := configDirFromFlag(&cobra.Command{}); err == nil {
		t.Fatal("configDirFromFlag accepted command without config-dir flag")
	}
}

func TestResolveClientDirFallsBackToUserConfig(t *testing.T) {
	got, err := resolveClientDir("")
	if err != nil {
		t.Fatalf("resolveClientDir: %v", err)
	}
	if !strings.HasSuffix(got, clientConfigSubdir) {
		t.Errorf("got %q, want suffix %q", got, clientConfigSubdir)
	}
}

func TestLoadOrGenerateClientFirstRunPersists(t *testing.T) {
	dir := t.TempDir()
	id1, err := loadOrGenerateClient(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	id2, err := loadOrGenerateClient(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Error("fingerprint changed across calls — second call regenerated")
	}
}

func TestLoadOrGenerateClientRejectsCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cert.pem"), "garbage")
	mustWrite(t, filepath.Join(dir, "key.pem"), "garbage")
	if _, err := loadOrGenerateClient(dir); err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

func TestAuthShowPrintsFingerprintAndDir(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := runDaft(t, authCmdName, configDirArg, dir, "show")
	if err != nil {
		t.Fatalf("auth show: %v", err)
	}
	if !strings.Contains(stdout, "fingerprint: sha256:") {
		t.Errorf("stdout missing fingerprint line: %q", stdout)
	}
	if !strings.Contains(stdout, "config-dir:  "+dir) {
		t.Errorf("stdout missing config-dir line: %q", stdout)
	}
}

func TestAuthListEmpty(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := runDaft(t, authCmdName, configDirArg, dir, listSubName)
	if err != nil {
		t.Fatalf("auth list: %v", err)
	}
	if !strings.Contains(stdout, "no peers approved") {
		t.Errorf("stdout = %q, want empty-list message", stdout)
	}
}

func TestAuthApproveAndListRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	fp := id.Fingerprint().String()

	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "approve",
		fp, "--label", "lab1", "--role", "orchestrator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	stdout, _, err := runDaft(t, authCmdName, configDirArg, dir, listSubName)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, fp) {
		t.Errorf("list stdout missing fingerprint: %q", stdout)
	}
	if !strings.Contains(stdout, "lab1") {
		t.Errorf("list stdout missing label: %q", stdout)
	}
	if !strings.Contains(stdout, "orchestrator") {
		t.Errorf("list stdout missing role: %q", stdout)
	}
}

func TestAuthListHandlesUnlabelledEntries(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	fp := id.Fingerprint().String()

	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "approve", fp); err != nil {
		t.Fatalf("approve: %v", err)
	}
	stdout, _, err := runDaft(t, authCmdName, configDirArg, dir, listSubName)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "<unlabeled>") {
		t.Errorf("stdout missing <unlabeled> placeholder: %q", stdout)
	}
	if !strings.Contains(stdout, "<no-role>") {
		t.Errorf("stdout missing <no-role> placeholder: %q", stdout)
	}
}

func TestAuthApproveRejectsBadFingerprint(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "approve", "not-a-fp"); err == nil {
		t.Fatal("expected error for malformed fingerprint")
	}
}

func TestAuthApproveRejectsUnknownRole(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	_, _, err := runDaft(t, authCmdName, configDirArg, dir, "approve",
		id.Fingerprint().String(), "--role", "zalgo")
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestAuthRemoveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	fp := id.Fingerprint().String()

	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "approve", fp); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "remove", fp); err != nil {
		t.Fatalf("remove: %v", err)
	}
	stdout, _, err := runDaft(t, authCmdName, configDirArg, dir, listSubName)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "no peers approved") {
		t.Errorf("expected empty list after remove, got %q", stdout)
	}
}

func TestAuthRemoveIsIdempotentForUnknown(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "remove",
		id.Fingerprint().String()); err != nil {
		t.Errorf("remove of unknown fp = %v, want nil", err)
	}
}

func TestAuthRemoveRejectsBadFingerprint(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "remove", "not-a-fp"); err == nil {
		t.Fatal("expected error for malformed fingerprint")
	}
}

// TestAuthSubcommandsPropagateIdentityLoadError exercises the
// loadOrGenerateClient error branch in `daft auth show`, which is
// the only auth subcommand that touches the identity (the others
// only need the peer store).
func TestAuthShowPropagatesIdentityLoadError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cert.pem"), "garbage")
	mustWrite(t, filepath.Join(dir, "key.pem"), "garbage")
	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "show"); err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// TestAuthSubcommandsPropagatePeerStoreError covers the
// openClientPeerStore error branch through every subcommand that
// uses it. A malformed peers.json fails OpenPeerStore.
func TestAuthSubcommandsPropagatePeerStoreError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, peersFilename), "not json")

	id, _ := identity.Generate(identity.RoleClient)
	fp := id.Fingerprint().String()
	cases := [][]string{
		{authCmdName, configDirArg, dir, listSubName},
		{authCmdName, configDirArg, dir, "approve", fp},
		{authCmdName, configDirArg, dir, "remove", fp},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[3:], "_"), func(t *testing.T) {
			if _, _, err := runDaft(t, args...); err == nil {
				t.Fatalf("%v: expected peer-store error", args)
			}
		})
	}
}

// TestResolveClientDirPropagatesUserConfigDirError forces
// os.UserConfigDir to fail by pointing every env var it consults
// at empty strings. On Unix this triggers the "neither $XDG_CONFIG_HOME
// nor $HOME is defined" error path.
func TestResolveClientDirPropagatesUserConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := resolveClientDir(""); err == nil {
		t.Fatal("expected error when no config dir is resolvable")
	}
}

// TestRootRejectsMissingConfigDirOnAuth is a regression test:
// passing --config-dir="" should fall through to the UserConfigDir
// path, not crash.
func TestRootRejectsMissingConfigDirOnAuth(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)
	stdout, _, err := runDaft(t, authCmdName, "show")
	if err != nil {
		t.Fatalf("auth show: %v", err)
	}
	if !strings.Contains(stdout, "fingerprint: sha256:") {
		t.Errorf("stdout missing fingerprint: %q", stdout)
	}
}

// failingWriter forces fmt.Fprintf's error branch.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errFail }

var errFail = errFailErr()

func errFailErr() error { return errStub{} }

type errStub struct{}

func (errStub) Error() string { return "write failed" }

func executeWith(args []string, stdout, stderr interface{ Write([]byte) (int, error) }) error {
	return Execute(args, stdout, stderr)
}

func TestAuthShowPropagatesStdoutWriteError(t *testing.T) {
	dir := t.TempDir()
	var errBuf bytes.Buffer
	if err := executeWith([]string{authCmdName, configDirArg, dir, "show"},
		failingWriter{}, &errBuf); err == nil {
		t.Fatal("expected error from failing stdout")
	}
}

func TestAuthListPropagatesStdoutWriteError(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	if _, _, err := runDaft(t, authCmdName, configDirArg, dir, "approve",
		id.Fingerprint().String()); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var errBuf bytes.Buffer
	if err := executeWith([]string{authCmdName, configDirArg, dir, listSubName},
		failingWriter{}, &errBuf); err == nil {
		t.Fatal("expected error from failing stdout on populated list")
	}
	if err := executeWith([]string{authCmdName, configDirArg, t.TempDir(), "list"},
		failingWriter{}, &errBuf); err == nil {
		t.Fatal("expected error from failing stdout on empty-list message")
	}
}

func TestAuthApprovePropagatesStdoutWriteError(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	var errBuf bytes.Buffer
	if err := executeWith([]string{
		"auth", configDirArg, dir, "approve",
		id.Fingerprint().String(),
	}, failingWriter{}, &errBuf); err == nil {
		t.Fatal("expected error from failing stdout on approve")
	}
}

func TestAuthRemovePropagatesStdoutWriteError(t *testing.T) {
	dir := t.TempDir()
	id, _ := identity.Generate(identity.RoleClient)
	var errBuf bytes.Buffer
	if err := executeWith([]string{
		"auth", configDirArg, dir, "remove",
		id.Fingerprint().String(),
	}, failingWriter{}, &errBuf); err == nil {
		t.Fatal("expected error from failing stdout on remove")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
