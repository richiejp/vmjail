package sandboxexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/localai-org/darkaifactory/internal/devcontainer"
	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/localai-org/darkaifactory/internal/sandbox"
	"github.com/localai-org/darkaifactory/internal/sandboxboot"
)

// In-VM mount points + artefact names the in-sandbox image builder agrees
// on with the build script it generates: the devcontainer context is
// exported read-only at buildContextMount, and the build container writes
// the result tarball to buildOutMount, which the host reads back from the
// share's backing dir.
const (
	buildContextMount = "/build/ctx"
	buildOutMount     = "/build/out"
	buildOutTarball   = "image.tar"
	// buildLocalTag is buildah's in-storage tag for the freshly built
	// image; it never leaves the VM (the push is to a docker-archive).
	buildLocalTag = "daft-build"
	// defaultDockerfile is the devcontainer spec's default when `build`
	// names no dockerfile.
	defaultDockerfile = "Dockerfile"
)

// Build VM resource floors. A from-scratch devcontainer build copies each
// pulled layer onto the RAM-backed vfs store under /tmp, so the build VM
// needs materially more headroom than a typical workload boot — hence a
// floor rather than the workload default.
const (
	buildMinMemoryBytes uint64 = 4 << 30 // 4 GiB
	buildMinCPUs               = 2
)

// defaultBuildTimeout bounds a single in-sandbox build when the operator
// sets none. A build container that hangs (no BOOT_COMPLETE) would
// otherwise block the dispatch for the lifetime of its (long-lived)
// context; this is the backstop. Generous so a real multi-layer
// `FROM`-pull-plus-compile finishes well inside it.
const defaultBuildTimeout = 30 * time.Minute

// devcontainerBuildPlan is a resolved build-based devcontainer: absolute
// host paths plus the buildah inputs, ready to drive an in-sandbox build.
type devcontainerBuildPlan struct {
	contextDir string            // abs host path, exported read-only at buildContextMount
	dockerfile string            // path to the Dockerfile, relative to contextDir (slash form)
	args       map[string]string // --build-arg values (already ${…}-expanded)
	target     string            // optional --target stage
}

