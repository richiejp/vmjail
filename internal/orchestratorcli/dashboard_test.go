package orchestratorcli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestDashboardPasswordHashCommand(t *testing.T) {
	cmd := New(&bytes.Buffer{}, &bytes.Buffer{})
	output := &bytes.Buffer{}
	cmd.SetOut(output)
	cmd.SetIn(bytes.NewBufferString("secret\n"))
	cmd.SetArgs([]string{dashboardCmdName, passwordHashCmdName})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if bcrypt.CompareHashAndPassword(bytes.TrimSpace(output.Bytes()), []byte("secret")) != nil {
		t.Fatalf("output is not matching bcrypt hash: %q", output.String())
	}

	for _, password := range []string{"", string(bytes.Repeat([]byte("x"), maxDashboardPasswordBytes+1))} {
		cmd := New(&bytes.Buffer{}, &bytes.Buffer{})
		cmd.SetIn(bytes.NewBufferString(password))
		cmd.SetArgs([]string{dashboardCmdName, passwordHashCmdName})
		if err := cmd.Execute(); err == nil {
			t.Fatalf("password length %d: expected error", len(password))
		}
	}
}

func TestReadDashboardPasswordHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard-password")
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := os.WriteFile(path, append(hash, '\n'), 0o600); err != nil {
		t.Fatalf("write hash: %v", err)
	}
	got, err := readDashboardPasswordHash(path)
	if err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if bcrypt.CompareHashAndPassword(got, []byte("secret")) != nil {
		t.Fatal("loaded hash does not match")
	}
	if err := os.WriteFile(path, []byte("plaintext"), 0o600); err != nil {
		t.Fatalf("write invalid hash: %v", err)
	}
	if _, err := readDashboardPasswordHash(path); err == nil {
		t.Fatal("expected invalid-hash error")
	}
	if _, err := readDashboardPasswordHash(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected missing-file error")
	}
	if hash, err := optionalDashboardPasswordHash(""); err != nil || hash != nil {
		t.Fatalf("empty optional hash = %q, %v", hash, err)
	}
}

func TestOptionalDashboardWebAuthnConfig(t *testing.T) {
	dir := t.TempDir()
	config, err := optionalDashboardWebAuthnConfig("example.com", []string{"https://example.com"}, dir)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if config.RPID != "example.com" || len(config.RPOrigins) != 1 || config.StatePath != filepath.Join(dir, "state", "dashboard-webauthn.json") {
		t.Fatalf("config = %#v", config)
	}
	if config, err := optionalDashboardWebAuthnConfig("", nil, dir); err != nil || config != nil {
		t.Fatalf("disabled config = %#v, %v", config, err)
	}
	if _, err := optionalDashboardWebAuthnConfig("", []string{"https://example.com"}, dir); err == nil {
		t.Fatal("expected origins-without-RP-ID error")
	}
	if _, err := optionalDashboardWebAuthnConfig("example.com", nil, dir); err == nil {
		t.Fatal("expected RP-ID-without-origin error")
	}
}
