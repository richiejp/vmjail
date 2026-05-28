package kconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := `
# comment line
CONFIG_A=y
CONFIG_B=m
# CONFIG_C is not set
CONFIG_NR=16
CONFIG_NAME="hello"

# CONFIG_D is not set
`
	got, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{
		"CONFIG_A":    "y",
		"CONFIG_B":    "m",
		"CONFIG_C":    "n",
		"CONFIG_NR":   "16",
		"CONFIG_NAME": "hello",
		"CONFIG_D":    "n",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d symbols, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	// Final config: CFI was demoted to "not set", NR differs, KVM absent.
	final := writeFile(t, dir, ".config", strings.Join([]string{
		"CONFIG_TUN=y",
		"# CONFIG_CFI is not set",
		"CONFIG_NR=8",
		"CONFIG_IO_URING=y",
	}, "\n"))
	base := writeFile(t, dir, "base.config", strings.Join([]string{
		"CONFIG_TUN=y",
		"CONFIG_NR=16",
		"# CONFIG_IO_URING is not set",
	}, "\n"))
	hard := writeFile(t, dir, "hard.config", strings.Join([]string{
		"CONFIG_CFI=y",
		"CONFIG_KVM=y",
		"CONFIG_NR=4", // later fragment wins over base's 16
	}, "\n"))

	ms, err := Verify(final, []string{base, hard})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	byName := map[string]Mismatch{}
	for _, m := range ms {
		byName[m.Symbol] = m
	}
	// CFI requested y, demoted to n.
	if m, ok := byName["CONFIG_CFI"]; !ok || m.Want != "y" || m.Got != "n" {
		t.Errorf("CONFIG_CFI mismatch = %+v (ok=%v), want want=y got=n", byName["CONFIG_CFI"], ok)
	}
	// KVM requested y, absent from final -> n.
	if m, ok := byName["CONFIG_KVM"]; !ok || m.Got != "n" {
		t.Errorf("CONFIG_KVM mismatch = %+v (ok=%v), want got=n", byName["CONFIG_KVM"], ok)
	}
	// NR: later fragment requested 4, final has 8.
	if m, ok := byName["CONFIG_NR"]; !ok || m.Want != "4" || m.Got != "8" {
		t.Errorf("CONFIG_NR mismatch = %+v (ok=%v), want want=4 got=8", byName["CONFIG_NR"], ok)
	}
	// IO_URING requested n (unset), final has y -> mismatch.
	if m, ok := byName["CONFIG_IO_URING"]; !ok || m.Want != "n" || m.Got != "y" {
		t.Errorf("CONFIG_IO_URING mismatch = %+v (ok=%v), want want=n got=y", byName["CONFIG_IO_URING"], ok)
	}
	// TUN matched -> must NOT be a mismatch.
	if _, ok := byName["CONFIG_TUN"]; ok {
		t.Errorf("CONFIG_TUN should match, but reported as mismatch")
	}
	// Exactly the four mismatches above.
	if len(ms) != 4 {
		t.Errorf("got %d mismatches, want 4: %v", len(ms), ms)
	}
	// String form is non-empty and names the symbol.
	if s := ms[0].String(); !strings.Contains(s, ms[0].Symbol) {
		t.Errorf("Mismatch.String() = %q, want it to contain %q", s, ms[0].Symbol)
	}
}

func TestVerifyAllPresent(t *testing.T) {
	dir := t.TempDir()
	final := writeFile(t, dir, ".config", "CONFIG_A=y\nCONFIG_B=m\n# CONFIG_C is not set\n")
	frag := writeFile(t, dir, "f.config", "CONFIG_A=y\n# CONFIG_C is not set\n")
	ms, err := Verify(final, []string{frag})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(ms) != 0 {
		t.Fatalf("want no mismatches, got %v", ms)
	}
}

func TestVerifyMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	final := writeFile(t, dir, ".config", "CONFIG_A=y\n")
	if _, err := Verify(filepath.Join(dir, "nope.config"), []string{final}); err == nil {
		t.Error("expected error for missing config file")
	}
	if _, err := Verify(final, []string{filepath.Join(dir, "nope.config")}); err == nil {
		t.Error("expected error for missing fragment file")
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, ".config", "CONFIG_A=y\n# CONFIG_B is not set\n")
	okFrag := writeFile(t, dir, "ok.config", "CONFIG_A=y\n")
	badFrag := writeFile(t, dir, "bad.config", "CONFIG_A=y\nCONFIG_B=y\n") // B demoted

	const prog = "kconfig-verify"
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string // substring expected on stderr
	}{
		{"usage", []string{prog, "only-one"}, 2, "usage:"},
		{"ok", []string{prog, cfg, okFrag}, 0, ""},
		{"mismatch", []string{prog, cfg, badFrag}, 1, "CONFIG_B"},
		{"bad path", []string{prog, filepath.Join(dir, "nope"), okFrag}, 2, "read config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := Run(tc.args, &buf)
			if code != tc.wantCode {
				t.Errorf("Run code = %d, want %d (stderr: %q)", code, tc.wantCode, buf.String())
			}
			if tc.wantErr != "" && !strings.Contains(buf.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want substring %q", buf.String(), tc.wantErr)
			}
		})
	}
}
