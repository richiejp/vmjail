package agentprofile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `{
  "version": 1,
  "profiles": [{
    "name": "codex-large",
    "provider": "codex",
    "image": "ghcr.io/example/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "command": ["codex", "exec"],
    "credentials": {"enable": true, "profile": "primary"},
    "resources": {"enable": true, "cpu_cores": 4, "memory_bytes": 8589934592},
    "egress": {"enable": true}
  }]
}`

func TestDecodeReturnsImmutableSortedCatalogAndStableDigest(t *testing.T) {
	catalog, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := catalog.Get("codex-large")
	if !ok || !strings.HasPrefix(entry.Digest, "sha256:") || len(entry.Digest) != 71 {
		t.Fatalf("entry = %#v", entry)
	}
	entry.Profile.Command[0] = "mutated"
	again, _ := catalog.Get("codex-large")
	if again.Profile.Command[0] != "codex" || again.Digest != entry.Digest {
		t.Fatalf("catalog was mutated: %#v", again)
	}
	if got := catalog.List(); len(got) != 1 || got[0].Profile.Name != "codex-large" {
		t.Fatalf("list = %#v", got)
	}
}

func TestDecodeRejectsInvalidAndSecretBearingProfiles(t *testing.T) {
	tests := map[string]string{
		"unknown field":                  strings.Replace(validConfig, `"egress":`, `"environment": {}, "egress":`, 1),
		"secret field":                   strings.Replace(validConfig, `"image":`, `"OPENAI_API_KEY": "leak", "image":`, 1),
		"duplicate":                      `{"version":1,"profiles":[` + profileJSON("same") + `,` + profileJSON("same") + `]}`,
		"bad provider":                   strings.Replace(validConfig, `"codex"`, `"claude"`, 1),
		"disabled credentials with name": strings.Replace(validConfig, `"enable": true, "profile": "primary"`, `"enable": false, "profile": "primary"`, 1),
		"disabled resources with values": strings.Replace(validConfig, `"enable": true, "cpu_cores": 4`, `"enable": false, "cpu_cores": 4`, 1),
		"partial disabled resources":     strings.Replace(validConfig, `"enable": true, "cpu_cores": 4, "memory_bytes": 8589934592`, `"enable": false, "cpu_cores": 0, "memory_bytes": 1`, 1),
		"trailing JSON":                  validConfig + `{}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
}

func profileJSON(name string) string {
	return `{"name":"` + name + `","provider":"codex","image":"example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":["codex"],"credentials":{"enable":false},"resources":{"enable":false},"egress":{"enable":false}}`
}

func TestDecodeLimitsAndLoadErrors(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
	if _, err := Decode(strings.NewReader(strings.Repeat(" ", MaxFileBytes+1))); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := Load(t.TempDir() + "/missing"); err == nil {
		t.Fatal("missing file accepted")
	}
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if catalog, err := Load(path); err != nil || len(catalog.List()) != 1 {
		t.Fatalf("Load() = %#v, %v", catalog, err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("invalid loaded file accepted")
	}
	if _, err := Decode(errorReader{}); err == nil {
		t.Fatal("reader failure accepted")
	}
	if got := (*Catalog)(nil).List(); got != nil {
		t.Fatalf("nil list = %#v", got)
	}
	if _, ok := (*Catalog)(nil).Get("x"); ok {
		t.Fatal("nil catalog lookup succeeded")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestDecodeRejectsContractBoundaries(t *testing.T) {
	tests := []string{
		`{}`,
		`{"version":2,"profiles":[]}`,
		`{"version":1,"profiles":[]}`,
		`{"version":1,"profiles":[` + profileJSON(" moving") + `]}`,
		strings.Replace(validConfig, `@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, `:latest`, 1),
		strings.Replace(validConfig, `"ghcr.io/example/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `""`, 1),
		strings.Replace(validConfig, `["codex", "exec"]`, `[]`, 1),
		strings.Replace(validConfig, `["codex", "exec"]`, `["codex", ""]`, 1),
		strings.Replace(validConfig, `"primary"`, `" primary"`, 1),
		strings.Replace(validConfig, `"cpu_cores": 4`, `"cpu_cores": 1025`, 1),
		strings.Replace(validConfig, `"memory_bytes": 8589934592`, `"memory_bytes": 17592186044417`, 1),
		validConfig + ` !`,
	}
	for i, input := range tests {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestDecodeRejectsTooManyProfiles(t *testing.T) {
	profiles := make([]string, MaxProfiles+1)
	for i := range profiles {
		profiles[i] = profileJSON(string(rune('a'+i%26)) + strings.Repeat("x", i/26))
	}
	input := `{"version":1,"profiles":[` + strings.Join(profiles, ",") + `]}`
	if _, err := Decode(strings.NewReader(input)); err == nil {
		t.Fatal("too many profiles accepted")
	}
}

func TestDecodeRejectsNestedCredentialFields(t *testing.T) {
	for _, field := range []string{"token", "access-token", "refresh_token", "api_key", "claude_code_oauth_token"} {
		input := `{"version":1,"profiles":[` + strings.TrimSuffix(profileJSON("safe"), `}`) + `,"` + field + `":[{"token":"secret"}]}]}`
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Errorf("field %q accepted", field)
		}
	}
	if forbiddenSecretField("ordinary") {
		t.Fatal("ordinary field classified as a secret")
	}
}
