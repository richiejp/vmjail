package orchestratorcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// runAuth executes a `daft-orchestrator auth …` invocation and returns
// stdout. Fatals on unexpected error (wantErr controls that).
func runAuth(t *testing.T, wantErr bool, args ...string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	err := Execute(append([]string{authCmdName}, args...), &out, &errBuf)
	if wantErr && err == nil {
		t.Fatalf("auth %v: expected error, got none (out=%q)", args, out.String())
	}
	if !wantErr && err != nil {
		t.Fatalf("auth %v: %v (err=%q)", args, err, errBuf.String())
	}
	return out.String()
}

// showFingerprint runs `auth show` against dir and returns the generated
// identity's fingerprint, mirroring how the dev cluster captures it.
func showFingerprint(t *testing.T, dir string) string {
	t.Helper()
	out := runAuth(t, false, "show", configDirFlag, dir)
	for _, line := range strings.Split(out, "\n") {
		if fp, ok := strings.CutPrefix(line, "fingerprint: "); ok {
			return strings.TrimSpace(fp)
		}
	}
	t.Fatalf("show output missing fingerprint line: %q", out)
	return ""
}

// TestAuthShowGeneratesOrchestratorIdentity is the core CN fix: `show`
// must mint an *orchestrator*-role identity, not a client one.
func TestAuthShowGeneratesOrchestratorIdentity(t *testing.T) {
	dir := t.TempDir()
	first := showFingerprint(t, dir)
	if !strings.HasPrefix(first, "sha256:") {
		t.Errorf("fingerprint = %q, want sha256: prefix", first)
	}
	// Stable across calls — second show loads, not regenerates.
	if second := showFingerprint(t, dir); second != first {
		t.Errorf("fingerprint changed across calls: %q vs %q", first, second)
	}
	// The on-disk cert carries the orchestrator role in its CN.
	id, err := loadOrGenerate(dir)
	if err != nil {
		t.Fatalf("loadOrGenerate: %v", err)
	}
	if cn := id.Cert.Subject.CommonName; !strings.Contains(cn, string(identity.RoleOrchestrator)) {
		t.Errorf("cert CN = %q, want it to contain %q", cn, identity.RoleOrchestrator)
	}
}

func TestAuthApproveListRevokeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	peerFP := showFingerprint(t, t.TempDir()) // a different node's fingerprint

	// Empty list first.
	if out := runAuth(t, false, listSubName, configDirFlag, dir); !strings.Contains(out, "no peers approved") {
		t.Errorf("empty list = %q", out)
	}

	runAuth(t, false, "approve", peerFP, configDirFlag, dir, "--role", "sandbox-host", "--label", "sbx")

	out := runAuth(t, false, listSubName, configDirFlag, dir)
	if !strings.Contains(out, peerFP) || !strings.Contains(out, "sandbox-host") || !strings.Contains(out, "sbx") {
		t.Errorf("list after approve = %q, want fp+role+label", out)
	}

	runAuth(t, false, "revoke", peerFP, configDirFlag, dir)
	if out := runAuth(t, false, listSubName, configDirFlag, dir); strings.Contains(out, peerFP) {
		t.Errorf("list after revoke still shows peer: %q", out)
	}
}

func TestAuthApproveRejectsBadFingerprint(t *testing.T) {
	runAuth(t, true, "approve", "not-a-fingerprint", configDirFlag, t.TempDir())
}

func TestAuthApproveRejectsUnknownRole(t *testing.T) {
	fp := showFingerprint(t, t.TempDir())
	runAuth(t, true, "approve", fp, configDirFlag, t.TempDir(), "--role", "bogus")
}

func TestAuthRevokeRejectsBadFingerprint(t *testing.T) {
	runAuth(t, true, "revoke", "nope", configDirFlag, t.TempDir())
}

func TestAuthRemoveVerbIsRejected(t *testing.T) {
	runAuth(t, true, "remove", "unused")
}

// TestAuthShowReportsCorruptIdentity covers show's loadOrGenerate error
// branch: a config dir holding an unparseable key.pem can't be loaded.
func TestAuthShowReportsCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"key.pem", "cert.pem"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("not a pem"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	runAuth(t, true, "show", configDirFlag, dir)
}

// TestAuthSubcommandsPropagateConfigDirError drives every subcommand
// with no --config-dir and an unresolvable default (empty HOME/XDG), so
// authConfigDir → resolveConfigDir fails and each subcommand surfaces the
// error rather than touching disk.
func TestAuthSubcommandsPropagateConfigDirError(t *testing.T) {
	// A valid fingerprint so approve/revoke get past the arg parse and
	// reach config-dir resolution. Generated before clearing HOME (it's
	// in-memory, but order keeps intent clear).
	gen, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	fp := gen.Fingerprint().String()

	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, args := range [][]string{
		{"show"},
		{listSubName},
		{"approve", fp},
		{"revoke", fp},
	} {
		runAuth(t, true, args...)
	}
}
