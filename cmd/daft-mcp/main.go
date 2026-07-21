// Command daft-mcp exposes daft's bounded overseer tools over MCP stdio.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/agentcredential"
	"github.com/richiejp/darkaifactory/internal/agentprofile"
	"github.com/richiejp/darkaifactory/internal/daftclient"
	"github.com/richiejp/darkaifactory/internal/mcpserver"
	"github.com/richiejp/darkaifactory/internal/version"
)

var (
	osExit     = os.Exit
	openClient = daftclient.Open
	newReader  = mcpserver.NewConnectReaderWithCredentials
	newServer  = mcpserver.NewWithProfiles
	runServer  = runMCPServer
)

type nopCloserWriter struct{ io.Writer }

func (nopCloserWriter) Close() error { return nil }

func runMCPServer(ctx context.Context, server *mcp.Server, stdin io.Reader, stdout io.Writer) error {
	return server.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopCloserWriter{stdout}})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	osExit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("daft-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	orchestrator := flags.String("orchestrator", "", "orchestrator URL (overrides project and local-cluster discovery)")
	configDir := flags.String("config-dir", "", "daft client identity directory")
	projectDir := flags.String("project-dir", ".", "project directory used for .daft discovery")
	agentProfiles := flags.String("agent-profiles", "", "trusted-host AgentProfile JSON file")
	agentCredentials := flags.String("agent-credentials", "", "trusted-host agent credential catalog JSON file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		if _, err := fmt.Fprintf(stderr, "daft-mcp: unexpected arguments: %v\n", flags.Args()); err != nil {
			return 2
		}
		return 2
	}
	client, err := openClient(daftclient.Config{Orchestrator: *orchestrator, ConfigDir: *configDir, ProjectDir: *projectDir})
	if err != nil {
		return reportError(stderr, err)
	}
	defer client.Close()
	var profiles []agentprofile.Entry
	if *agentProfiles != "" {
		catalog, loadErr := agentprofile.Load(*agentProfiles)
		if loadErr != nil {
			return reportError(stderr, loadErr)
		}
		profiles = catalog.List()
	}
	var credentials *agentcredential.Catalog
	if *agentCredentials != "" {
		credentials, err = agentcredential.Load(*agentCredentials)
		if err != nil {
			return reportError(stderr, err)
		}
		defer credentials.Destroy()
	}
	if err := validateCredentialBindings(profiles, credentials); err != nil {
		return reportError(stderr, err)
	}
	reader, err := newReader(client, credentials)
	if err != nil {
		return reportError(stderr, err)
	}
	server, err := newServer(reader, profiles, version.Version)
	if err != nil {
		return reportError(stderr, err)
	}
	if err := runServer(ctx, server, stdin, stdout); err != nil {
		return reportError(stderr, err)
	}
	return 0
}

func validateCredentialBindings(profiles []agentprofile.Entry, credentials *agentcredential.Catalog) error {
	for i := range profiles {
		profile := &profiles[i]
		if !profile.Profile.Credentials.Enable {
			continue
		}
		if credentials == nil {
			return fmt.Errorf("daft-mcp: AgentProfile %q requires --agent-credentials", profile.Profile.Name)
		}
		entry, ok := credentials.Get(profile.Profile.Credentials.Profile)
		if !ok {
			return fmt.Errorf("daft-mcp: AgentProfile %q references unknown credential %q", profile.Profile.Name, profile.Profile.Credentials.Profile)
		}
		if string(entry.Provider) != string(profile.Profile.Provider) {
			return fmt.Errorf("daft-mcp: AgentProfile %q provider does not match credential %q", profile.Profile.Name, entry.Name)
		}
	}
	return nil
}

func reportError(stderr io.Writer, err error) int {
	if _, writeErr := fmt.Fprintln(stderr, "daft-mcp:", err); writeErr != nil {
		return 2
	}
	return 1
}
