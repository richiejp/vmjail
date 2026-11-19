package sandboxexec

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/devcontainer"
	sandboxv1 "github.com/richiejp/vmjail/internal/genpb/sandbox/v1"
	"github.com/richiejp/vmjail/internal/sandbox"
	"github.com/richiejp/vmjail/internal/sandboxboot"
)

var (
	errAssemble = errors.New("assemble boom")
	errBuildVM  = errors.New("build vm boom")
	errPush     = errors.New("push boom")
)

// stubTransport is a non-nil http.RoundTripper satisfying New()'s
// BuildImageRef⇒BuildPushTransport pairing in tests that override
// e.pushImage (so the transport is never actually used).
type stubTransport struct{}

func (stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("stubTransport: unused")
}

func TestResolveBuildPlan(t *testing.T) {
	root := t.TempDir()
	// resolveBuildPlan EvalSymlinks the context, so the dirs must exist.
	if err := os.MkdirAll(filepath.Join(root, ".devcontainer"), 0o750); err != nil {
		t.Fatal(err)
	}
	devFile := filepath.Join(root, ".devcontainer", "devcontainer.json")

	tests := []struct {
		name       string
		build      *devcontainer.BuildSpec
		wantCtx    string // relative to root
		wantDocker string
		wantErr    bool
	}{
		{
			name:       "defaults: context . dockerfile Dockerfile",
			build:      &devcontainer.BuildSpec{},
			wantCtx:    ".devcontainer",
			wantDocker: defaultDockerfile,
		},
		{
			name:       "context one level up keeps dockerfile within it",
			build:      &devcontainer.BuildSpec{Context: "..", Dockerfile: defaultDockerfile},
			wantCtx:    ".",
			wantDocker: ".devcontainer/Dockerfile",
		},
		{
			name:       "explicit dockerfile under context",
			build:      &devcontainer.BuildSpec{Context: ".", Dockerfile: "build/Containerfile"},
			wantCtx:    ".devcontainer",
			wantDocker: "build/Containerfile",
		},
		{
			name:    "context escaping the workspace root is rejected",
			build:   &devcontainer.BuildSpec{Context: "../../.."},
			wantErr: true,
		},
		{
			name:    "dockerfile outside the context is rejected",
			build:   &devcontainer.BuildSpec{Context: ".", Dockerfile: "../Dockerfile"},
			wantErr: true,
		},
		{
			name:    "non-existent context is rejected",
			build:   &devcontainer.BuildSpec{Context: "missing-subdir"},
			wantErr: true,
		},
		{
			name:    "nil build spec is rejected",
			build:   nil,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := resolveBuildPlan(devFile, root, tc.build)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got plan %+v", plan)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBuildPlan: %v", err)
			}
			// resolveBuildPlan returns the symlink-resolved context, so
			// compare against the resolved expected path.
			wantCtx, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(tc.wantCtx)))
			if err != nil {
				t.Fatalf("EvalSymlinks want: %v", err)
			}
			if plan.contextDir != wantCtx {
				t.Errorf("contextDir = %q, want %q", plan.contextDir, wantCtx)
			}
			if plan.dockerfile != tc.wantDocker {
				t.Errorf("dockerfile = %q, want %q", plan.dockerfile, tc.wantDocker)
			}
		})
	}
}

// TestResolveBuildPlanSymlinkEscape pins the security fix: a context that
// is a symlink escaping the workspace content is rejected even though it's
// lexically inside the root (the threat model includes hostile configs).
func TestResolveBuildPlanSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, ".devcontainer")
	if err := os.MkdirAll(devDir, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() // a sibling tree, not under root
	if err := os.Symlink(outside, filepath.Join(devDir, "escape")); err != nil {
		t.Fatal(err)
	}
	devFile := filepath.Join(devDir, "devcontainer.json")
	_, err := resolveBuildPlan(devFile, root, &devcontainer.BuildSpec{Context: "escape"})
	if err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("symlink-escaping context not rejected: %v", err)
	}
}

