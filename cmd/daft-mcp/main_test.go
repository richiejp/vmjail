package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/daftclient"
	"github.com/richiejp/darkaifactory/internal/mcpserver"
)

const testValue = "test"

func stubDependencies(t *testing.T) {
	t.Helper()
	oldOpen, oldReader, oldServer, oldRun := openClient, newReader, newServer, runServer
	t.Cleanup(func() { openClient, newReader, newServer, runServer = oldOpen, oldReader, oldServer, oldRun })
	openClient = func(daftclient.Config) (*daftclient.Client, error) { return &daftclient.Client{}, nil }
	newReader = func(*daftclient.Client) (*mcpserver.ConnectReader, error) { return &mcpserver.ConnectReader{}, nil }
	newServer = func(mcpserver.Reader, string) (*mcp.Server, error) {
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
			newReader = func(*daftclient.Client) (*mcpserver.ConnectReader, error) { return nil, errors.New("reader") }
		}},
		{name: "server", set: func() {
			newServer = func(mcpserver.Reader, string) (*mcp.Server, error) { return nil, errors.New("server") }
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
