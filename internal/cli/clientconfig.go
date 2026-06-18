package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// defaultOrchestratorURL is the built-in fallback used when no flag,
// env var, dir-local config, or global config supplies one. It
// mirrors `daft workspace serve`'s direct-dial default
// (--addr 0.0.0.0:9443) so a single-host dev setup works with zero
// configuration.
const defaultOrchestratorURL = "https://localhost:9443"

// orchestratorEnvVar overrides the config files but loses to an
// explicit --orchestrator flag. Follows the DAFT_ prefix convention.
const orchestratorEnvVar = envPrefix + "ORCHESTRATOR"

// dirLocalConfigDir / dirLocalConfigFile name the per-project config
// that lives in the current working directory: ./.daft/config.json.
// Discovery is cwd-only — no parent walk — so behaviour doesn't
// depend on where in a tree you happen to stand.
const (
	dirLocalConfigDir  = ".daft"
	dirLocalConfigFile = "config.json"
)

// globalConfigFile is the per-user config sitting alongside cert.pem,
// key.pem, and peers.json in the client config dir.
const globalConfigFile = "config.json"

// Config keys recognised by `get`/`set`. orchestrator is meaningful
// in both scopes; name only as a dir-local default workspace name.
const (
	configKeyOrchestrator = "orchestrator"
	configKeyName         = "name"
)

// globalConfigKeys / dirLocalConfigKeys are the per-scope allow-lists
// the get/set commands validate against, also defining the order
// `get` (no key) prints them in. A global default workspace name is
// meaningless, so name is dir-local only.
func globalConfigKeys() []string   { return []string{configKeyOrchestrator} }
func dirLocalConfigKeys() []string { return []string{configKeyOrchestrator, configKeyName} }

// mountMapping records one local↔in-VM directory mapping in the
// dir-local config. `daft workspace mount ./local /remote` appends
// one; push/pull walk the list. Local is stored as the user typed it
// (resolved against the cwd at push/pull time) so the config stays
// portable if the project moves.
type mountMapping struct {
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// fileConfig is the on-disk shape of both the global and dir-local
// config files. Fields are additive and omitempty so an older binary
// tolerates keys a newer one writes (unknown JSON keys are ignored on
// read). Mounts only ever appears in the dir-local file.
type fileConfig struct {
	Orchestrator string         `json:"orchestrator,omitempty"`
	Name         string         `json:"name,omitempty"`
	Mounts       []mountMapping `json:"mounts,omitempty"`
	// Devcontainer is the absolute HOST path of the devcontainer.json the
	// `daft workspace devcontainer` command recorded (resolved against the
	// cwd at that time). `daft workspace up` reads it to learn the
	// forwardPorts to auto-open — the orchestrator resolves the
	// devcontainer node-side and never reports the ports back, so the
	// client re-parses the file locally. Dir-local only (meaningless in the
	// global scope); not a get/set key.
	Devcontainer string `json:"devcontainer,omitempty"`
}

// get returns the value for key and whether key is a known field.
func (c fileConfig) get(key string) (string, bool) {
	switch key {
	case configKeyOrchestrator:
		return c.Orchestrator, true
	case configKeyName:
		return c.Name, true
	default:
		return "", false
	}
}

// set assigns value to the field named by key, returning false for an
// unknown key.
func (c *fileConfig) set(key, value string) bool {
	switch key {
	case configKeyOrchestrator:
		c.Orchestrator = value
	case configKeyName:
		c.Name = value
	default:
		return false
	}
	return true
}

// loadFileConfig reads path into a fileConfig. A missing file is not
// an error — config files are optional — and yields the zero value.
func loadFileConfig(path string) (fileConfig, error) {
	var c fileConfig
	// #nosec G304 — caller-supplied config path.
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("daft: read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("daft: parse config %s: %w", path, err)
	}
	return c, nil
}

// saveFileConfig writes c to path atomically (temp file + rename),
// creating the parent directory if needed.
func saveFileConfig(path string, c fileConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("daft: create config dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("daft: marshal config: %w", err)
	}
	data = append(data, '\n')
	// Per-process temp name (not a fixed path+".tmp") so two concurrent
	// writers — necessarily separate processes for a CLI — can't
	// interleave into one temp file and rename a torn result into place.
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("daft: write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("daft: replace config: %w", err)
	}
	return nil
}

// globalConfigPath returns <clientDir>/config.json, honouring the
// same --config-dir override the identity store uses.
func globalConfigPath(configDirOverride string) (string, error) {
	dir, err := resolveClientDir(configDirOverride)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, globalConfigFile), nil
}

// dirLocalConfigPath returns ./.daft/config.json relative to the
// current working directory.
func dirLocalConfigPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("daft: getwd: %w", err)
	}
	return filepath.Join(cwd, dirLocalConfigDir, dirLocalConfigFile), nil
}

// loadDirLocalConfig loads ./.daft/config.json (absent → zero value).
func loadDirLocalConfig() (fileConfig, error) {
	path, err := dirLocalConfigPath()
	if err != nil {
		return fileConfig{}, err
	}
	return loadFileConfig(path)
}

// configSource labels which layer of the precedence chain a resolved
// value came from. Used by `daft globals resolve` to explain the
// effective configuration.
type configSource string

const (
	sourceFlag         configSource = "--orchestrator flag"
	sourceEnv          configSource = "$" + orchestratorEnvVar
	sourceDirLocal     configSource = "./" + dirLocalConfigDir + "/" + dirLocalConfigFile
	sourceGlobal       configSource = "global config"
	sourceLocalCluster configSource = "running local cluster (daft cluster up)"
	sourceDefault      configSource = "built-in default"
	sourceCwd          configSource = "basename(cwd)"
)

