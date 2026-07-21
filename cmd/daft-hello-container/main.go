// daft-hello-container is the workload binary in the v0 container
// bundle. It prints a single line and exits 0 — that's the smoke
// test for "we got a container running inside the sandbox VM".
//
// If /workspace exists (because daft-sandboxd run was given
// --workspace / --workspace-from), the binary also acts as the
// in-VM peer for the workspace round-trip e2e test: it reads
// /workspace/in.txt, appends a marker, and writes the result to
// /workspace/out.txt before printing the banner. Tests on the host
// then assert that out.txt is present and modified — proving that
// a real container in a real VM can write to the virtiofs share
// and the host sees the result.
//
// With --keepalive the binary prints the banner and then blocks
// until SIGTERM/SIGINT. This makes the same binary usable as the
// long-running PID 1 of a container that the D2 attach e2e test
// can `crun exec` into.
//
// The agent's banner-scanner on the host watches for the banner
// string (see internal/agent.ContainerBanner), so don't change it
// without updating that constant.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/agentcredential"
)

// writeErrf forwards a diagnostic to out, swallowing the (rare)
// case where out itself errors. Used inside workspaceRoundTrip
// where the caller already has a return code in flight.
func writeErrf(out io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(out, format, args...); err != nil {
		// stdout is closed — there's nowhere left to surface this.
		_ = err
	}
}

// keepaliveFlag enables long-running PID-1 mode.
const (
	keepaliveFlag = "--keepalive"
	proofFlag     = "--credential-proof"
	proofOK       = "codex credential available"
)

//nolint:gochecknoglobals // test seams: tests stub these to avoid blocking on real signals or touching the real fs.
var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
	// fsRead / fsWrite are vars so tests can stub the file-system
	// boundary without needing a real /workspace.
	fsRead  = os.ReadFile
	fsWrite = os.WriteFile
	fsStat  = os.Stat
	getenv  = os.Getenv
	// block waits for a SIGTERM/SIGINT and returns. Stubbed in
	// tests so they don't actually pin the test process.
	block = blockUntilTerminationSignal
)

func main() {
	osExit(run(
		stdout,
		slices.Contains(os.Args[1:], keepaliveFlag),
		slices.Contains(os.Args[1:], proofFlag),
	))
}

func run(out io.Writer, keepalive, credentialProof bool) int {
	if credentialProof {
		return proveCredential(out)
	}
	if code := workspaceRoundTrip(out); code != 0 {
		return code
	}
	if _, err := fmt.Fprintln(out, agent.ContainerBanner); err != nil {
		return 1
	}
	if keepalive {
		block()
	}
	return 0
}

// proveCredential is a test-workload mode used by the real-VM credential
// delivery proof. It verifies the fixed CODEX_HOME contract and a non-empty
// provider-native token, but emits only a constant success sentinel: secret
// bytes and derived hashes must never enter the detached job spool.
func proveCredential(out io.Writer) int {
	if getenv("CODEX_HOME") != agentcredential.CodexHome {
		writeErrf(out, "credential proof error: CODEX_HOME is not the fixed guest path\n")
		return 3
	}
	authPath := agentcredential.CodexHome + "/auth.json"
	info, err := fsStat(authPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		writeErrf(out, "credential proof error: auth store permissions invalid\n")
		return 3
	}
	payload, err := fsRead(authPath)
	if err != nil {
		writeErrf(out, "credential proof error: auth store unavailable\n")
		return 3
	}
	defer clear(payload)
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(payload, &auth); err != nil || auth.Tokens.AccessToken == "" {
		writeErrf(out, "credential proof error: provider-native token unavailable\n")
		return 3
	}
	if _, err := fmt.Fprintln(out, proofOK); err != nil {
		return 1
	}
	return 0
}

// blockUntilTerminationSignal waits until SIGTERM or SIGINT
// arrives. Used as the PID-1 hold so the container stays alive
// for `crun exec` attach sessions; teardown comes from the VM
// shutting down (crun kill → SIGTERM). signal.Stop is omitted
// because the process exits immediately after the channel fires.
func blockUntilTerminationSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}

// workspaceRoundTrip is the optional in-VM half of the workspace
// e2e check. Returns 0 if /workspace/in.txt doesn't exist (the
// no-share path — every existing test exercises this), or after a
// successful append+write, or 2 if a real read/write error fired.
func workspaceRoundTrip(out io.Writer) int {
	content, err := fsRead(agent.WorkspaceInPath)
	switch {
	case err == nil:
		// fall through
	case errors.Is(err, fs.ErrNotExist):
		return 0
	default:
		writeErrf(out, "workspace round-trip error: read %s: %v\n",
			agent.WorkspaceInPath, err)
		return 2
	}
	modified := slices.Concat(content, []byte(agent.WorkspaceSuffix))
	if err := fsWrite(agent.WorkspaceOutPath, modified, 0o600); err != nil {
		writeErrf(out, "workspace round-trip error: write %s: %v\n",
			agent.WorkspaceOutPath, err)
		return 2
	}
	return 0
}