func TestBuildScript(t *testing.T) {
	got := buildScript(&devcontainerBuildPlan{
		contextDir: "/host/ctx",
		dockerfile: defaultDockerfile,
		args:       map[string]string{"B": "2", "A": "1"},
		target:     "dev",
	}, "")
	want := "set -e\n" +
		"export HOME=/tmp\n" +
		"mkdir -p /tmp/cstore /tmp/crun\n" +
		"cd '/build/ctx'\n" +
		"buildah --root /tmp/cstore --runroot /tmp/crun build --isolation chroot --storage-driver vfs" +
		" --target 'dev' --build-arg 'A=1' --build-arg 'B=2' -t vmjail-build -f 'Dockerfile' .\n" +
		"buildah --root /tmp/cstore --runroot /tmp/crun push vmjail-build docker-archive:'/build/out/image.tar'\n" +
		"sync\n"
	if got != want {
		t.Errorf("buildScript mismatch\n got: %q\nwant: %q", got, want)
	}

	// No target, no args: those flags vanish entirely.
	bare := buildScript(&devcontainerBuildPlan{dockerfile: "Containerfile"}, "")
	if strings.Contains(bare, "--target") || strings.Contains(bare, "--build-arg") {
		t.Errorf("bare script should carry no target/build-arg flags: %q", bare)
	}
	if !strings.Contains(bare, "-f 'Containerfile' .") {
		t.Errorf("bare script missing -f Containerfile: %q", bare)
	}
	// Without an egress CA, no trust-store wiring leaks in.
	if strings.Contains(bare, "--volume") || strings.Contains(bare, "NODE_EXTRA_CA_CERTS") {
		t.Errorf("no-egress script should carry no CA wiring: %q", bare)
	}
}

// TestBuildScriptEgressCA covers the default-deny-egress path: the proxy CA
// is mounted over the system trust store for RUN steps (ephemeral) and
// NODE_EXTRA_CA_CERTS points at it, so RUN-step TLS through the MITM proxy
// is trusted without baking a brittle file-path CA env into the image.
func TestBuildScriptEgressCA(t *testing.T) {
	got := buildScript(&devcontainerBuildPlan{dockerfile: defaultDockerfile}, "/etc/vmjail-egress-ca.pem")
	const sysBundle = "/etc/ssl/certs/ca-certificates.crt"
	if !strings.Contains(got, "--volume '/etc/vmjail-egress-ca.pem:"+sysBundle+":ro'") {
		t.Errorf("egress script missing CA volume mount: %q", got)
	}
	if !strings.Contains(got, "--env NODE_EXTRA_CA_CERTS="+sysBundle) {
		t.Errorf("egress script missing NODE_EXTRA_CA_CERTS: %q", got)
	}
	// The CA wiring must precede the positional context/dockerfile args.
	if strings.Index(got, "--volume") > strings.Index(got, "-f ") {
		t.Errorf("CA volume must come before -f: %q", got)
	}
}

func TestShQuote(t *testing.T) {
	// An embedded single quote must be escaped so it can't break out.
	if got := shQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shQuote = %q", got)
	}
	// A would-be injection stays inside the quotes.
	if got := shQuote("; rm -rf /"); got != "'; rm -rf /'" {
		t.Errorf("shQuote = %q", got)
	}
}

func TestSanitizeRefAndNames(t *testing.T) {
	if got := sanitizeRef("Foo/Bar:Baz Qux"); got != "foo-bar-baz-qux" {
		t.Errorf("sanitizeRef = %q", got)
	}
	if got := sanitizeRef(""); got != "x" {
		t.Errorf("sanitizeRef empty = %q, want x", got)
	}
	if got := buildRepo(""); got != "vmjail-build/anon" {
		t.Errorf("buildRepo empty = %q", got)
	}
	if got := buildRepo("WS-123"); got != "vmjail-build/ws-123" {
		t.Errorf("buildRepo = %q", got)
	}
	if got := buildTag(""); got != "build" {
		t.Errorf("buildTag empty = %q", got)
	}
	if got := buildTag("UID-9"); got != "uid-9" {
		t.Errorf("buildTag = %q", got)
	}
}

func TestApplyBuildResourceSizing(t *testing.T) {
	// Below the floor → floored.
	var cfg sandbox.Config
	applyBuildResourceSizing(&cfg, &sandboxv1.SandboxSpec{})
	if cfg.CPUs != buildMinCPUs {
		t.Errorf("CPUs = %d, want floor %d", cfg.CPUs, buildMinCPUs)
	}
	if cfg.Memory != memoryArgFromBytes(buildMinMemoryBytes) {
		t.Errorf("Memory = %q, want floor %q", cfg.Memory, memoryArgFromBytes(buildMinMemoryBytes))
	}
	// Above the floor → the workspace's own request wins.
	cfg = sandbox.Config{}
	applyBuildResourceSizing(&cfg, &sandboxv1.SandboxSpec{MemoryBytes: 8 << 30, CpuCores: 6})
	if cfg.CPUs != 6 {
		t.Errorf("CPUs = %d, want 6", cfg.CPUs)
	}
	if cfg.Memory != memoryArgFromBytes(8<<30) {
		t.Errorf("Memory = %q", cfg.Memory)
	}
}

// shareAssembler is a fake Assembler that mirrors BuildBundleConfig's
// share layout (one share per mount) so a fake build runner can find the
// output mount's host backing dir.
func shareAssembler(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
	shares := make([]*sandbox.ShareConfig, 0, len(opts.Mounts))
	for _, m := range opts.Mounts {
		shares = append(shares, &sandbox.ShareConfig{HostDir: m.Source, Mountpoint: m.Target})
	}
	return sandbox.Config{Shares: shares}, func() {}, nil
}

