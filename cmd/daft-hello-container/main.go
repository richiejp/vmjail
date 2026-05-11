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
// The agent's banner-scanner on the host watches for the banner
// string (see internal/agent.ContainerBanner), so don't change it
// without updating that constant.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/richiejp/darkaifactory/internal/agent"
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

var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
	// fsRead / fsWrite are vars so tests can stub the file-system
	// boundary without needing a real /workspace.
	fsRead  = os.ReadFile
	fsWrite = os.WriteFile
)

func main() {
	osExit(run(stdout))
}

func run(out io.Writer) int {
	if code := workspaceRoundTrip(out); code != 0 {
		return code
	}
	if _, err := fmt.Fprintln(out, agent.ContainerBanner); err != nil {
		return 1
	}
	return 0
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
