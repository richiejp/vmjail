// Package agentcredential loads provider-native coding-agent credentials on
// the trusted MCP host. Credential bytes leave this package only through the
// explicitly redacted secretvalue callback boundary.
package agentcredential

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/richiejp/vmjail/internal/secretvalue"
)

const (
	FileVersion  = 1
	MaxFileBytes = 1 << 20
	MaxEntries   = 128
	MaxNameBytes = 128
	// CodexHome is the fixed tmpfs-backed directory exposed to sandboxed
	// Codex processes. Callers cannot override this path or its env name.
	CodexHome     = "/run/vmjail/agent-home/codex"
	AgentHomeRoot = "/run/vmjail/agent-home"
)

type Provider string

const ProviderCodex Provider = "codex"

type Source struct {
	File string `json:"file"`
}
type configEntry struct {
	Name     string   `json:"name"`
	Provider Provider `json:"provider"`
	Source   Source   `json:"source"`
}
type configFile struct {
	Version     int           `json:"version"`
	Credentials []configEntry `json:"credentials"`
}

// Entry is a frozen credential snapshot. Secret copies share storage, so
// destroying the Catalog invalidates every Entry previously returned from it.
type Entry struct {
	Name     string
	Provider Provider
	Secret   secretvalue.Value
}
type Catalog struct{ entries map[string]Entry }

func Load(path string) (*Catalog, error) {
	data, err := readFile(path, MaxFileBytes, false)
	if err != nil {
		return nil, fmt.Errorf("agentcredential: catalog: %w", err)
	}
	return decode(data, filepath.Dir(path))
}

func decode(data []byte, baseDir string) (*Catalog, error) {
	var cfg configFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("agentcredential: decode catalog: %w", err)
	}
	if err := requireEOF(dec); err != nil {
		return nil, err
	}
	if cfg.Version != FileVersion {
		return nil, fmt.Errorf("agentcredential: version must be %d", FileVersion)
	}
	if len(cfg.Credentials) == 0 || len(cfg.Credentials) > MaxEntries {
		return nil, fmt.Errorf("agentcredential: credentials must contain 1..%d entries", MaxEntries)
	}
	c := &Catalog{entries: make(map[string]Entry, len(cfg.Credentials))}
	for i, configured := range cfg.Credentials {
		if err := validateConfig(configured); err != nil {
			c.Destroy()
			return nil, fmt.Errorf("agentcredential: credential %d: %w", i, err)
		}
		if _, exists := c.entries[configured.Name]; exists {
			c.Destroy()
			return nil, fmt.Errorf("agentcredential: duplicate credential %q", configured.Name)
		}
		path := configured.Source.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		secret, err := loadSecret(path, configured.Provider)
		if err != nil {
			c.Destroy()
			return nil, fmt.Errorf("agentcredential: credential %q: %w", configured.Name, err)
		}
		c.entries[configured.Name] = Entry{Name: configured.Name, Provider: configured.Provider, Secret: secret}
	}
	return c, nil
}

func requireEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("agentcredential: catalog contains trailing JSON")
		}
		return fmt.Errorf("agentcredential: decode catalog: %w", err)
	}
	return nil
}

func validateConfig(entry configEntry) error {
	if entry.Name == "" || len(entry.Name) > MaxNameBytes || strings.TrimSpace(entry.Name) != entry.Name {
		return fmt.Errorf("name must be non-empty, trimmed, and at most %d bytes", MaxNameBytes)
	}
	if entry.Provider != ProviderCodex {
		return fmt.Errorf("unsupported provider %q", entry.Provider)
	}
	if entry.Source.File == "" || strings.TrimSpace(entry.Source.File) != entry.Source.File {
		return errors.New("source.file must be non-empty and trimmed")
	}
	return nil
}

func loadSecret(path string, provider Provider) (secretvalue.Value, error) {
	data, err := readFile(path, secretvalue.MaxBytes, true)
	if err != nil {
		return secretvalue.Value{}, fmt.Errorf("source: %w", err)
	}
	defer zero(data)
	if len(data) == 0 {
		return secretvalue.Value{}, fmt.Errorf("source must contain 1..%d bytes", secretvalue.MaxBytes)
	}
	if provider == ProviderCodex && !json.Valid(data) {
		return secretvalue.Value{}, errors.New("codex source must contain a JSON auth store")
	}
	return secretvalue.New(data)
}

func readFile(path string, maxBytes int64, private bool) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("must be a regular file")
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("permissions %04o expose credentials to group or other users", info.Mode().Perm())
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- explicit trusted-host configuration path.
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if int64(len(data)) > maxBytes {
		zero(data)
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func (c *Catalog) Get(name string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	entry, ok := c.entries[name]
	return entry, ok
}

func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.entries))
	for name := range c.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (c *Catalog) Destroy() {
	if c == nil {
		return
	}
	for name, entry := range c.entries {
		entry.Secret.Destroy()
		delete(c.entries, name)
	}
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