// outHostDir finds the host backing dir of the build output mount on cfg.
func outHostDir(cfg sandbox.Config) string {
	for _, s := range cfg.Shares {
		if s.Mountpoint == buildOutMount {
			return s.HostDir
		}
	}
	return ""
}

func TestBuildDevcontainerImageHappyPath(t *testing.T) {
	ctxDir := t.TempDir()
	plan := &devcontainerBuildPlan{contextDir: ctxDir, dockerfile: defaultDockerfile}

	// Fake build VM: write the docker-archive the host then reads back.
	var ranBuild bool
	buildRunner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		ranBuild = true
		out := outHostDir(cfg)
		if out == "" {
			t.Error("build cfg carries no output share")
			return nil
		}
		return os.WriteFile(filepath.Join(out, buildOutTarball), []byte("fake-tar"), 0o644)
	}

	e, err := New(Config{
		Stdout:             io.Discard,
		Assembler:          shareAssembler,
		Runner:             noopRunner(),
		BuildImageRef:      testBuildImageRef,
		BuildPushTransport: stubTransport{},
		BuildRunner:        buildRunner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var gotTarDir, gotRepo, gotTag, gotContents string
	e.pushImage = func(tarPath, repo, tag string) (string, error) {
		// Read while the temp dir still exists — buildDevcontainerImage
		// removes it on return.
		data, _ := os.ReadFile(tarPath) //nolint:gosec // test reads the temp tarball it just wrote.
		gotTarDir, gotRepo, gotTag, gotContents = filepath.Dir(tarPath), repo, tag, string(data)
		return "vmjail-registry.internal/" + repo + ":" + tag, nil
	}

	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-7"}
	ref, err := e.buildDevcontainerImage(context.Background(), "uid-2", spec, plan)
	if err != nil {
		t.Fatalf("buildDevcontainerImage: %v", err)
	}
	if !ranBuild {
		t.Error("build runner was not invoked")
	}
	if want := "vmjail-registry.internal/vmjail-build/ws-7:uid-2"; ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	if gotRepo != "vmjail-build/ws-7" || gotTag != "uid-2" {
		t.Errorf("push repo/tag = %q/%q", gotRepo, gotTag)
	}
	if gotContents != "fake-tar" {
		t.Errorf("pushed tar contents = %q", gotContents)
	}
	// The output dir is temporary and cleaned up after the build.
	if _, err := os.Stat(gotTarDir); !os.IsNotExist(err) {
		t.Errorf("build output dir not cleaned up: %v", err)
	}
}

// TestHandleSandboxEventBuildDevcontainer drives the whole 3c path: a
// build-based devcontainer is materialised, built in an ephemeral sandbox
// (the build runner writes the output tarball), pushed, and the workload
// boots from the resulting ref — not from ErrBuildUnsupported.
func TestHandleSandboxEventBuildDevcontainer(t *testing.T) {
	source := seedWorkspace(t, func(dest string) error {
		dir := filepath.Join(dest, ".devcontainer")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "devcontainer.json"),
			[]byte(`{"build":{"dockerfile":"Dockerfile"}}`), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, defaultDockerfile), []byte("FROM scratch\n"), 0o644)
	})

	// One assembler serves both boots; record every ref it bundles and
	// mirror the share layout so the build runner can find its output mount.
	var refs []string
	asm := func(ctx context.Context, ref string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		refs = append(refs, ref)
		return shareAssembler(ctx, ref, opts)
	}
	buildRunner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		return os.WriteFile(filepath.Join(outHostDir(cfg), buildOutTarball), []byte("img"), 0o644)
	}

	e, err := New(Config{
		Stdout: io.Discard, Runner: noopRunner(), Assembler: asm,

		BuildImageRef:      testBuildImageRef,
		BuildPushTransport: stubTransport{},
		BuildRunner:        buildRunner,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.pushImage = func(_, repo, tag string) (string, error) {
		return "vmjail-registry.internal/" + repo + ":" + tag, nil
	}

	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid:    testWSUUID,
		DevcontainerPath: devcontainerNestedPath,
		Mounts:           []*sandboxv1.Mount{{Target: mntWorkspace, Source: source}},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-build", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("assembler called %d times (%v), want 2 (build + workload)", len(refs), refs)
	}
	if refs[0] != testBuildImageRef {
		t.Errorf("build boot ref = %q, want the build image", refs[0])
	}
	wantWorkload := "vmjail-registry.internal/vmjail-build/" + sanitizeRef(testWSUUID) + ":sb-build"
	if refs[1] != wantWorkload {
		t.Errorf("workload boot ref = %q, want the built image %q", refs[1], wantWorkload)
	}
}

