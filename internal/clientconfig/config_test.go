package clientconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/localcluster"
)

var errDiscovery = errors.New("discovery failed")

func TestResolvePrecedenceAndIdentity(t *testing.T) {
	project := t.TempDir()
	global := t.TempDir()
	cluster := &localcluster.Cluster{URL: "https://cluster:1", ClientConfigDir: "/cluster/client"}
	cfg := Config{ProjectDir: project, ConfigDir: global, DiscoverLocalCluster: func() (Discovery, error) {
		return Discovery{Cluster: cluster}, nil
	}}
	t.Setenv(OrchestratorEnvVar, "")

	got, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.OrchestratorURL != cluster.URL || got.Source != SourceLocalCluster || got.IdentityDir != global {
		t.Fatalf("cluster with explicit config dir = %+v", got)
	}

	cfg.ConfigDir = ""
	t.Setenv("XDG_CONFIG_HOME", global)
	got, err = Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdentityDir != cluster.ClientConfigDir {
		t.Fatalf("cluster identity = %q", got.IdentityDir)
	}

	writeConfig(t, filepath.Join(global, clientConfigSubdir, configFilename), "https://global:2")
	got, err = Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.OrchestratorURL != "https://global:2" || got.Source != SourceGlobal || got.IdentityDir == cluster.ClientConfigDir {
		t.Fatalf("global = %+v", got)
	}

	writeConfig(t, filepath.Join(project, ".daft", configFilename), "https://project:3")
	t.Setenv(OrchestratorEnvVar, "https://env:4")
	cfg.Orchestrator = "https://explicit:5"
	got, err = Resolve(cfg)
	if err != nil || got.OrchestratorURL != cfg.Orchestrator || got.Source != SourceExplicit {
		t.Fatalf("explicit = %+v, %v", got, err)
	}
	cfg.Orchestrator = ""
	got, err = Resolve(cfg)
	if err != nil || got.OrchestratorURL != "https://env:4" || got.Source != SourceEnvironment {
		t.Fatalf("environment = %+v, %v", got, err)
	}
	t.Setenv(OrchestratorEnvVar, "")
	got, err = Resolve(cfg)
	if err != nil || got.OrchestratorURL != "https://project:3" || got.Source != SourceProject {
		t.Fatalf("project = %+v, %v", got, err)
	}
}

func TestResolveDefaultAndExplicitProject(t *testing.T) {
	t.Setenv(OrchestratorEnvVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	got, err := Resolve(Config{ProjectDir: project, DiscoverLocalCluster: func() (Discovery, error) {
		return Discovery{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.OrchestratorURL != DefaultOrchestratorURL || got.Source != SourceDefault {
		t.Fatalf("default = %+v", got)
	}
	writeConfig(t, filepath.Join(project, ".daft", configFilename), "https://project:1")
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	got, err = Resolve(Config{ProjectDir: project, DiscoverLocalCluster: func() (Discovery, error) {
		return Discovery{}, nil
	}})
	if err != nil || got.OrchestratorURL != "https://project:1" {
		t.Fatalf("explicit project independent of cwd = %+v, %v", got, err)
	}
}

func TestResolveReportsMalformedStateAndConfig(t *testing.T) {
	t.Setenv(OrchestratorEnvVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path := filepath.Join(state, "daft", "cluster", "cluster.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Config{ProjectDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "discover local cluster") {
		t.Fatalf("malformed state error = %v", err)
	}

	project := t.TempDir()
	configPath := filepath.Join(project, ".daft", configFilename)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Config{ProjectDir: project, DiscoverLocalCluster: func() (Discovery, error) {
		return Discovery{}, nil
	}}); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("malformed config error = %v", err)
	}
}

func TestResolveReportsDiscoveryAndReadErrors(t *testing.T) {
	t.Setenv(OrchestratorEnvVar, "")
	project := t.TempDir()
	global := t.TempDir()
	if _, err := Resolve(Config{
		ProjectDir: project,
		ConfigDir:  global,
		DiscoverLocalCluster: func() (Discovery, error) {
			return Discovery{}, errDiscovery
		},
	}); !errors.Is(err, errDiscovery) {
		t.Fatalf("discovery error = %v", err)
	}

	projectConfig := filepath.Join(project, ".daft", configFilename)
	if err := os.MkdirAll(projectConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Config{
		ProjectDir: project,
		ConfigDir:  global,
		DiscoverLocalCluster: func() (Discovery, error) {
			return Discovery{}, nil
		},
	}); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("config read error = %v", err)
	}
}

func TestResolveUsesCwdAndDefaultDiscovery(t *testing.T) {
	t.Setenv(OrchestratorEnvVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	t.Chdir(project)
	writeConfig(t, filepath.Join(project, ".daft", configFilename), "https://cwd:1")
	got, err := Resolve(Config{})
	if err != nil || got.OrchestratorURL != "https://cwd:1" {
		t.Fatalf("cwd resolution = %+v, %v", got, err)
	}
}

func TestResolveDefaultDiscoveryFindsLiveCluster(t *testing.T) {
	t.Setenv(OrchestratorEnvVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "daft", "cluster")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"url":"https://live:1","client_config_dir":"/live/client","pids":[` + fmt.Sprint(os.Getpid()) + `]}`)
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(Config{ProjectDir: t.TempDir()})
	if err != nil || got.OrchestratorURL != "https://live:1" || got.IdentityDir != "/live/client" {
		t.Fatalf("live cluster = %+v, %v", got, err)
	}
}

func TestResolveReportsGlobalReadError(t *testing.T) {
	t.Setenv(OrchestratorEnvVar, "")
	global := t.TempDir()
	if err := os.Mkdir(filepath.Join(global, configFilename), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(Config{
		ProjectDir: t.TempDir(),
		ConfigDir:  global,
		DiscoverLocalCluster: func() (Discovery, error) {
			return Discovery{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("global read error = %v", err)
	}
}

func writeConfig(t *testing.T, path, orchestrator string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"orchestrator":"` + orchestrator + `","ignored":"ok"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
