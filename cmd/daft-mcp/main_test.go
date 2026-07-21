package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/agentcredential"
	"github.com/richiejp/darkaifactory/internal/agentprofile"
	"github.com/richiejp/darkaifactory/internal/daftclient"
	"github.com/richiejp/darkaifactory/internal/mcpserver"
)

const (
	testValue         = "test"
	testAgentProfiles = "--agent-profiles"
)

func stubDependencies(t *testing.T) {
	t.Helper()
	oldOpen, oldReader, oldServer, oldRun := openClient, newReader, newServer, runServer
	t.Cleanup(func() { openClient, newReader, newServer, runServer = oldOpen, oldReader, oldServer, oldRun })
	openClient = func(daftclient.Config) (*daftclient.Client, error) { return &daftclient.Client{}, nil }
	newReader = func(*daftclient.Client, *agentcredential.Catalog) (*mcpserver.ConnectReader, error) {
		return &mcpserver.ConnectReader{}, nil
	}
	newServer = func(mcpserver.Reader, []agentprofile.Entry, string) (*mcp.Server, error) {
		return mcp.NewServer(&mcp.Implementation{Name: testValue, Version: testValue}, nil), nil
	}
	runServer = func(context.Context, *mcp.Server, io.Reader, io.Writer) error { return nil }
}

