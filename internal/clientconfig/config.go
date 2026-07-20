// Package clientconfig resolves the endpoint and identity used by daft clients.
package clientconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/richiejp/darkaifactory/internal/localcluster"
)

const (
	DefaultOrchestratorURL = "https://localhost:9443"
	OrchestratorEnvVar     = "DAFT_ORCHESTRATOR"
	clientConfigSubdir     = "daft/client"
	configFilename         = "config.json"
)

type Source string

const (
	SourceExplicit     Source = "explicit orchestrator"
	SourceEnvironment  Source = "$DAFT_ORCHESTRATOR"
	SourceProject      Source = "project config"
	SourceGlobal       Source = "global config"
	SourceLocalCluster Source = "running local cluster (daft cluster up)"
	SourceDefault      Source = "built-in default"
)

// Config describes resolution inputs. ProjectDir is explicit so a long-lived
// process can serve several projects without changing process cwd. An empty
// ProjectDir uses the current directory for CLI compatibility.
type Config struct {
	Orchestrator string
	ConfigDir    string
	ProjectDir   string

	// DiscoverLocalCluster is an optional test seam. Production callers leave
	// it nil to use the default state directory.
	DiscoverLocalCluster func() (Discovery, error)
}

type Discovery struct {
	Cluster *localcluster.Cluster
}

type Resolved struct {
	OrchestratorURL string
	IdentityDir     string
	Source          Source
}

type fileConfig struct {
	Orchestrator string `json:"orchestrator"`
}

// Resolve applies explicit, environment, project, global, local-cluster, then
// built-in precedence and chooses the identity directory matching the result.
func Resolve(cfg Config) (Resolved, error) {
	projectDir, err := resolveProjectDir(cfg.ProjectDir)
	if err != nil {
		return Resolved{}, err
	}
	configDir, err := resolveConfigDir(cfg.ConfigDir)
	if err != nil {
		return Resolved{}, err
	}

	discovery, err := discover(cfg.DiscoverLocalCluster)
	if err != nil {
		return Resolved{}, err
	}
	cluster := discovery.Cluster
	url, source, err := resolveURL(cfg.Orchestrator, projectDir, configDir, cluster)
	if err != nil {
		return Resolved{}, err
	}
	identityDir := configDir
	if cfg.ConfigDir == "" && cluster != nil && cluster.ClientConfigDir != "" && url == cluster.URL {
		identityDir = cluster.ClientConfigDir
	}
	return Resolved{OrchestratorURL: url, IdentityDir: identityDir, Source: source}, nil
}

func resolveURL(explicit, projectDir, configDir string, cluster *localcluster.Cluster) (string, Source, error) {
	if explicit != "" {
		return explicit, SourceExplicit, nil
	}
	if value := os.Getenv(OrchestratorEnvVar); value != "" {
		return value, SourceEnvironment, nil
	}
	project, err := load(filepath.Join(projectDir, ".daft", configFilename))
	if err != nil {
		return "", "", err
	}
	if project.Orchestrator != "" {
		return project.Orchestrator, SourceProject, nil
	}
	global, err := load(filepath.Join(configDir, configFilename))
	if err != nil {
		return "", "", err
	}
	if global.Orchestrator != "" {
		return global.Orchestrator, SourceGlobal, nil
	}
	if cluster != nil && cluster.URL != "" {
		return cluster.URL, SourceLocalCluster, nil
	}
	return DefaultOrchestratorURL, SourceDefault, nil
}

func resolveProjectDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("daft: getwd: %w", err)
	}
	return cwd, nil
}

func resolveConfigDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("daft: locate config dir: %w", err)
	}
	return filepath.Join(base, clientConfigSubdir), nil
}

func discover(seam func() (Discovery, error)) (Discovery, error) {
	if seam != nil {
		return seam()
	}
	dir, err := localcluster.DefaultStateDir()
	if err != nil {
		return Discovery{}, err
	}
	cluster, err := localcluster.Load(dir)
	if err != nil {
		return Discovery{}, fmt.Errorf("daft: discover local cluster: %w", err)
	}
	if cluster == nil || !cluster.Alive() {
		return Discovery{}, nil
	}
	return Discovery{Cluster: cluster}, nil
}

func load(path string) (fileConfig, error) {
	var cfg fileConfig
	data, err := os.ReadFile(path) // #nosec G304 -- resolved client configuration path.
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("daft: read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("daft: parse config %s: %w", path, err)
	}
	return cfg, nil
}
