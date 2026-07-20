package agentcredential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFreezesRelativeCodexCredential(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":1,"credentials":[{"name":"personal","provider":"codex","source":{"file":"auth.json"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := catalog.Get("personal")
	if !ok || entry.Provider != ProviderCodex || entry.Secret.String() != "[REDACTED]" {
		t.Fatalf("unexpected entry: %#v %v", entry, ok)
	}
	if err := os.WriteFile(authPath, []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := entry.Secret.WithBytes(func(value []byte) error {
		if strings.Contains(string(value), "changed") || !strings.Contains(string(value), "secret") {
			t.Fatalf("credential was not frozen: %q", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	catalog.Destroy()
	if err := entry.Secret.WithBytes(func([]byte) error { return nil }); err == nil {
		t.Fatal("entry remained readable after catalog destruction")
	}
}

func TestLoadRejectsUnsafeCatalogsAndSources(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("auth.json", `{}`, 0o600)
	write("invalid", `not-json`, 0o600)
	tests := []struct{ name, cfg, want string }{
		{"unknown field", `{"version":1,"credentials":[],"extra":true}`, "unknown field"},
		{"trailing JSON", `{"version":1,"credentials":[]} {}`, "trailing JSON"},
		{"version", `{"version":2,"credentials":[]}`, "version must"},
		{"empty", `{"version":1,"credentials":[]}`, "must contain"},
		{"provider", `{"version":1,"credentials":[{"name":"x","provider":"claude","source":{"file":"auth.json"}}]}`, "unsupported provider"},
		{"empty name", `{"version":1,"credentials":[{"name":"","provider":"codex","source":{"file":"auth.json"}}]}`, "name must"},
		{"space name", `{"version":1,"credentials":[{"name":" x","provider":"codex","source":{"file":"auth.json"}}]}`, "name must"},
		{"empty source", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":""}}]}`, "source.file"},
		{"space source", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":" auth.json"}}]}`, "source.file"},
		{"duplicate", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"auth.json"}},{"name":"x","provider":"codex","source":{"file":"auth.json"}}]}`, "duplicate"},
		{"invalid auth", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"invalid"}}]}`, "JSON auth store"},
		{"missing auth", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"missing"}}]}`, "source: stat"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := write(strings.ReplaceAll(tc.name, " ", "_")+".json", tc.cfg, 0o600)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want %q", err, tc.want)
			}
		})
	}
	openAuth := write("open-auth.json", `{}`, 0o644)
	openCfg := write("open.json", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"`+openAuth+`"}}]}`, 0o600)
	if _, err := Load(openCfg); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("Load() error = %v, want permissions rejection", err)
	}
	directoryCfg := write("directory.json", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"."}}]}`, 0o600)
	if _, err := Load(directoryCfg); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Load() error = %v, want regular-file rejection", err)
	}
	emptyAuth := write("empty-auth.json", ``, 0o600)
	emptyCfg := write("empty-auth-cfg.json", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"`+emptyAuth+`"}}]}`, 0o600)
	if _, err := Load(emptyCfg); err == nil || !strings.Contains(err.Error(), "must contain") {
		t.Fatalf("Load() error = %v, want empty-source rejection", err)
	}
	largeAuth := write("large-auth.json", strings.Repeat("x", 1+(1<<20)), 0o600)
	largeCfg := write("large-auth-cfg.json", `{"version":1,"credentials":[{"name":"x","provider":"codex","source":{"file":"`+largeAuth+`"}}]}`, 0o600)
	if _, err := Load(largeCfg); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load() error = %v, want oversized-source rejection", err)
	}
}

func TestLoadRejectsOversizedCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load() error = %v, want size rejection", err)
	}
}

func TestLoadRejectsMissingAndNonRegularCatalog(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "missing"), dir} {
		if _, err := Load(path); err == nil {
			t.Fatalf("Load(%q) unexpectedly succeeded", path)
		}
	}
}

func TestCatalogNamesAndNilMethods(t *testing.T) {
	var nilCatalog *Catalog
	if _, ok := nilCatalog.Get("missing"); ok || nilCatalog.Names() != nil {
		t.Fatal("nil catalog returned data")
	}
	nilCatalog.Destroy()
	dir := t.TempDir()
	for _, name := range []string{"a.json", "z.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := `{"version":1,"credentials":[` +
		`{"name":"z","provider":"codex","source":{"file":"z.json"}},` +
		`{"name":"a","provider":"codex","source":{"file":"a.json"}}]}`
	cfgPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(catalog.Names(), ","); got != "a,z" {
		t.Fatalf("Names() = %q", got)
	}
	if _, ok := catalog.Get("missing"); ok {
		t.Fatal("Get returned missing credential")
	}
	catalog.Destroy()
}
