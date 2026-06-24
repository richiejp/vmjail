package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	"github.com/localai-org/darkaifactory/internal/devcontainer"
	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// devcontainerDirName is the conventional folder that holds a
// devcontainer.json (`.devcontainer/devcontainer.json`). When the json
// lives there, the project root the workload sees is its parent.
const devcontainerDirName = ".devcontainer"

// devcontainerConventionPaths returns the locations the Dev Containers
// spec searches for a devcontainer.json, relative to the workspace content
// root, in precedence order: the `.devcontainer/` subdir first, then the
// root-level `.devcontainer.json` variant.
func devcontainerConventionPaths() []string {
	return []string{
		filepath.Join(devcontainerDirName, "devcontainer.json"),
		".devcontainer.json",
	}
}

// resolveDevcontainerArg turns the command's positional args plus the
// --clear flag into the devcontainer path to record: "" when clearing,
// the explicit arg when one is given, otherwise the auto-detected
// convention path. --clear alongside a positional arg is a usage error,
// as is the no-arg case when no conventional devcontainer.json exists.
func resolveDevcontainerArg(args []string, clearPath bool) (string, error) {
	if clearPath {
		if len(args) > 0 {
			return "", fmt.Errorf("daft workspace devcontainer: --clear takes no path argument")
		}
		return "", nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	return autodetectDevcontainerPath()
}

// autodetectDevcontainerPath returns the first of the conventional
// devcontainer.json locations that exists relative to the current
// directory (the workspace content root), or an error naming the searched
// paths when none is present.
func autodetectDevcontainerPath() (string, error) {
	paths := devcontainerConventionPaths()
	for _, rel := range paths {
		if fi, err := os.Stat(rel); err == nil && !fi.IsDir() {
			return rel, nil
		}
	}
	return "", fmt.Errorf(
		"daft workspace devcontainer: no devcontainer.json found in the conventional locations (%s); pass an explicit path or use --clear",
		strings.Join(paths, ", "),
	)
}

// devcontainerWorkspaceMount is the workspace-content mount a
// devcontainer implies: the host project root shared into the guest at
// the container's workspace folder. Source is left to the push-CAS
// default (the workspace UUID), so it syncs via push/pull like any
// daft workspace mount — i.e. we follow the devcontainer convention for
// the *path*, not the live bind.
type devcontainerWorkspaceMount struct {
	// Local is the host project root (the devcontainer's
	// ${localWorkspaceFolder}); the dir push/pull syncs from.
	Local string
	// Remote is the in-VM workspace folder (the container path).
	Remote string
}

// deriveDevcontainerWorkspaceMount reads the devcontainer.json at the
// host-relative relPath and works out the workspace-content mount it
// implies, following the Dev Containers convention:
//
//   - host root = ${localWorkspaceFolder}: the project being opened.
//     When the json sits in a `.devcontainer/` dir, that's its parent;
//     otherwise it's the json's own directory.
//   - container folder: explicit workspaceFolder wins, else an explicit
//     workspaceMount target, else the VS Code default
//     /workspaces/<basename-of-root>.
//
// An empty relPath (clearing the devcontainer) yields ok=false. A read
// or parse failure is returned as an error for the caller to downgrade
// to a warning — setting the devcontainer path itself must not depend
// on the host json being locally readable.
func deriveDevcontainerWorkspaceMount(relPath string) (m devcontainerWorkspaceMount, ok bool, err error) {
	if relPath == "" {
		return devcontainerWorkspaceMount{}, false, nil
	}
	abs, err := filepath.Abs(relPath)
	if err != nil {
		return devcontainerWorkspaceMount{}, false, fmt.Errorf("resolve devcontainer path: %w", err)
	}
	// #nosec G304 — abs is a path the user named on their own machine.
	data, err := os.ReadFile(abs)
	if err != nil {
		return devcontainerWorkspaceMount{}, false, fmt.Errorf("read devcontainer %s: %w", abs, err)
	}
	root := localWorkspaceFolderFor(abs)
	spec, err := devcontainer.Resolve(data, devcontainer.Substitutions{LocalWorkspaceFolder: root})
	if err != nil {
		return devcontainerWorkspaceMount{}, false, fmt.Errorf("parse devcontainer %s: %w", abs, err)
	}
	return devcontainerWorkspaceMount{
		Local:  root,
		Remote: containerWorkspaceFolder(spec, root),
	}, true, nil
}

// localWorkspaceFolderFor maps a devcontainer.json host path to the
// project root the workload opens: the parent of a `.devcontainer/`
// dir, else the json's own directory.
func localWorkspaceFolderFor(absJSONPath string) string {
	dir := filepath.Dir(absJSONPath)
	if filepath.Base(dir) == devcontainerDirName {
		return filepath.Dir(dir)
	}
	return dir
}

// containerWorkspaceFolder picks the in-VM workspace folder following
// the Dev Containers precedence: explicit workspaceFolder, then an
// explicit workspaceMount target, then the /workspaces/<basename>
// default.
func containerWorkspaceFolder(spec *devcontainer.Spec, root string) string {
	if spec.Workspace.Folder != "" {
		return spec.Workspace.Folder
	}
	if spec.Workspace.Mount != nil && spec.Workspace.Mount.Target != "" {
		return spec.Workspace.Mount.Target
	}
	return "/workspaces/" + filepath.Base(root)
}

// registerDevcontainerWorkspaceMount records the devcontainer-implied
// workspace mount through the same path a manual `daft workspace mount`
// uses: AddMount on the catalog (empty Source → the workspace UUID,
// push-CAS) plus the dir-local mapping push/pull reads. The point is to
// avoid a separate manual mount step without special-casing
// devcontainers anywhere below this layer — the boot path sees an
// ordinary registered mount.
//
// It is best-effort and idempotent: an already-present target (a repeat
// run, or a mount the user added by hand) is treated as success, and
// addDirLocalMount upserts by target. Failures are reported to stderr
// but never roll back the devcontainer path that was just set.
func registerDevcontainerWorkspaceMount(
	ctx context.Context,
	c workspacev1connect.WorkspaceCatalogServiceClient,
	h *workspacev1.WorkspaceHandle,
	out io.Writer, m devcontainerWorkspaceMount,
) {
	_, err := c.AddMount(ctx, connect.NewRequest(&workspacev1.AddMountRequest{
		Handle: h,
		Mount:  &sandboxv1.Mount{Target: m.Remote},
	}))
	switch {
	case err == nil, connect.CodeOf(err) == connect.CodeAlreadyExists:
		// Registered now, or already there — either way the mount stands.
	default:
		_, _ = fmt.Fprintf(out, "daft workspace devcontainer: register workspace mount %s: %v\n", m.Remote, err)
		return
	}
	if _, err := addDirLocalMount(mountMapping{Local: m.Local, Remote: m.Remote}); err != nil {
		_, _ = fmt.Fprintf(out, "daft workspace devcontainer: record workspace mount dir-local: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "devcontainer workspace mount %s -> %s registered\n", m.Local, m.Remote)
}