// resolveOrchestratorSource is the precedence chain, highest first:
// explicit --orchestrator flag, $DAFT_ORCHESTRATOR, ./.daft/config.json,
// the global config, a running single-box cluster (`daft cluster up`),
// then the built-in localhost default. It never returns an empty URL and
// always names the winning layer.
//
// The local-cluster layer sits just above the built-in default and below
// every explicit config, so `daft cluster up` followed by a bare
// `daft workspace …` connects with no flag and no env var, while a user
// who has pointed daft at a real orchestrator (flag/env/config) is never
// silently redirected to a local cluster that happens to be up.
func resolveOrchestratorSource(flagVal, configDirOverride string) (string, configSource, error) {
	if flagVal != "" {
		return flagVal, sourceFlag, nil
	}
	if env := os.Getenv(orchestratorEnvVar); env != "" {
		return env, sourceEnv, nil
	}
	dirLocal, err := loadDirLocalConfig()
	if err != nil {
		return "", "", err
	}
	if dirLocal.Orchestrator != "" {
		return dirLocal.Orchestrator, sourceDirLocal, nil
	}
	globalPath, err := globalConfigPath(configDirOverride)
	if err != nil {
		return "", "", err
	}
	global, err := loadFileConfig(globalPath)
	if err != nil {
		return "", "", err
	}
	if global.Orchestrator != "" {
		return global.Orchestrator, sourceGlobal, nil
	}
	if c, ok := discoverLocalCluster(); ok && c.URL != "" {
		return c.URL, sourceLocalCluster, nil
	}
	return defaultOrchestratorURL, sourceDefault, nil
}

// resolveOrchestrator is the value-only wrapper used by the command
// runners; they don't care which layer won.
func resolveOrchestrator(flagVal, configDirOverride string) (string, error) {
	v, _, err := resolveOrchestratorSource(flagVal, configDirOverride)
	return v, err
}

// resolveWorkspaceNameSource returns the dir-local config's name if
// set, else basename($PWD), naming the winning layer.
func resolveWorkspaceNameSource() (string, configSource, error) {
	dirLocal, err := loadDirLocalConfig()
	if err != nil {
		return "", "", err
	}
	if dirLocal.Name != "" {
		return dirLocal.Name, sourceDirLocal, nil
	}
	name, err := deriveWorkspaceNameFromCwd()
	if err != nil {
		return "", "", err
	}
	return name, sourceCwd, nil
}

// resolveWorkspaceNameDefault is the value-only wrapper used when
// --name is omitted.
func resolveWorkspaceNameDefault() (string, error) {
	v, _, err := resolveWorkspaceNameSource()
	return v, err
}

// setDirLocalDevcontainer records (or, with an empty hostPath, clears) the
// absolute host path of the workspace's devcontainer.json in
// ./.daft/config.json, so `daft workspace up` can re-parse it for the
// forwardPorts to auto-open.
func setDirLocalDevcontainer(hostPath string) error {
	path, err := dirLocalConfigPath()
	if err != nil {
		return err
	}
	c, err := loadFileConfig(path)
	if err != nil {
		return err
	}
	c.Devcontainer = hostPath
	return saveFileConfig(path, c)
}

// dirLocalDevcontainer returns the recorded devcontainer.json host path
// (empty when none).
func dirLocalDevcontainer() (string, error) {
	c, err := loadDirLocalConfig()
	if err != nil {
		return "", err
	}
	return c.Devcontainer, nil
}

// dirLocalMounts returns the mount mappings recorded in
// ./.daft/config.json (nil when none).
func dirLocalMounts() ([]mountMapping, error) {
	c, err := loadDirLocalConfig()
	if err != nil {
		return nil, err
	}
	return c.Mounts, nil
}

// addDirLocalMount appends (or, when the in-VM target already exists,
// replaces) a mount mapping in ./.daft/config.json and returns the
// file path it wrote.
func addDirLocalMount(m mountMapping) (string, error) {
	path, err := dirLocalConfigPath()
	if err != nil {
		return "", err
	}
	c, err := loadFileConfig(path)
	if err != nil {
		return "", err
	}
	replaced := false
	for i := range c.Mounts {
		if c.Mounts[i].Remote == m.Remote {
			c.Mounts[i] = m
			replaced = true
			break
		}
	}
	if !replaced {
		c.Mounts = append(c.Mounts, m)
	}
	if err := saveFileConfig(path, c); err != nil {
		return "", err
	}
	return path, nil
}

// removeDirLocalMount drops the mapping whose in-VM target is remote,
// reporting whether one was found.
func removeDirLocalMount(remote string) (found bool, err error) {
	path, err := dirLocalConfigPath()
	if err != nil {
		return false, err
	}
	c, err := loadFileConfig(path)
	if err != nil {
		return false, err
	}
	kept := c.Mounts[:0]
	for _, m := range c.Mounts {
		if m.Remote == remote {
			found = true
			continue
		}
		kept = append(kept, m)
	}
	if !found {
		return false, nil
	}
	c.Mounts = kept
	if err := saveFileConfig(path, c); err != nil {
		return false, err
	}
	return true, nil
}

// validateConfigValue rejects values that would silently break a
// command. Currently only the orchestrator URL: the Connect client
// needs an http(s):// scheme, so a scheme-less host is caught here
// rather than surfacing as an opaque dial failure later.
func validateConfigValue(key, value string) error {
	if key == configKeyOrchestrator {
		if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
			return fmt.Errorf("orchestrator must be a URL with an http:// or https:// scheme, got %q", value)
		}
	}
	return nil
}

// knownConfigKey reports whether key is in the scope's allow-list.
func knownConfigKey(allowed []string, key string) bool {
	return slices.Contains(allowed, key)
}