func TestRunPassesExplicitConfigurationAndStreams(t *testing.T) {
	stubDependencies(t)
	var got daftclient.Config
	openClient = func(config daftclient.Config) (*daftclient.Client, error) {
		got = config
		return &daftclient.Client{}, nil
	}
	if code := run(t.Context(), []string{"--orchestrator=https://host:8443", "--config-dir=/config", "--project-dir=/project"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got.Orchestrator != "https://host:8443" || got.ConfigDir != "/config" || got.ProjectDir != "/project" {
		t.Errorf("config = %#v", got)
	}
}

func TestRunLoadsAgentProfilesBeforeStartingServer(t *testing.T) {
	stubDependencies(t)
	path := filepath.Join(t.TempDir(), "profiles.json")
	data := `{"version":1,"profiles":[{"name":"codex","provider":"codex","image":"example.invalid/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":["codex","exec"],"credentials":{"enable":true,"profile":"subscription"},"resources":{"enable":false},"egress":{"enable":true}}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	credentialData := fmt.Sprintf(`{"version":1,"credentials":[{"name":"subscription","provider":"codex","source":{"file":%q}}]}`, authPath)
	if err := os.WriteFile(credentialPath, []byte(credentialData), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []agentprofile.Entry
	newServer = func(_ mcpserver.Reader, profiles []agentprofile.Entry, _ string) (*mcp.Server, error) {
		got = profiles
		return mcp.NewServer(&mcp.Implementation{Name: testValue, Version: testValue}, nil), nil
	}
	if code := run(t.Context(), []string{testAgentProfiles, path, "--agent-credentials", credentialPath}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("run code = %d", code)
	}
	if len(got) != 1 || got[0].Profile.Name != "codex" || got[0].Digest == "" {
		t.Fatalf("profiles = %#v", got)
	}
}

func TestValidateCredentialBindings(t *testing.T) {
	disabled := agentprofile.Entry{Profile: agentprofile.Profile{Name: "disabled"}}
	if err := validateCredentialBindings([]agentprofile.Entry{disabled}, nil); err != nil {
		t.Fatal(err)
	}
	required := agentprofile.Entry{Profile: agentprofile.Profile{Name: "codex", Provider: agentprofile.ProviderCodex, Credentials: agentprofile.CredentialsSection{Enable: true, Profile: "subscription"}}}
	if err := validateCredentialBindings([]agentprofile.Entry{required}, nil); err == nil || !strings.Contains(err.Error(), "requires --agent-credentials") {
		t.Fatalf("missing catalog error = %v", err)
	}

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(t.TempDir(), "credentials.json")
	data := fmt.Sprintf(`{"version":1,"credentials":[{"name":"other","provider":"codex","source":{"file":%q}}]}`, authPath)
	if err := os.WriteFile(catalogPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := agentcredential.Load(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Destroy)
	if err := validateCredentialBindings([]agentprofile.Entry{required}, catalog); err == nil || !strings.Contains(err.Error(), "unknown credential") {
		t.Fatalf("unknown credential error = %v", err)
	}

	required.Profile.Credentials.Profile = "other"
	required.Profile.Provider = agentprofile.Provider("claude")
	if err := validateCredentialBindings([]agentprofile.Entry{required}, catalog); err == nil || !strings.Contains(err.Error(), "provider does not match") {
		t.Fatalf("provider mismatch error = %v", err)
	}
}

func TestRunRejectsInvalidAgentProfileFile(t *testing.T) {
	stubDependencies(t)
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{testAgentProfiles, filepath.Join(t.TempDir(), "missing")}, strings.NewReader(""), io.Discard, &stderr); code != 1 {
		t.Fatalf("run code = %d", code)
	}
	if !strings.Contains(stderr.String(), "agentprofile") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRejectsInvalidCredentialCatalogAndMissingBinding(t *testing.T) {
	stubDependencies(t)
	for _, args := range [][]string{
		{"--agent-credentials", filepath.Join(t.TempDir(), "missing")},
		{testAgentProfiles, writeRequiredProfile(t)},
	} {
		var stderr bytes.Buffer
		if code := run(t.Context(), args, strings.NewReader(""), io.Discard, &stderr); code != 1 {
			t.Fatalf("run(%v) code = %d, stderr = %q", args, code, stderr.String())
		}
	}
}

func writeRequiredProfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	data := `{"version":1,"profiles":[{"name":"codex","provider":"codex","image":"example.invalid/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":["codex","exec"],"credentials":{"enable":true,"profile":"subscription"},"resources":{"enable":false},"egress":{"enable":true}}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	stubDependencies(t)
	for _, args := range [][]string{{"--unknown"}, {"positional"}} {
		var stderr bytes.Buffer
		if code := run(t.Context(), args, strings.NewReader(""), io.Discard, &stderr); code != 2 {
			t.Errorf("run(%v) code = %d, want 2", args, code)
		}
		if stderr.Len() == 0 {
			t.Errorf("run(%v) wrote no diagnostic", args)
		}
	}
}

func TestRunReportsDependencyAndTransportErrors(t *testing.T) {
	tests := []struct {
		name string
		set  func()
	}{
		{name: "open", set: func() {
			openClient = func(daftclient.Config) (*daftclient.Client, error) { return nil, errors.New("open") }
		}},
		{name: "reader", set: func() {
			newReader = func(*daftclient.Client, *agentcredential.Catalog) (*mcpserver.ConnectReader, error) {
				return nil, errors.New("reader")
			}
		}},
		{name: "server", set: func() {
			newServer = func(mcpserver.Reader, []agentprofile.Entry, string) (*mcp.Server, error) {
				return nil, errors.New("server")
			}
		}},
		{name: "transport", set: func() {
			runServer = func(context.Context, *mcp.Server, io.Reader, io.Writer) error { return errors.New("transport") }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stubDependencies(t)
			test.set()
			var stderr bytes.Buffer
			if code := run(t.Context(), nil, strings.NewReader(""), io.Discard, &stderr); code != 1 {
				t.Fatalf("run code = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), test.name) {
				t.Errorf("stderr = %q, want %q", stderr.String(), test.name)
			}
		})
	}
}

func TestReportErrorHandlesBrokenStderr(t *testing.T) {
	if code := reportError(errorWriter{}, errors.New("failure")); code != 2 {
		t.Fatalf("reportError code = %d, want 2", code)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestNOPCloserWriter(t *testing.T) {
	var output bytes.Buffer
	writer := nopCloserWriter{Writer: &output}
	if _, err := writer.Write([]byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil || output.String() != "value" {
		t.Fatalf("Close() = %v, output = %q", err, output.String())
	}
}

func TestRunMCPServerHandlesInputEOF(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: testValue, Version: testValue}, nil)
	if err := runMCPServer(t.Context(), server, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("runMCPServer: %v", err)
	}
}

func TestMainReturnsArgumentFailure(t *testing.T) {
	oldArgs, oldExit := os.Args, osExit
	os.Args = []string{"daft-mcp", "unexpected"}
	exitCode := -1
	osExit = func(code int) { exitCode = code }
	t.Cleanup(func() { os.Args, osExit = oldArgs, oldExit })
	main()
	if exitCode != 2 {
		t.Fatalf("main exit code = %d, want 2", exitCode)
	}
}
