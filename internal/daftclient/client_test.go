package daftclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	testOrchestratorURL = "https://example.test"
	testWorkspaceUUID   = "workspace"
)

func TestOpenRequiresExistingIdentity(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(Config{Orchestrator: "https://example.test:9443", ConfigDir: dir, ProjectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "load client identity") {
		t.Fatalf("Open error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "key.pem")); !os.IsNotExist(err) {
		t.Fatalf("Open created identity: stat error = %v", err)
	}
}

func TestBootstrapBuildsSharedClients(t *testing.T) {
	dir := t.TempDir()
	client, err := Bootstrap(Config{Orchestrator: "https://example.test:9443/", ConfigDir: dir, ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Resolved.OrchestratorURL != "https://example.test:9443" {
		t.Fatalf("URL = %q", client.Resolved.OrchestratorURL)
	}
	if client.Workspace == nil || client.Nodes == nil || client.Health == nil || client.transport == nil {
		t.Fatalf("incomplete client: %+v", client)
	}
	if _, err := identity.Load(dir); err != nil {
		t.Fatalf("Bootstrap identity: %v", err)
	}
}

func TestOpenLoadsExistingIdentity(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.LoadOrGenerate(dir, identity.RoleClient); err != nil {
		t.Fatal(err)
	}
	client, err := Open(Config{Orchestrator: testOrchestratorURL, ConfigDir: dir, ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	client.Close()
}

func TestRejectsUnsafeOrAmbiguousURLs(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.LoadOrGenerate(dir, identity.RoleClient); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"%", "http://host:1", "https://", "https://user@host", "https://host/path", "https://host?x=1"} {
		t.Run(raw, func(t *testing.T) {
			_, err := Open(Config{Orchestrator: raw, ConfigDir: dir, ProjectDir: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), "orchestrator URL") {
				t.Fatalf("Open(%q) error = %v", raw, err)
			}
		})
	}
}

func TestOpenReportsPeerStoreError(t *testing.T) {
	dir := t.TempDir()
	if _, err := identity.LoadOrGenerate(dir, identity.RoleClient); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, peersFilename), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Open(Config{Orchestrator: testOrchestratorURL, ConfigDir: dir, ProjectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "open client peer store") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestNilClientClose(t *testing.T) {
	var client *Client
	client.Close()
	if _, err := client.PushWorkspace(context.Background(), WorkspacePushOptions{}); err == nil {
		t.Fatal("nil client PushWorkspace succeeded")
	}
}

func TestPushWorkspaceValidatesProjectAndCAS(t *testing.T) {
	dir := t.TempDir()
	project := t.TempDir()
	client, err := Bootstrap(Config{
		Orchestrator: testOrchestratorURL, ConfigDir: dir, ProjectDir: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.PushWorkspace(t.Context(), WorkspacePushOptions{
		WorkspaceUUID: testWorkspaceUUID, ProjectDir: project,
	}); err == nil || !strings.Contains(err.Error(), "project config") {
		t.Fatalf("missing project config error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(project, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".daft", "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := client.PushWorkspace(t.Context(), WorkspacePushOptions{
		WorkspaceUUID: testWorkspaceUUID, ProjectDir: project,
	})
	if err != nil || result.ProjectDir != project {
		t.Fatalf("empty PushWorkspace = %#v, %v", result, err)
	}
	casPath := filepath.Join(t.TempDir(), "cas-file")
	if err := os.WriteFile(casPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushWorkspace(t.Context(), WorkspacePushOptions{
		WorkspaceUUID: testWorkspaceUUID, ProjectDir: project, CASDir: casPath,
	}); err == nil || !strings.Contains(err.Error(), "workspace CAS") {
		t.Fatalf("invalid CAS error = %v", err)
	}
}