// resolveBuildPlan turns a resolved devcontainer BuildSpec into a plan with
// absolute host paths. Per the devcontainer spec both `context` (default
// ".") and `dockerfile` (default "Dockerfile") are relative to the dir
// holding devcontainer.json. root is the workspace-content mount backing
// dir the devcontainer file was found under; the context must stay within
// it so a hostile devcontainer.json can't bind arbitrary host paths into
// the build via `context: "../../.."`, and the Dockerfile must resolve
// inside the context so it's actually present on the exported share.
func resolveBuildPlan(devcontainerFile, root string, b *devcontainer.BuildSpec) (*devcontainerBuildPlan, error) {
	if b == nil {
		return nil, errors.New("sandboxexec: nil build spec")
	}
	devDir := filepath.Dir(devcontainerFile)

	buildContext := b.Context
	if buildContext == "" {
		buildContext = "."
	}
	contextDir := filepath.Clean(filepath.Join(devDir, filepath.FromSlash(buildContext)))

	// Resolve symlinks before confining the context to the workspace
	// content — and do it first, before anything else looks at the context:
	// the threat model includes a malicious third-party devcontainer.json,
	// which could aim `context` at a symlink escaping the workspace (a
	// lexical check alone would miss it, and the resolved dir is what
	// virtiofsd exports into the build). The content is already
	// materialised, so both paths exist on disk.
	realContext, err := filepath.EvalSymlinks(contextDir)
	if err != nil {
		return nil, fmt.Errorf("sandboxexec: resolve build context %q: %w", buildContext, err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("sandboxexec: resolve workspace root: %w", err)
	}
	if !withinRoot(realRoot, realContext) {
		return nil, fmt.Errorf("sandboxexec: build context %q escapes the workspace content", buildContext)
	}

	dockerfile := b.Dockerfile
	if dockerfile == "" {
		dockerfile = defaultDockerfile
	}
	// The dockerfile path is logical within the context tree (passed to
	// buildah's -f against the in-VM /build/ctx mount), so compute it
	// lexically against the context. It must stay inside the context.
	dockerfileAbs := filepath.Clean(filepath.Join(devDir, filepath.FromSlash(dockerfile)))
	rel, err := filepath.Rel(contextDir, dockerfileAbs)
	if err != nil || !withinRoot(contextDir, dockerfileAbs) {
		return nil, fmt.Errorf("sandboxexec: dockerfile %q resolves outside the build context %q", dockerfile, buildContext)
	}
	return &devcontainerBuildPlan{
		contextDir: realContext,
		dockerfile: filepath.ToSlash(rel),
		args:       b.Args,
		target:     b.Target,
	}, nil
}

// withinRoot reports whether path is root itself or nested under it (both
// cleaned absolute paths).
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// buildScript emits the shell the build container runs as its command
// override: a daemonless buildah build of the devcontainer Dockerfile,
// pushed to a docker-archive on the writable output mount. `set -e` makes
// a failed build exit non-zero and skip the push, so the host sees no
// tarball and reports the failure. The container then exits — the signal
// RunToCompletion waits on (BOOT_COMPLETE) — and the final `sync` flushes
// the tarball through virtiofs before that exit. vfs storage on the
// RAM-backed /tmp keeps the read-only rootfs untouched.
func buildScript(plan *devcontainerBuildPlan, egressCAPath string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("export HOME=/tmp\n")
	b.WriteString("mkdir -p /tmp/cstore /tmp/crun\n")
	b.WriteString("cd " + shQuote(buildContextMount) + "\n")
	b.WriteString("buildah --root /tmp/cstore --runroot /tmp/crun build")
	b.WriteString(" --isolation chroot --storage-driver vfs")
	if egressCAPath != "" {
		// Default-deny egress means RUN steps reach the network only through
		// the in-stack MITM proxy, so their TLS clients must trust its CA.
		// buildah already trusts it for the FROM pull (SSL_CERT_FILE on the
		// buildah process), but RUN steps run in the built image's rootfs
		// with its own env, so wire the CA in two ways:
		//   - mount it over the Debian/Ubuntu system trust store for the RUN
		//     duration (ephemeral, NOT baked into the image — a persisted
		//     file-path CA env would hard-fail TLS in a proxy-off workload).
		//     Covers apt / curl / openssl / Go (they read the system store).
		//   - Node ignores the system store, so point NODE_EXTRA_CA_CERTS at
		//     the same path. Safe to persist: Node only warns (never fails)
		//     when the file is absent, and post-build that path is the base
		//     image's real bundle. Scoped to Debian/Ubuntu bases (the
		//     devcontainer-features scope).
		const systemCABundle = "/etc/ssl/certs/ca-certificates.crt"
		b.WriteString(" --volume " + shQuote(egressCAPath+":"+systemCABundle+":ro"))
		b.WriteString(" --env NODE_EXTRA_CA_CERTS=" + systemCABundle)
	}
	if plan.target != "" {
		b.WriteString(" --target " + shQuote(plan.target))
	}
	for _, k := range sortedKeys(plan.args) {
		b.WriteString(" --build-arg " + shQuote(k+"="+plan.args[k]))
	}
	b.WriteString(" -t " + buildLocalTag)
	b.WriteString(" -f " + shQuote(plan.dockerfile) + " .\n")
	b.WriteString("buildah --root /tmp/cstore --runroot /tmp/crun push " + buildLocalTag +
		" docker-archive:" + shQuote(buildOutMount+"/"+buildOutTarball) + "\n")
	b.WriteString("sync\n")
	return b.String()
}

// shQuote single-quotes s for POSIX sh, escaping embedded single quotes,
// so a devcontainer-supplied build arg / target / path can't break out of
// the generated script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sortedKeys returns m's keys sorted, so the emitted --build-arg order is
// deterministic (stable scripts, testable output).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// buildDevcontainerImage runs an in-sandbox buildah build of a build-based
// devcontainer and returns the orchestrator-hosted ref of the built image,
// ready to boot the workload from. It boots an ephemeral build VM (the
// static build image, with the generated script as its command override),
// waits for the build container to exit (RunToCompletion → BOOT_COMPLETE),
// reads the docker-archive the build wrote onto the output mount, and
// pushes it into the orchestrator registry. A build that fails inside the
// sandbox produces no tarball, which surfaces here as an error.
func (e *Executor) buildDevcontainerImage(ctx context.Context, uid string, spec *sandboxv1.SandboxSpec, plan *devcontainerBuildPlan) (string, error) {
	if e.buildImageRef == "" {
		return "", errors.New("sandboxexec: build-based devcontainer needs a build image; set DAFT_BUILD_IMAGE_REF on the node")
	}
	outDir, err := os.MkdirTemp("", "daft-build-out-")
	if err != nil {
		return "", fmt.Errorf("sandboxexec: build output dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(outDir); rmErr != nil {
			_, _ = fmt.Fprintf(e.stdout, "sandboxexec: cleanup build output dir: %v\n", rmErr)
		}
	}()

	// When the egress proxy is active, applyEgressOpts below writes its CA
	// into the build container at egressCAGuestPath and routes egress through
	// the proxy; tell buildScript so its RUN steps trust the CA too. Gated on
	// the same egressActive() as applyEgressOpts so the two never disagree.
	egressCA := ""
	if e.egressActive() {
		egressCA = egressCAGuestPath
	}
	opts := sandboxboot.BootOptions{
		Args: []string{"/bin/dash", "-c", buildScript(plan, egressCA)},
		Mounts: []sandboxboot.Mount{
			{Target: buildContextMount, Source: plan.contextDir, ReadOnly: true},
			{Target: buildOutMount, Source: outDir},
		},
		PrivilegedBuild: true,
	}
	e.applyEgressOpts(&opts)

	cfg, unlock, err := e.assembler(ctx, e.buildImageRef, opts)
	if err != nil {
		return "", fmt.Errorf("sandboxexec: assemble build sandbox: %w", err)
	}
	defer unlock()
	applyBuildResourceSizing(&cfg, spec)
	// The build VM only needs the control channel (for BOOT_COMPLETE) and,
	// for a `FROM <registry>` base, the default-deny egress lane — never
	// attach or port ingress, so those stay off.
	e.applyVsockEgress(&cfg, uid+"-build", true, false)

	buildCtx := ctx
	if e.buildTimeout > 0 {
		var cancel context.CancelFunc
		buildCtx, cancel = context.WithTimeout(ctx, e.buildTimeout)
		defer cancel()
	}
	_, _ = fmt.Fprintf(e.stdout, "daft-sandboxd: building devcontainer image for %s (context %s)\n", uid, plan.contextDir)
	if err := e.buildRunner(buildCtx, e.stdout, cfg); err != nil {
		return "", fmt.Errorf("sandboxexec: build sandbox: %w", err)
	}

	tarPath := filepath.Join(outDir, buildOutTarball)
	if _, err := os.Stat(tarPath); err != nil {
		return "", fmt.Errorf("sandboxexec: build produced no image (buildah failed inside the sandbox): %w", err)
	}
	ref, err := e.pushImage(tarPath, buildRepo(spec.GetWorkspaceUuid()), buildTag(uid))
	if err != nil {
		return "", fmt.Errorf("sandboxexec: push built image: %w", err)
	}
	_, _ = fmt.Fprintf(e.stdout, "daft-sandboxd: built devcontainer image %s\n", ref)
	return ref, nil
}

// applyBuildResourceSizing sizes the build VM: the workspace's requested
// resources, floored at the build minimums (vfs layer copies are
// RAM-hungry). Independent of applyResourceSizing — the build is a
// separate, heavier VM than the workload it produces an image for.
func applyBuildResourceSizing(cfg *sandbox.Config, spec *sandboxv1.SandboxSpec) {
	cfg.Memory = memoryArgFromBytes(max(spec.GetMemoryBytes(), buildMinMemoryBytes))
	cfg.CPUs = max(int(spec.GetCpuCores()), buildMinCPUs)
}

// buildRepo / buildTag name the built image in the orchestrator registry.
// One repo per workspace keys cleanup; the per-launch sandbox uid as tag
// keeps concurrent / repeat builds from clobbering each other.
func buildRepo(workspaceUUID string) string {
	if workspaceUUID == "" {
		return "daft-build/anon"
	}
	return "daft-build/" + sanitizeRef(workspaceUUID)
}

func buildTag(uid string) string {
	if uid == "" {
		return "build"
	}
	return sanitizeRef(uid)
}

// sanitizeRef lowercases s and replaces any byte outside the OCI
// repository/tag grammar ([a-z0-9._-]) with '-', so a workspace UUID or
// sandbox uid is always a legal ref component.
func sanitizeRef(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