// TestExplicitImageOverridesBuild pins the overlay precedence: a workspace
// image_ref wins over a build-based devcontainer, so no build runs.
func TestExplicitImageOverridesBuild(t *testing.T) {
	source := seedWorkspace(t, func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "devcontainer.json"),
			[]byte(`{"build":{"dockerfile":"Dockerfile"}}`), 0o644)
	})
	var gotRef string
	asm := func(_ context.Context, ref string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		gotRef = ref
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{
		Stdout: io.Discard, Runner: noopRunner(), Assembler: asm,

		BuildImageRef:      testBuildImageRef,
		BuildPushTransport: stubTransport{},
		BuildRunner: func(context.Context, io.Writer, sandbox.Config) error {
			t.Error("build runner must not run when the workspace overrides the image")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid:    testWSUUID,
		DevcontainerPath: devcontainerFlatPath,
		ImageRef:         "ghcr.io/acme/override:9",
		Mounts:           []*sandboxv1.Mount{{Target: mntWorkspace, Source: source}},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-ovr", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if gotRef != "ghcr.io/acme/override:9" {
		t.Errorf("assembler ref = %q, want the explicit override", gotRef)
	}
}

func TestNewRejectsBuildImageWithoutTransport(t *testing.T) {
	_, err := New(Config{Stdout: io.Discard, Runner: noopRunner(), BuildImageRef: testBuildImageRef})
	if err == nil || !strings.Contains(err.Error(), "BuildPushTransport") {
		t.Fatalf("want BuildPushTransport error, got %v", err)
	}
}

func TestBuildDevcontainerImageNoBuildImage(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Assembler: shareAssembler, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = e.buildDevcontainerImage(context.Background(), "uid", &sandboxv1.SandboxSpec{},
		&devcontainerBuildPlan{contextDir: t.TempDir(), dockerfile: defaultDockerfile})
	if err == nil || !strings.Contains(err.Error(), "build image") {
		t.Errorf("want build-image error, got %v", err)
	}
}

func TestBuildDevcontainerImageErrorPaths(t *testing.T) {
	okAsm := shareAssembler
	writeTarRunner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		return os.WriteFile(filepath.Join(outHostDir(cfg), buildOutTarball), []byte("x"), 0o644)
	}
	tests := []struct {
		name    string
		asm     Assembler
		runner  Runner
		push    func(string, string, string) (string, error)
		timeout time.Duration
		wantErr string
	}{
		{
			name: "assembler failure",
			asm: func(context.Context, string, sandboxboot.BootOptions) (sandbox.Config, func(), error) {
				return sandbox.Config{}, func() {}, errAssemble
			},
			runner:  writeTarRunner,
			wantErr: "assemble build sandbox",
		},
		{
			name:    "build runner failure",
			asm:     okAsm,
			runner:  func(context.Context, io.Writer, sandbox.Config) error { return errBuildVM },
			wantErr: "build sandbox",
		},
		{
			name:    "push failure",
			asm:     okAsm,
			runner:  writeTarRunner,
			push:    func(string, string, string) (string, error) { return "", errPush },
			wantErr: "push built image",
		},
		{
			name:    "timeout still builds",
			asm:     okAsm,
			runner:  writeTarRunner,
			timeout: time.Hour,
			wantErr: "", // no error: the deadline is generous
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := New(Config{
				Stdout: io.Discard, Runner: noopRunner(), Assembler: tc.asm,
				BuildImageRef:      testBuildImageRef,
				BuildPushTransport: stubTransport{},
				BuildRunner:        tc.runner, BuildTimeout: tc.timeout,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			e.pushImage = func(tarPath, repo, tag string) (string, error) {
				if tc.push != nil {
					return tc.push(tarPath, repo, tag)
				}
				return "vmjail-registry.internal/" + repo + ":" + tag, nil
			}
			_, err = e.buildDevcontainerImage(context.Background(), "uid", &sandboxv1.SandboxSpec{},
				&devcontainerBuildPlan{contextDir: t.TempDir(), dockerfile: defaultDockerfile})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildDevcontainerImageNoTarball(t *testing.T) {
	// A build runner that produces no tarball stands in for a buildah
	// failure inside the sandbox; the missing output surfaces as an error.
	e, err := New(Config{
		Stdout:             io.Discard,
		Assembler:          shareAssembler,
		Runner:             noopRunner(),
		BuildImageRef:      testBuildImageRef,
		BuildPushTransport: stubTransport{},
		BuildRunner:        func(context.Context, io.Writer, sandbox.Config) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = e.buildDevcontainerImage(context.Background(), "uid", &sandboxv1.SandboxSpec{},
		&devcontainerBuildPlan{contextDir: t.TempDir(), dockerfile: defaultDockerfile})
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("want no-image error, got %v", err)
	}
}
