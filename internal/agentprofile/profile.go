// Package agentprofile loads immutable, trusted-host coding-agent profiles.
package agentprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

const (
	FileVersion        = 1
	MaxFileBytes       = 1 << 20
	MaxProfiles        = 128
	MaxNameBytes       = 128
	MaxImageBytes      = 4 << 10
	MaxCommandArgs     = 64
	MaxCommandArgBytes = 16 << 10
	MaxCPUCores        = 1024
	MaxMemoryBytes     = 16 << 40
)

type Provider string

const ProviderCodex Provider = "codex"

// Profile is launch policy configured by the trusted MCP host. It contains a
// credential profile name, never credential material.
type Profile struct {
	Name        string             `json:"name"`
	Provider    Provider           `json:"provider"`
	Image       string             `json:"image"`
	Command     []string           `json:"command"`
	Credentials CredentialsSection `json:"credentials"`
	Resources   ResourcesSection   `json:"resources"`
	Egress      EgressSection      `json:"egress"`
}

type CredentialsSection struct {
	Enable  bool   `json:"enable"`
	Profile string `json:"profile,omitempty"`
}

type ResourcesSection struct {
	Enable      bool   `json:"enable"`
	CPUCores    uint32 `json:"cpu_cores,omitempty"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
}

type EgressSection struct {
	Enable bool `json:"enable"`
}

type Entry struct {
	Profile Profile `json:"profile"`
	Digest  string  `json:"digest"`
}

type file struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}

// Catalog is a frozen snapshot. Returned profiles are deep copies so callers
// cannot mutate a profile selected for a run after its digest was recorded.
type Catalog struct {
	entries map[string]Entry
}

func Load(path string) (*Catalog, error) {
	f, err := os.Open(path) // #nosec G304 -- explicit trusted-host configuration path.
	if err != nil {
		return nil, fmt.Errorf("agentprofile: open %s: %w", path, err)
	}
	catalog, decodeErr := Decode(io.LimitReader(f, MaxFileBytes+1))
	closeErr := f.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("agentprofile: close %s: %w", path, closeErr)
	}
	return catalog, nil
}

func Decode(r io.Reader) (*Catalog, error) {
	if r == nil {
		return nil, errors.New("agentprofile: reader is required")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("agentprofile: read: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("agentprofile: file exceeds %d bytes", MaxFileBytes)
	}
	if err := rejectSecretFields(data); err != nil {
		return nil, err
	}
	var cfg file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("agentprofile: decode: %w", err)
	}
	if cfg.Version != FileVersion {
		return nil, fmt.Errorf("agentprofile: version must be %d", FileVersion)
	}
	if len(cfg.Profiles) == 0 || len(cfg.Profiles) > MaxProfiles {
		return nil, fmt.Errorf("agentprofile: profiles must contain 1..%d entries", MaxProfiles)
	}
	c := &Catalog{entries: make(map[string]Entry, len(cfg.Profiles))}
	for i := range cfg.Profiles {
		profile := clone(cfg.Profiles[i])
		if err := validate(profile); err != nil {
			return nil, fmt.Errorf("agentprofile: profile %d: %w", i, err)
		}
		if _, exists := c.entries[profile.Name]; exists {
			return nil, fmt.Errorf("agentprofile: duplicate profile %q", profile.Name)
		}
		c.entries[profile.Name] = Entry{Profile: profile, Digest: profileDigest(profile)}
	}
	return c, nil
}

func (c *Catalog) Get(profileName string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	entry, ok := c.entries[profileName]
	entry.Profile = clone(entry.Profile)
	return entry, ok
}

func (c *Catalog) List() []Entry {
	if c == nil {
		return nil
	}
	out := make([]Entry, 0, len(c.entries))
	for profileName := range c.entries {
		entry := c.entries[profileName]
		entry.Profile = clone(entry.Profile)
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Profile.Name, b.Profile.Name) })
	return out
}

func validate(p Profile) error {
	if p.Name == "" || len(p.Name) > MaxNameBytes || strings.TrimSpace(p.Name) != p.Name {
		return fmt.Errorf("name must be non-empty, trimmed, and at most %d bytes", MaxNameBytes)
	}
	if p.Provider != ProviderCodex {
		return fmt.Errorf("unsupported provider %q", p.Provider)
	}
	if p.Image == "" || len(p.Image) > MaxImageBytes {
		return fmt.Errorf("image must be non-empty and at most %d bytes", MaxImageBytes)
	}
	if _, err := name.NewDigest(p.Image, name.StrictValidation); err != nil {
		return fmt.Errorf("image must be an OCI digest reference: %w", err)
	}
	if len(p.Command) == 0 || len(p.Command) > MaxCommandArgs {
		return fmt.Errorf("command must contain 1..%d arguments", MaxCommandArgs)
	}
	for _, arg := range p.Command {
		if arg == "" || len(arg) > MaxCommandArgBytes {
			return fmt.Errorf("command arguments must be non-empty and at most %d bytes", MaxCommandArgBytes)
		}
	}
	if p.Credentials.Enable != (p.Credentials.Profile != "") || len(p.Credentials.Profile) > MaxNameBytes || strings.TrimSpace(p.Credentials.Profile) != p.Credentials.Profile {
		return fmt.Errorf("credentials.profile must be set exactly when credentials.enable is true")
	}
	if (p.Resources.Enable && (p.Resources.CPUCores == 0 || p.Resources.MemoryBytes == 0)) ||
		(!p.Resources.Enable && (p.Resources.CPUCores != 0 || p.Resources.MemoryBytes != 0)) {
		return fmt.Errorf("resource limits must both be positive exactly when resources.enable is true")
	}
	if p.Resources.CPUCores > MaxCPUCores || p.Resources.MemoryBytes > MaxMemoryBytes {
		return fmt.Errorf("resource limits exceed %d CPU cores or %d memory bytes", MaxCPUCores, MaxMemoryBytes)
	}
	return nil
}

func profileDigest(p Profile) string {
	data, _ := json.Marshal(p) // Profile contains only JSON-native scalar and slice fields.
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func clone(p Profile) Profile {
	p.Command = slices.Clone(p.Command)
	return p
}

func rejectSecretFields(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("agentprofile: decode: %w", err)
	}
	var walk func(any) error
	walk = func(v any) error {
		switch typed := v.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
				if forbiddenSecretField(normalized) {
					return fmt.Errorf("agentprofile: forbidden credential field %q", key)
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func forbiddenSecretField(field string) bool {
	switch field {
	case "access_token", "anthropic_api_key", "api_key", "claude_code_oauth_token", "codex_api_key", "oauth_token", "openai_api_key", "refresh_token", "token":
		return true
	default:
		return false
	}
}
