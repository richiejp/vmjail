package sandboxexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/sandboxboot"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/sandboxnet"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"github.com/richiejp/darkaifactory/internal/workspacedisk"
)

const (
	testImageRef  = "alpine:3"
	testSandboxID = "sb-1"
	testVsockDir  = "/var/run/daft-sandboxd"
	mntWorkspace  = "/workspace"
	mntData       = "/data"
	mntReference  = "/reference"
	testWSUUID    = "ws-uuid"
	hostWS        = "/host/ws"

	devcontainerNestedPath = ".devcontainer/devcontainer.json"
	devcontainerFlatPath   = "devcontainer.json"
	testBuildImageRef      = "daft-registry.internal/builder:v1"
)

func TestProductionAssemblerReturnsConfiguredAssembler(t *testing.T) {
	if assembler := ProductionAssembler(sandboxboot.Deps{}); assembler == nil {
		t.Fatal("ProductionAssembler returned nil")
	}
}

func noopRunner() Runner {
	return func(context.Context, io.Writer, sandbox.Config) error { return nil }
}

// addedEvent wraps a SandboxSpec in a freshly-minted SandboxWatchEvent
// of type ADDED for tests that exercise the executor's ADDED path.
func addedEvent(uid string, spec *sandboxv1.SandboxSpec) *nodectrlv1.SandboxWatchEvent {
	return &nodectrlv1.SandboxWatchEvent{
		Type: sandboxv1.EventType_EVENT_TYPE_ADDED,
		Sandbox: &sandboxv1.Sandbox{
			Metadata: &sandboxv1.SandboxMetadata{Uid: uid},
			Spec:     spec,
		},
	}
}

func deletedEvent(uid string) *nodectrlv1.SandboxWatchEvent {
	return &nodectrlv1.SandboxWatchEvent{
		Type: sandboxv1.EventType_EVENT_TYPE_DELETED,
		Sandbox: &sandboxv1.Sandbox{
			Metadata: &sandboxv1.SandboxMetadata{Uid: uid},
		},
	}
}

func TestNewRejectsNilStdout(t *testing.T) {
	_, err := New(Config{Runner: noopRunner()})
	if err == nil {
		t.Fatal("expected error for nil stdout")
	}
}

func TestNewDefaultsRunnerToHello(t *testing.T) {
	// Production callers pass Runner = nil; the constructor must
	// substitute sandbox.Hello so HandleSandboxEvent doesn't NPE.
	e, err := New(Config{Stdout: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.run == nil {
		t.Fatal("runner not defaulted")
	}
}

func TestDeletedSandboxCancelsExactRunningVM(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	e, err := New(Config{Stdout: io.Discard, Runner: func(ctx context.Context, _ io.Writer, _ sandbox.Config) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.HandleSandboxEvent(t.Context(), addedEvent(testSandboxID, &sandboxv1.SandboxSpec{})) }()
	<-started
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent("other")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
		t.Fatal("deleting another Sandbox canceled the running VM")
	default:
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent(testSandboxID, &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("duplicate ADDED: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent(testSandboxID)); err != nil {
		t.Fatal(err)
	}
	<-stopped
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ADDED handler error = %v", err)
	}
}

func TestDeletePreservesWorkspaceBeforeCancel(t *testing.T) {
	mat := &fakeMaterializer{}
	canceled := false
	e := &Executor{
		mountMaterializer: mat,
		running:           map[string]context.CancelFunc{testSandboxID: func() { canceled = true }},
		preserve:          map[string]func(context.Context) error{},
		live:              map[string]*liveSandbox{},
		deleted:           map[string]time.Time{},
	}
	e.setSandboxPreserver(testSandboxID, &sandboxv1.SandboxSpec{WorkspaceUuid: testWSUUID, Mounts: []*sandboxv1.Mount{{Target: mntWorkspace}}})
	if err := e.deleteSandbox(t.Context(), testSandboxID); err != nil {
		t.Fatal(err)
	}
	if !canceled || len(mat.preserved) != 1 || mat.preserved[0] != workspace.SyncKey(testWSUUID, mntWorkspace) {
		t.Fatalf("canceled=%v preserved=%v", canceled, mat.preserved)
	}
}

func TestDeleteKeepsWorkspaceRunningWhenPreserveFails(t *testing.T) {
	want := errors.New("snapshot failed")
	canceled := false
	e := &Executor{
		running:  map[string]context.CancelFunc{testSandboxID: func() { canceled = true }},
		preserve: map[string]func(context.Context) error{testSandboxID: func(context.Context) error { return want }},
		live:     map[string]*liveSandbox{},
		deleted:  map[string]time.Time{},
	}
	err := e.deleteSandbox(t.Context(), testSandboxID)
	if !errors.Is(err, want) || canceled {
		t.Fatalf("delete = %v, canceled=%v", err, canceled)
	}
}

func TestDeleteBeforeAddedPreventsBootAndIsIdempotent(t *testing.T) {
	calls := 0
	e, err := New(Config{Stdout: io.Discard, Runner: func(context.Context, io.Writer, sandbox.Config) error {
		calls++
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent(testSandboxID)); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent(testSandboxID, &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("runner called %d times after delete-before-add", calls)
	}
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent(testSandboxID)); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredDeleteTombstonesDoNotExhaustCapacity(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxPendingSandboxDeletes {
		e.deleted[fmt.Sprintf("old-%d", i)] = time.Now().Add(-pendingSandboxDeleteTTL)
	}
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent(testSandboxID)); err != nil {
		t.Fatalf("delete after expired tombstones: %v", err)
	}
	if len(e.deleted) != 1 {
		t.Fatalf("pending deletes = %d, want 1", len(e.deleted))
	}
}

func TestPendingDeleteCapacityFailsClosed(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxPendingSandboxDeletes {
		e.deleted[fmt.Sprintf("pending-%d", i)] = time.Now()
	}
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent(testSandboxID)); err == nil {
		t.Fatal("delete beyond tombstone capacity succeeded")
	}
}

func TestTerminalSnapshotEntryIgnoresLateDeleteAndDuplicateAdd(t *testing.T) {
	calls := 0
	e, err := New(Config{Stdout: io.Discard, Runner: func(context.Context, io.Writer, sandbox.Config) error {
		calls++
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.markPhase(testSandboxID, phaseExited, 0)
	if err := e.HandleSandboxEvent(t.Context(), deletedEvent(testSandboxID)); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent(testSandboxID, &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("runner called %d times for a completed Sandbox", calls)
	}
}

// TestHandleSandboxEventBareHelloPath covers the empty-image_ref branch:
// the runner sees the executor's baseline config, no assembler is
// consulted.
func TestHandleSandboxEventBareHelloPath(t *testing.T) {
	base := sandbox.Config{
		CloudHypervisor: "/path/to/ch",
		Kernel:          "/path/to/kernel",
		Initramfs:       "/path/to/initramfs",
		Memory:          "512M",
		CPUs:            2,
	}
	var seen sandbox.Config
	called := 0
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		called++
		return nil
	}
	assemblerCalled := false
	asm := func(context.Context, string, sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		assemblerCalled = true
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{Base: base, Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-42", &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-1"})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if called != 1 {
		t.Errorf("runner called %d times, want 1", called)
	}
	if assemblerCalled {
		t.Error("assembler called for empty image_ref")
	}
	if seen.CloudHypervisor != base.CloudHypervisor ||
		seen.Kernel != base.Kernel ||
		seen.Initramfs != base.Initramfs {
		t.Errorf("runner saw cfg %+v, want baseline %+v", seen, base)
	}
}

// TestHandleSandboxEventAssemblerPath covers the image_ref-set branch:
// the assembler is consulted, the runner sees the bundled config,
// and the unlock returned by the assembler runs after the boot.
func TestHandleSandboxEventAssemblerPath(t *testing.T) {
	bundled := sandbox.Config{
		CloudHypervisor: "/from/assembler/ch",
		Kernel:          "/from/assembler/kernel",
		Initramfs:       "/from/assembler/initramfs",
		Bundle:          "/bundles/abc",
	}
	var seenRef string
	unlockCalls := 0
	asm := func(_ context.Context, ref string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		seenRef = ref
		return bundled, func() { unlockCalls++ }, nil
	}
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-7", &sandboxv1.SandboxSpec{ImageRef: testImageRef})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seenRef != testImageRef {
		t.Errorf("assembler saw ref %q, want %q", seenRef, testImageRef)
	}
	if seen.Bundle != bundled.Bundle {
		t.Errorf("runner saw bundle %q, want %q", seen.Bundle, bundled.Bundle)
	}
	if unlockCalls != 1 {
		t.Errorf("unlock called %d times, want 1", unlockCalls)
	}
}

func TestEnvFromSpec(t *testing.T) {
	if got := envFromSpec(nil); got != nil {
		t.Fatalf("envFromSpec(nil) = %v, want nil", got)
	}
	got := envFromSpec([]*sandboxv1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "x=y"}})
	want := []string{"A=1", "B=x=y"}
	if !slices.Equal(got, want) {
		t.Fatalf("envFromSpec = %v, want %v", got, want)
	}
}

// TestHandleSandboxEventProjectsEnv proves the spec's env reaches the
// assembler's BootOptions (the J2a wiring) rather than being dropped.
func TestHandleSandboxEventProjectsEnv(t *testing.T) {
	var seenEnv []string
	asm := func(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		seenEnv = opts.Env
		return sandbox.Config{Bundle: "/b"}, func() {}, nil
	}
	runner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
	e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-env", &sandboxv1.SandboxSpec{
		ImageRef: testImageRef,
		Env:      []*sandboxv1.EnvVar{{Name: "FOO", Value: "bar"}},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if !slices.Contains(seenEnv, "FOO=bar") {
		t.Fatalf("assembler BootOptions.Env = %v, want to contain FOO=bar", seenEnv)
	}
}

// TestHandleSandboxEventProjectsNestedVirt proves the spec's
// nested_virtualization.enable reaches the assembler's
// BootOptions.Nested — the wire that flips the /dev/kvm bind — and
// that an unset section leaves it false (the unprivileged default).
//
// projection test; each pins one grant's spec→BootOptions wire.
//
//nolint:dupl // Deliberately parallel to the privileged-fuse
func TestHandleSandboxEventProjectsNestedVirt(t *testing.T) {
	for _, tc := range []struct {
		name string
		nv   *sandboxv1.NestedVirtualization
		want bool
	}{
		{"unset", nil, false},
		{"disabled", &sandboxv1.NestedVirtualization{Enable: false}, false},
		{"enabled", &sandboxv1.NestedVirtualization{Enable: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			asm := func(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
				seen = opts.Nested
				return sandbox.Config{Bundle: "/b"}, func() {}, nil
			}
			runner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
			e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			evt := addedEvent("sb-nv", &sandboxv1.SandboxSpec{
				ImageRef:             testImageRef,
				NestedVirtualization: tc.nv,
			})
			if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
				t.Fatalf("HandleSandboxEvent: %v", err)
			}
			if seen != tc.want {
				t.Fatalf("assembler BootOptions.Nested = %v, want %v", seen, tc.want)
			}
		})
	}
}

// TestHandleSandboxEventProjectsPrivilegedFuse proves the spec's
// privileged_fuse_mounts.enable reaches the assembler's
// BootOptions.PrivilegedFuseMounts — the wire that flips the /dev/fuse
// bind + CAP_SYS_ADMIN — and that an unset section leaves it false.
//
// test; each pins one grant's spec→BootOptions wire.
//
//nolint:dupl // Deliberately parallel to the nested-virt projection
func TestHandleSandboxEventProjectsPrivilegedFuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		pf   *sandboxv1.PrivilegedFuseMounts
		want bool
	}{
		{"unset", nil, false},
		{"disabled", &sandboxv1.PrivilegedFuseMounts{Enable: false}, false},
		{"enabled", &sandboxv1.PrivilegedFuseMounts{Enable: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			asm := func(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
				seen = opts.PrivilegedFuseMounts
				return sandbox.Config{Bundle: "/b"}, func() {}, nil
			}
			runner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
			e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			evt := addedEvent("sb-pf", &sandboxv1.SandboxSpec{
				ImageRef:             testImageRef,
				PrivilegedFuseMounts: tc.pf,
			})
			if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
				t.Fatalf("HandleSandboxEvent: %v", err)
			}
			if seen != tc.want {
				t.Fatalf("assembler BootOptions.PrivilegedFuseMounts = %v, want %v", seen, tc.want)
			}
		})
	}
}

// TestEgressActive pins the single predicate that gates BOTH proxy
// injection and the DenyEgress decision: an EgressProxy with a nil
// Handler must not count as active, or deny would turn on with no
// proxy lane and black-hole all egress.
func TestEgressActive(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		name string
		p    *EgressProxy
		want bool
	}{
		{"nil proxy", nil, false},
		{"nil handler", &EgressProxy{Handler: nil, CACertPEM: []byte("x")}, false},
		{"handler set", &EgressProxy{Handler: handler}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Executor{egressProxy: tc.p}
			if got := e.egressActive(); got != tc.want {
				t.Fatalf("egressActive() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProxyPort pins the gateway-port resolution: a configured Port
// wins, zero falls back to sandboxnet's default. The fallback is what
// the fixed guest https_proxy depends on.
func TestProxyPort(t *testing.T) {
	custom := &Executor{egressProxy: &EgressProxy{Port: 9999}}
	if got := custom.proxyPort(); got != 9999 {
		t.Errorf("proxyPort(custom) = %d, want 9999", got)
	}
	deflt := &Executor{egressProxy: &EgressProxy{}}
	if got := deflt.proxyPort(); got != sandboxnet.DefaultProxyPort {
		t.Errorf("proxyPort(default) = %d, want %d", got, sandboxnet.DefaultProxyPort)
	}
}

func TestProxyEnv(t *testing.T) {
	const u = "http://192.168.127.1:3128"
	got := proxyEnv(u)
	for _, want := range []string{
		"http_proxy=" + u, "https_proxy=" + u,
		"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u,
		"no_proxy=localhost,127.0.0.1", "NO_PROXY=localhost,127.0.0.1",
		"NIX_SSL_CERT_FILE=" + egressCAGuestPath,
		"SSL_CERT_FILE=" + egressCAGuestPath,
		"CURL_CA_BUNDLE=" + egressCAGuestPath,
		"GIT_SSL_CAINFO=" + egressCAGuestPath,
		"REQUESTS_CA_BUNDLE=" + egressCAGuestPath,
		"NODE_EXTRA_CA_CERTS=" + egressCAGuestPath,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("proxyEnv missing %q in %v", want, got)
		}
	}
}

// TestHandleSandboxEventInjectsProxy proves the executor layers the
// egress-proxy env + CA onto an image-backed dispatch, and that the
// proxy's https_proxy is ordered after a workspace's own so the
// bundle's env merge lets the proxy win.
func TestHandleSandboxEventInjectsProxy(t *testing.T) {
	// The in-stack proxy lives on the netstack gateway at a fixed
	// gateway:DefaultProxyPort, so the injected https_proxy is derived,
	// not configured.
	u := "http://" + net.JoinHostPort(sandboxnet.DefaultGatewayIP, strconv.Itoa(int(sandboxnet.DefaultProxyPort)))
	ca := []byte("CA-PEM")
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	var opts sandboxboot.BootOptions
	asm := func(_ context.Context, _ string, o sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		opts = o
		return sandbox.Config{Bundle: "/b"}, func() {}, nil
	}
	runner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
	e, err := New(Config{
		Stdout: io.Discard, Runner: runner, Assembler: asm,
		EgressProxy: &EgressProxy{Handler: handler, CACertPEM: ca},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-proxy", &sandboxv1.SandboxSpec{
		ImageRef: testImageRef,
		Env:      []*sandboxv1.EnvVar{{Name: "https_proxy", Value: "user-set"}},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	userIdx := slices.Index(opts.Env, "https_proxy=user-set")
	proxyIdx := slices.Index(opts.Env, "https_proxy="+u)
	if userIdx < 0 || proxyIdx < 0 || proxyIdx < userIdx {
		t.Errorf("want proxy https_proxy after user's; env=%v (user=%d proxy=%d)", opts.Env, userIdx, proxyIdx)
	}
	if !slices.Contains(opts.Env, "SSL_CERT_FILE="+egressCAGuestPath) {
		t.Errorf("SSL_CERT_FILE not injected: %v", opts.Env)
	}
	if !bytes.Equal(opts.CACert, ca) || opts.CACertGuestPath != egressCAGuestPath {
		t.Errorf("CA not passed: pem=%q path=%q", opts.CACert, opts.CACertGuestPath)
	}
}

func TestExplicitSandboxEgressPolicy(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	e := &Executor{netEnabled: true, vsockDir: t.TempDir(), egressProxy: &EgressProxy{Handler: handler, CACertPEM: []byte("CA")}}
	e.nextCID.Store(2)

	var denied sandbox.Config
	e.applyVsockEgress(&denied, "denied", true, false, &sandboxv1.EgressPolicy{})
	if !denied.DenyEgress || denied.ProxyHandler != nil || denied.ProxyPort != 0 {
		t.Errorf("disabled egress config = %#v", denied)
	}
	var deniedOpts sandboxboot.BootOptions
	if err := e.applyEgressOpts(&deniedOpts, &sandboxv1.EgressPolicy{}); err != nil || len(deniedOpts.Env) != 0 || len(deniedOpts.CACert) != 0 {
		t.Errorf("disabled egress opts = %#v, %v", deniedOpts, err)
	}

	var allowed sandbox.Config
	e.applyVsockEgress(&allowed, "allowed", true, false, &sandboxv1.EgressPolicy{Enable: true})
	if !allowed.DenyEgress || allowed.ProxyHandler == nil || allowed.ProxyPort != sandboxnet.DefaultProxyPort {
		t.Errorf("enabled egress config = %#v", allowed)
	}
	var allowedOpts sandboxboot.BootOptions
	if err := e.applyEgressOpts(&allowedOpts, &sandboxv1.EgressPolicy{Enable: true}); err != nil || len(allowedOpts.Env) == 0 || !bytes.Equal(allowedOpts.CACert, []byte("CA")) {
		t.Errorf("enabled egress opts = %#v, %v", allowedOpts, err)
	}

	withoutProxy := &Executor{}
	if err := withoutProxy.applyEgressOpts(&sandboxboot.BootOptions{}, &sandboxv1.EgressPolicy{Enable: true}); err == nil {
		t.Error("enabled egress without a Node proxy succeeded")
	}
}

// TestHandleSandboxEventAssemblerError pins the error path when the
// assembler fails — runner must not be invoked and the dispatch
// id must appear in the wrapped error.
func TestHandleSandboxEventAssemblerError(t *testing.T) {
	want := errors.New("synthetic pull failure")
	asm := func(context.Context, string, sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		return sandbox.Config{}, func() {}, want
	}
	runCalls := 0
	runner := func(context.Context, io.Writer, sandbox.Config) error {
		runCalls++
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner, Assembler: asm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-bad", &sandboxv1.SandboxSpec{ImageRef: testImageRef})
	got := e.HandleSandboxEvent(t.Context(), evt)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if runCalls != 0 {
		t.Error("runner was invoked despite assembler failure")
	}
	if !strings.Contains(got.Error(), "sb-bad") {
		t.Errorf("err message %q should mention sandbox uid", got.Error())
	}
}

// TestHandleSandboxEventNoAssemblerFallback covers the production
// safety net: image_ref is set but no Assembler is configured.
// Expected behaviour is to fall back to the baseline config so
// the dispatch still produces something observable; A4b/A5a
// production wiring always sets an Assembler.
func TestHandleSandboxEventNoAssemblerFallback(t *testing.T) {
	base := sandbox.Config{Kernel: "/baseline/kernel"}
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Base: base, Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent(testSandboxID, &sandboxv1.SandboxSpec{ImageRef: testImageRef})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Kernel != base.Kernel {
		t.Errorf("runner saw cfg %+v, want baseline (assembler absent)", seen)
	}
}

// TestHandleSandboxEventPropagatesMounts pins D6c.1's mount
// wiring: spec.Mounts flow through to the assembler as
// sandboxboot.BootOptions.Mounts, in declaration order. Without
// this seam, `daft workspace mount add /workspace` is a no-op
// inside the container.
func TestHandleSandboxEventPropagatesMounts(t *testing.T) {
	var seenOpts sandboxboot.BootOptions
	asm := func(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		seenOpts = opts
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{
		Stdout:    io.Discard,
		Runner:    noopRunner(),
		Assembler: asm,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-mnt", &sandboxv1.SandboxSpec{
		ImageRef: testImageRef,
		Mounts: []*sandboxv1.Mount{
			{Target: mntWorkspace, Source: hostWS},
			{Target: mntData, Source: "/host/data", ReadOnly: true},
		},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if len(seenOpts.Mounts) != 2 {
		t.Fatalf("Mounts = %d, want 2", len(seenOpts.Mounts))
	}
	want := []sandboxboot.Mount{
		{Target: mntWorkspace, Source: hostWS},
		{Target: mntData, Source: "/host/data", ReadOnly: true},
	}
	for i, m := range want {
		if seenOpts.Mounts[i] != m {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, seenOpts.Mounts[i], m)
		}
	}
}

// fakeMaterializer records the sync keys it's asked to materialise and
// optionally creates the dest dir, so tests can assert the executor
// drives it with the right key and wires the backing dir as the share
// Source.
type fakeMaterializer struct {
	keys    []string
	applied bool
	err     error
	mkdir   bool
	// seed, when set, populates the freshly-materialised dest dir — used
	// to drop a devcontainer.json into the workspace content for the
	// node-side overlay test. Implies mkdir.
	seed        func(dest string) error
	liveSet     map[string]string
	preserved   []string
	cleared     []string
	preserveErr error
	refreshers  map[string]func(context.Context, string) error
}

func (f *fakeMaterializer) SetLiveMountRefresher(key string, refresh func(context.Context, string) error) {
	if f.refreshers == nil {
		f.refreshers = map[string]func(context.Context, string) error{}
	}
	f.refreshers[key] = refresh
}

type fakeDiskPreparer struct {
	workspace string
	size      uint64
	path      string
	err       error
}

func (f *fakeDiskPreparer) Prepare(workspaceID string, virtualSize uint64) (path string, created bool, err error) {
	f.workspace = workspaceID
	f.size = virtualSize
	return f.path, true, f.err
}

func TestApplyWorkspaceDiskDefaultsAndConfiguresVMM(t *testing.T) {
	preparer := &fakeDiskPreparer{path: "/state/workspace.qcow2"}
	e := &Executor{workspaceDisks: preparer, vsockDir: testVsockDir}
	cfg := sandbox.Config{Shares: []*sandbox.ShareConfig{
		{Tag: "bundles", Mountpoint: "/bundles"},
		{Tag: "rw", Mountpoint: mntWorkspace},
		{Tag: "ro", Mountpoint: mntReference},
	}}
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: testWSUUID, Mounts: []*sandboxv1.Mount{
		{Target: mntWorkspace}, {Target: mntReference, ReadOnly: true},
	}}
	if err := e.applyWorkspaceDisk(&cfg, testSandboxID, spec); err != nil {
		t.Fatal(err)
	}
	if preparer.workspace != testWSUUID || preparer.size != workspacedisk.DefaultDiskBytes {
		t.Errorf("Prepare = (%q, %d)", preparer.workspace, preparer.size)
	}
	if cfg.WorkspaceDisk == nil || cfg.WorkspaceDisk.Path != preparer.path || cfg.WorkspaceDisk.ID != workspaceDiskID {
		t.Errorf("WorkspaceDisk = %#v", cfg.WorkspaceDisk)
	}
	if cfg.APISocket != filepath.Join(testVsockDir, testSandboxID+".api.sock") {
		t.Errorf("APISocket = %q", cfg.APISocket)
	}
	if len(cfg.WorkspaceMounts) != 1 || cfg.WorkspaceMounts[0].Target != mntWorkspace ||
		cfg.WorkspaceMounts[0].Directory != "mounts/0" || cfg.WorkspaceMounts[0].Seed != workspaceSeedRoot+"/0" {
		t.Errorf("WorkspaceMounts = %#v", cfg.WorkspaceMounts)
	}
	if len(cfg.Shares) != 3 || cfg.Shares[1].Mountpoint != workspaceSeedRoot+"/0" || !cfg.Shares[1].ReadOnly ||
		cfg.Shares[2].Mountpoint != mntReference {
		t.Errorf("Shares = %#v, want bundle, read-only seed, and read-only reference", cfg.Shares)
	}
}

func TestConfigureWorkspaceMountsKeepsAbsoluteHostControlMountWritable(t *testing.T) {
	cfg := sandbox.Config{Shares: []*sandbox.ShareConfig{{Mountpoint: mntData}}}
	configureWorkspaceMounts(&cfg, []*sandboxv1.Mount{{Target: mntData, Source: hostWS}})
	if len(cfg.WorkspaceMounts) != 0 || cfg.Shares[0].Mountpoint != mntData || cfg.Shares[0].ReadOnly {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestApplyWorkspaceDiskUsesExplicitSizeAndPropagatesErrors(t *testing.T) {
	want := errors.New("disk unavailable")
	preparer := &fakeDiskPreparer{err: want}
	e := &Executor{workspaceDisks: preparer, vsockDir: testVsockDir}
	err := e.applyWorkspaceDisk(&sandbox.Config{}, testSandboxID,
		&sandboxv1.SandboxSpec{WorkspaceUuid: testWSUUID, DiskBytes: 24 << 30})
	if !errors.Is(err, want) || preparer.size != 24<<30 {
		t.Fatalf("applyWorkspaceDisk = %v, size %d", err, preparer.size)
	}
	if err := e.applyWorkspaceDisk(&sandbox.Config{}, testSandboxID, &sandboxv1.SandboxSpec{}); err == nil {
		t.Fatal("missing workspace UUID accepted")
	}
}

func TestRegisterWorkspaceSnapshotRoutes(t *testing.T) {
	mat := &fakeMaterializer{}
	e := &Executor{mountMaterializer: mat}
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: testWSUUID, Mounts: []*sandboxv1.Mount{
		{Target: mntWorkspace}, {Target: mntReference, ReadOnly: true}, {Target: mntData, Source: hostWS}, nil,
	}}
	e.registerWorkspaceSnapshotRoutes(sandbox.Config{}, spec)
	if len(mat.refreshers) != 0 {
		t.Fatalf("routes without disk/vsock = %v", mat.refreshers)
	}
	e.registerWorkspaceSnapshotRoutes(sandbox.Config{
		Vsock: &sandbox.VsockConfig{HostSocket: "/run/daft/test.sock"}, WorkspaceDisk: &sandbox.DiskConfig{Path: "/disk"},
	}, spec)
	key := workspace.SyncKey(testWSUUID, mntWorkspace)
	if len(mat.refreshers) != 1 || mat.refreshers[key] == nil {
		t.Fatalf("refreshers = %v", mat.refreshers)
	}
}

func (f *fakeMaterializer) MaterializeMount(_ context.Context, key, dest string) (bool, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return false, f.err
	}
	if f.mkdir || f.seed != nil {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return false, err
		}
	}
	if f.seed != nil {
		if err := f.seed(dest); err != nil {
			return false, err
		}
	}
	return f.applied, nil
}

func (f *fakeMaterializer) SetLiveMount(key, dir string) {
	if f.liveSet == nil {
		f.liveSet = map[string]string{}
	}
	f.liveSet[key] = dir
}

func (f *fakeMaterializer) PreserveLiveMount(_ context.Context, key string) error {
	f.preserved = append(f.preserved, key)
	return f.preserveErr
}

func (f *fakeMaterializer) ClearLiveMount(key string) {
	f.cleared = append(f.cleared, key)
}

// TestHandleSandboxEventDevcontainerOverlay pins the node-side slice-2d
// resolve+overlay: a devcontainer-only spec (no explicit image_ref) boots
// the image the devcontainer names, read from the materialised workspace
// content, with the workspace's explicit env overlaid on top.
func TestHandleSandboxEventDevcontainerOverlay(t *testing.T) {
	mat := &fakeMaterializer{applied: true, seed: func(dest string) error {
		dir := filepath.Join(dest, ".devcontainer")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "devcontainer.json"),
			[]byte(`{"image":"ghcr.io/acme/dev:1","containerEnv":{"FOO":"bar"}}`), 0o644)
	}}
	var gotRef string
	var gotOpts sandboxboot.BootOptions
	asm := func(_ context.Context, ref string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		gotRef, gotOpts = ref, opts
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{
		Stdout: io.Discard, Runner: noopRunner(), Assembler: asm,
		MountMaterializer: mat, MountBackingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid:    testWSUUID,
		DevcontainerPath: devcontainerNestedPath,
		// Empty Source ⇒ CAS-materialised content mount.
		Mounts: []*sandboxv1.Mount{{Target: mntWorkspace}},
		Env:    []*sandboxv1.EnvVar{{Name: "OVERRIDE", Value: "ws"}},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-dc", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if gotRef != "ghcr.io/acme/dev:1" {
		t.Errorf("assembler image ref = %q, want the devcontainer's image", gotRef)
	}
	if !slices.Contains(gotOpts.Env, "FOO=bar") {
		t.Errorf("env = %v, want FOO=bar from the devcontainer", gotOpts.Env)
	}
	if !slices.Contains(gotOpts.Env, "OVERRIDE=ws") {
		t.Errorf("env = %v, want the workspace's explicit OVERRIDE preserved", gotOpts.Env)
	}
}

// TestDevcontainerForwardPortsOverlay pins the port overlay: a
// devcontainer's forwardPorts are folded into the dispatch's forwards,
// unioned with the workspace's explicit ports (explicit winning a clash).
func TestDevcontainerForwardPortsOverlay(t *testing.T) {
	mat := &fakeMaterializer{applied: true, seed: func(dest string) error {
		return os.WriteFile(filepath.Join(dest, "devcontainer.json"),
			[]byte(`{"image":"ghcr.io/acme/dev:1","forwardPorts":[3000]}`), 0o644)
	}}
	asm := func(_ context.Context, _ string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		return sandbox.Config{}, func() {}, nil
	}
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{
		Stdout: io.Discard, Runner: runner, Assembler: asm,
		MountMaterializer: mat, MountBackingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid:    testWSUUID,
		DevcontainerPath: devcontainerFlatPath,
		Mounts:           []*sandboxv1.Mount{{Target: mntWorkspace}},
		// An explicit workspace port that should survive alongside the
		// devcontainer's 3000.
		Ports: []*sandboxv1.PortMapping{{ContainerPort: 8080, Protocol: sandboxv1.Protocol_PROTOCOL_TCP}},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-ports", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	got := make(map[uint32]bool)
	for _, pf := range seen.PortForwards {
		got[pf.ContainerPort] = true
	}
	if !got[3000] {
		t.Errorf("PortForwards %v missing the devcontainer's 3000", seen.PortForwards)
	}
	if !got[8080] {
		t.Errorf("PortForwards %v missing the workspace's explicit 8080", seen.PortForwards)
	}
}

// TestDevcontainerMissingFileFailsDispatch pins that a devcontainer_path
// pointing at a file absent from the materialised content fails the
// dispatch loudly rather than booting something unexpected.
func TestDevcontainerMissingFileFailsDispatch(t *testing.T) {
	mat := &fakeMaterializer{applied: true, mkdir: true} // materialises an empty dir
	asm := func(_ context.Context, _ string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		t.Error("assembler must not run when the devcontainer is missing")
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{
		Stdout: io.Discard, Runner: noopRunner(), Assembler: asm,
		MountMaterializer: mat, MountBackingDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid:    testWSUUID,
		DevcontainerPath: devcontainerNestedPath,
		Mounts:           []*sandboxv1.Mount{{Target: "/workspace"}},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-miss", spec)); err == nil {
		t.Error("expected dispatch to fail when the devcontainer file is absent")
	}
}

// TestDevcontainerInvalidConfigFailsDispatch pins that a present but
// unusable devcontainer (malformed JSON, or build-based which daft can't
// build yet) fails the dispatch rather than booting the fallback.
func TestDevcontainerInvalidConfigFailsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed", `{not json`},
		{"build-based", `{"build":{"dockerfile":"Dockerfile"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mat := &fakeMaterializer{applied: true, seed: func(dest string) error {
				return os.WriteFile(filepath.Join(dest, "devcontainer.json"), []byte(tc.body), 0o644)
			}}
			asm := func(_ context.Context, _ string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
				t.Error("assembler must not run for an invalid devcontainer")
				return sandbox.Config{}, func() {}, nil
			}
			e, err := New(Config{
				Stdout: io.Discard, Runner: noopRunner(), Assembler: asm,
				MountMaterializer: mat, MountBackingDir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			spec := &sandboxv1.SandboxSpec{
				WorkspaceUuid:    testWSUUID,
				DevcontainerPath: devcontainerFlatPath,
				Mounts:           []*sandboxv1.Mount{{Target: "/workspace"}},
			}
			if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-bad", spec)); err == nil {
				t.Error("expected dispatch to fail for an invalid devcontainer")
			}
		})
	}
}

func TestFindDevcontainerFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".devcontainer"), 0o750); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".devcontainer", "devcontainer.json")
	if err := os.WriteFile(want, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mounts := []sandboxboot.Mount{{Source: ""}, {Source: root}} // empty source skipped

	got, gotRoot, err := findDevcontainerFile(mounts, ".devcontainer/devcontainer.json")
	if err != nil || got != want {
		t.Errorf("findDevcontainerFile = %q, %v; want %q", got, err, want)
	}
	if gotRoot != root {
		t.Errorf("findDevcontainerFile root = %q; want %q", gotRoot, root)
	}
	// A "../" escape is rooted-and-cleaned away, so it resolves within the
	// mount (here: not found) rather than reaching outside it.
	if _, _, err := findDevcontainerFile(mounts, "../../etc/passwd"); err == nil {
		t.Error("expected not-found for an escaping path, got nil")
	}
	// Degenerate paths are rejected.
	for _, bad := range []string{"", ".", "/"} {
		if _, _, err := findDevcontainerFile(mounts, bad); err == nil {
			t.Errorf("findDevcontainerFile(%q): want error", bad)
		}
	}
}

func TestNewRejectsMaterializerWithoutBackingDir(t *testing.T) {
	if _, err := New(Config{Stdout: io.Discard, MountMaterializer: &fakeMaterializer{}}); err == nil {
		t.Error("expected error for materializer without backing dir")
	}
	if _, err := New(Config{
		Stdout:            io.Discard,
		MountMaterializer: &fakeMaterializer{},
		MountBackingDir:   "relative/path",
	}); err == nil {
		t.Error("expected error for non-absolute backing dir")
	}
}

func TestNewRejectsRelativeObserverDir(t *testing.T) {
	if _, err := New(Config{
		Stdout:      io.Discard,
		Runner:      noopRunner(),
		ObserverDir: "relative/observers",
	}); err == nil {
		t.Error("expected error for non-absolute ObserverDir")
	}
}

// TestHandleSandboxEventDefaultsObserverLog pins the Tier-1 wiring: a
// dispatch under an executor with an ObserverDir set boots with
// ObserverLogPath defaulted to <ObserverDir>/<uid>/observer.jsonl,
// and the per-uid directory is created so the observer can open it.
func TestHandleSandboxEventDefaultsObserverLog(t *testing.T) {
	observerDir := t.TempDir()
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner, ObserverDir: observerDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-obs", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	want := filepath.Join(observerDir, "sb-obs", "observer.jsonl")
	if seen.ObserverLogPath != want {
		t.Errorf("ObserverLogPath = %q, want %q", seen.ObserverLogPath, want)
	}
	if fi, err := os.Stat(filepath.Dir(want)); err != nil || !fi.IsDir() {
		t.Errorf("per-uid observer dir not created: stat err=%v", err)
	}
}

// TestHandleSandboxEventObserverLogDisabledWhenNoDir pins the default
// (pre-Tier-1) behaviour: with no ObserverDir, the dispatched config
// carries no ObserverLogPath.
func TestHandleSandboxEventObserverLogDisabledWhenNoDir(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-x", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.ObserverLogPath != "" {
		t.Errorf("ObserverLogPath = %q, want empty when no ObserverDir", seen.ObserverLogPath)
	}
}

// TestHandleSandboxEventObserverLogMkdirFailureIsBestEffort pins that a
// per-uid observer dir that cannot be created (its parent is a regular
// file) disables the observer for that dispatch rather than failing it.
func TestHandleSandboxEventObserverLogMkdirFailureIsBestEffort(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	var out bytes.Buffer
	// ObserverDir lives under the regular file, so MkdirAll ENOTDIRs.
	e, err := New(Config{Stdout: &out, Runner: runner, ObserverDir: filepath.Join(blocker, "observers")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-fail", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.ObserverLogPath != "" {
		t.Errorf("ObserverLogPath = %q, want empty when the dir can't be made", seen.ObserverLogPath)
	}
	if !strings.Contains(out.String(), "observer log disabled") {
		t.Errorf("stdout = %q, want a disabled notice", out.String())
	}
}

// TestHandleSandboxEventRegistersObserverRoute pins that a dispatch
// with an ObserverRegistry + ObserverDir registers a uid→log-path
// route for the duration of the run and unregisters it on exit.
func TestHandleSandboxEventRegistersObserverRoute(t *testing.T) {
	reg := sandboxgateway.NewObserverRegistry()
	observerDir := t.TempDir()
	var pathDuringRun string
	var resolveErr error
	runner := func(_ context.Context, _ io.Writer, _ sandbox.Config) error {
		pathDuringRun, resolveErr = reg.Resolve("sb-obs")
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner, ObserverDir: observerDir, ObserverRegistry: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-obs", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	want := filepath.Join(observerDir, "sb-obs", "observer.jsonl")
	if resolveErr != nil || pathDuringRun != want {
		t.Errorf("route during run = %q (err %v), want %q", pathDuringRun, resolveErr, want)
	}
	if _, err := reg.Resolve("sb-obs"); err == nil {
		t.Error("observer route should be unregistered after the dispatch exits")
	}
}

// TestHandleSandboxEventRegistersSummaryRoute pins the Tier-2 observe-share
// wiring: with ObserveSummary opted in, a dispatch exports the observe
// share, points the gadget + summary guest paths into it, and registers the
// host summary path — torn down when the dispatch exits.
func TestHandleSandboxEventRegistersSummaryRoute(t *testing.T) {
	reg := sandboxgateway.NewSummaryRegistry()
	observerDir := t.TempDir()
	var routeDuringRun string
	var resolveErr error
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		routeDuringRun, resolveErr = reg.Resolve("sb-sum")
		seen = cfg
		return nil
	}
	e, err := New(Config{
		Stdout: io.Discard, Runner: runner,
		ObserverDir: observerDir, SummaryRegistry: reg, ObserveSummary: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-sum", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	wantHost := filepath.Join(observerDir, "sb-sum", "observe", "progress.json")
	if resolveErr != nil || routeDuringRun != wantHost {
		t.Errorf("summary route during run = %q (err %v), want %q", routeDuringRun, resolveErr, wantHost)
	}
	// The guest paths point into the observe share, and the share is exported.
	if seen.ObserveSummaryPath != "/run/daft/observe/progress.json" {
		t.Errorf("guest summary path = %q", seen.ObserveSummaryPath)
	}
	if seen.OpensnoopLogPath != "/run/daft/observe/opensnoop.log" {
		t.Errorf("guest opensnoop path = %q", seen.OpensnoopLogPath)
	}
	if seen.ExecsnoopLogPath != "/run/daft/observe/execsnoop.log" {
		t.Errorf("guest execsnoop path = %q", seen.ExecsnoopLogPath)
	}
	if seen.SleepsnoopLogPath != "/run/daft/observe/sleepsnoop.log" {
		t.Errorf("guest sleepsnoop path = %q", seen.SleepsnoopLogPath)
	}
	if seen.IosnoopLogPath != "/run/daft/observe/iosnoop.log" {
		t.Errorf("guest iosnoop path = %q", seen.IosnoopLogPath)
	}
	if seen.SyscallListPath != "/run/daft/observe/syscalls.txt" {
		t.Errorf("guest syscalls path = %q", seen.SyscallListPath)
	}
	var haveObserveShare bool
	for _, s := range seen.Shares {
		if s.Tag == "daftobserve" && s.Mountpoint == "/run/daft/observe" {
			haveObserveShare = true
		}
	}
	if !haveObserveShare {
		t.Errorf("observe share not exported: %+v", seen.Shares)
	}
	if _, err := reg.Resolve("sb-sum"); err == nil {
		t.Error("summary route should be unregistered after the dispatch exits")
	}
}

// TestHandleSandboxEventNoSummaryRouteWhenOptedOut pins the default: without
// ObserveSummary, no observe share and no summary route.
func TestHandleSandboxEventNoSummaryRouteWhenOptedOut(t *testing.T) {
	reg := sandboxgateway.NewSummaryRegistry()
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error { seen = cfg; return nil }
	e, err := New(Config{
		Stdout: io.Discard, Runner: runner,
		ObserverDir: t.TempDir(), SummaryRegistry: reg, // ObserveSummary defaults false
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-y", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.ObserveSummaryPath != "" {
		t.Errorf("summary path set despite opt-out: %q", seen.ObserveSummaryPath)
	}
	if _, err := reg.Resolve("sb-y"); err == nil {
		t.Error("no summary route should be registered when opted out")
	}
}

// TestHandleSandboxEventNoObserverRouteWithoutDir pins that an
// ObserverRegistry without an ObserverDir (so no ObserverLogPath)
// registers nothing.
func TestHandleSandboxEventNoObserverRouteWithoutDir(t *testing.T) {
	reg := sandboxgateway.NewObserverRegistry()
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner(), ObserverRegistry: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-x", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if _, err := reg.Resolve("sb-x"); err == nil {
		t.Error("no observer route should be registered without an ObserverDir")
	}
}

// TestHandleSandboxEventObserverLogBaselineWins pins that an explicit
// baseline ObserverLogPath is not overridden by the per-uid default.
func TestHandleSandboxEventObserverLogBaselineWins(t *testing.T) {
	const baselinePath = "/explicit/observer.jsonl"
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{
		Base:        sandbox.Config{ObserverLogPath: baselinePath},
		Stdout:      io.Discard,
		Runner:      runner,
		ObserverDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-base", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.ObserverLogPath != baselinePath {
		t.Errorf("ObserverLogPath = %q, want baseline %q untouched", seen.ObserverLogPath, baselinePath)
	}
}

// TestHandleSandboxEventMaterializesMounts pins the Slice S3 path: each
// spec mount is materialised from the node CAS into a per-Sandbox
// backing dir, and that dir — not the mount's logical Source — becomes
// the virtiofs share's host path. The materializer is keyed by
// SyncKey(workspace_uuid, target).
func TestHandleSandboxEventMaterializesMounts(t *testing.T) {
	var seenOpts sandboxboot.BootOptions
	asm := func(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		seenOpts = opts
		return sandbox.Config{}, func() {}, nil
	}
	fm := &fakeMaterializer{applied: true, mkdir: true}
	backing := t.TempDir()
	e, err := New(Config{
		Stdout:            io.Discard,
		Runner:            noopRunner(),
		Assembler:         asm,
		MountMaterializer: fm,
		MountBackingDir:   backing,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-mat", &sandboxv1.SandboxSpec{
		ImageRef:      testImageRef,
		WorkspaceUuid: testWSUUID,
		Mounts: []*sandboxv1.Mount{
			{Target: mntWorkspace, Source: testWSUUID},
			{Target: mntData, Source: testWSUUID, ReadOnly: true},
		},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}

	wantKeys := []string{
		workspace.SyncKey(testWSUUID, mntWorkspace),
		workspace.SyncKey(testWSUUID, mntData),
	}
	if !slices.Equal(fm.keys, wantKeys) {
		t.Errorf("materializer keys = %v, want %v", fm.keys, wantKeys)
	}
	if len(seenOpts.Mounts) != 2 {
		t.Fatalf("Mounts = %d, want 2", len(seenOpts.Mounts))
	}
	want := []sandboxboot.Mount{
		{Target: mntWorkspace, Source: filepath.Join(backing, "sb-mat", "mnt0")},
		{Target: mntData, Source: filepath.Join(backing, "sb-mat", "mnt1"), ReadOnly: true},
	}
	for i, m := range want {
		if seenOpts.Mounts[i] != m {
			t.Errorf("Mounts[%d] = %+v, want %+v", i, seenOpts.Mounts[i], m)
		}
	}
	// The rw mount is registered live (so a pull re-manifests it); the
	// ro mount is not (the VM can't have changed it). The writable mount is
	// durably preserved on teardown.
	if _, ok := fm.liveSet[workspace.SyncKey(testWSUUID, mntWorkspace)]; !ok {
		t.Error("rw mount not registered live")
	}
	if _, ok := fm.liveSet[workspace.SyncKey(testWSUUID, mntData)]; ok {
		t.Error("ro mount should not be registered live")
	}
	if !slices.Contains(fm.preserved, workspace.SyncKey(testWSUUID, mntWorkspace)) {
		t.Errorf("rw mount not preserved on teardown: %v", fm.preserved)
	}

	// The backing tree is cleaned up once the dispatch returns.
	if _, statErr := os.Stat(filepath.Join(backing, "sb-mat")); !os.IsNotExist(statErr) {
		t.Errorf("backing dir not cleaned up: stat err = %v", statErr)
	}
}

func TestTeardownKeepsBackingTreeWhenPreservationFails(t *testing.T) {
	backing := t.TempDir()
	uid := "preserve-failed"
	dir := filepath.Join(backing, uid, "mnt0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := errors.New("CAS unavailable")
	fm := &fakeMaterializer{preserveErr: want}
	var out bytes.Buffer
	e := &Executor{stdout: &out, mountMaterializer: fm, mountBackingDir: backing}
	e.teardownMounts(t.Context(), uid, &sandboxv1.SandboxSpec{WorkspaceUuid: testWSUUID, Mounts: []*sandboxv1.Mount{{Source: testWSUUID, Target: mntWorkspace}}})
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("backing tree was removed after preserve failure: %v", err)
	}
	if !strings.Contains(out.String(), want.Error()) {
		t.Fatalf("teardown output = %q", out.String())
	}
}

// TestHandleSandboxEventAbsoluteSourceBypassesMaterializer pins the
// discriminator: a mount whose Source is an absolute host path is a
// direct bind (the `mount add --source /abs` form) and must NOT be
// routed through the CAS materializer, even when one is wired.
func TestHandleSandboxEventAbsoluteSourceBypassesMaterializer(t *testing.T) {
	var seenOpts sandboxboot.BootOptions
	asm := func(_ context.Context, _ string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		seenOpts = opts
		return sandbox.Config{}, func() {}, nil
	}
	fm := &fakeMaterializer{applied: true, mkdir: true}
	e, err := New(Config{
		Stdout:            io.Discard,
		Runner:            noopRunner(),
		Assembler:         asm,
		MountMaterializer: fm,
		MountBackingDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-abs", &sandboxv1.SandboxSpec{
		ImageRef:      testImageRef,
		WorkspaceUuid: testWSUUID,
		Mounts:        []*sandboxv1.Mount{{Target: mntWorkspace, Source: hostWS}},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if len(fm.keys) != 0 {
		t.Errorf("materializer called for an absolute-source mount: %v", fm.keys)
	}
	if len(seenOpts.Mounts) != 1 || seenOpts.Mounts[0].Source != hostWS {
		t.Errorf("Mounts = %+v, want Source=/host/ws passthrough", seenOpts.Mounts)
	}
}

// TestHandleSandboxEventUnpushedMountWarns covers the applied=false
// branch: an unpushed mount still wires its (empty) backing dir and the
// executor logs a hint.
func TestHandleSandboxEventUnpushedMountWarns(t *testing.T) {
	var buf bytes.Buffer
	asm := func(_ context.Context, _ string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{
		Stdout:            &buf,
		Runner:            noopRunner(),
		Assembler:         asm,
		MountMaterializer: &fakeMaterializer{applied: false},
		MountBackingDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-empty", &sandboxv1.SandboxSpec{
		ImageRef:      testImageRef,
		WorkspaceUuid: "ws",
		Mounts:        []*sandboxv1.Mount{{Target: mntWorkspace, Source: "ws"}},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if !strings.Contains(buf.String(), "no pushed data") {
		t.Errorf("stdout missing unpushed-mount hint, got:\n%s", buf.String())
	}
}

// TestHandleSandboxEventMaterializeErrorFailsDispatch: a materializer
// error aborts the boot and marks the sandbox Killed.
func TestHandleSandboxEventMaterializeErrorFailsDispatch(t *testing.T) {
	asmCalled := false
	asm := func(_ context.Context, _ string, _ sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		asmCalled = true
		return sandbox.Config{}, func() {}, nil
	}
	e, err := New(Config{
		Stdout:            io.Discard,
		Runner:            noopRunner(),
		Assembler:         asm,
		MountMaterializer: &fakeMaterializer{err: errors.New("cas gone")},
		MountBackingDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-fail", &sandboxv1.SandboxSpec{
		ImageRef:      testImageRef,
		WorkspaceUuid: "ws",
		Mounts:        []*sandboxv1.Mount{{Target: mntWorkspace, Source: "ws"}},
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err == nil {
		t.Fatal("expected error when materializer fails")
	}
	if asmCalled {
		t.Error("assembler should not run after a materializer error")
	}
}

func TestHandleSandboxEventLogsToStdout(t *testing.T) {
	var buf bytes.Buffer
	e, err := New(Config{Stdout: &buf, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent("sb-99", &sandboxv1.SandboxSpec{
		ImageRef:      testImageRef,
		WorkspaceUuid: "ws-9",
	})
	if err := e.HandleSandboxEvent(t.Context(), evt); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"sb-99", "ws-9", testImageRef} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestHandleSandboxEventPanicsOnNilEvent(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("HandleSandboxEvent did not panic on nil event")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "nil event") {
			t.Errorf("panic = %q, want substring 'nil event'", msg)
		}
	}()
	_ = e.HandleSandboxEvent(t.Context(), nil)
}

func TestHandleSandboxEventPanicsWithoutSpec(t *testing.T) {
	e, err := New(Config{Stdout: io.Discard, Runner: noopRunner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("HandleSandboxEvent did not panic when Spec is nil")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "without a Spec") {
			t.Errorf("panic = %q, want substring 'without a Spec'", msg)
		}
	}()
	_ = e.HandleSandboxEvent(t.Context(), &nodectrlv1.SandboxWatchEvent{
		Type:    sandboxv1.EventType_EVENT_TYPE_ADDED,
		Sandbox: &sandboxv1.Sandbox{Metadata: &sandboxv1.SandboxMetadata{Uid: testSandboxID}},
	})
}

func TestHandleSandboxEventSurfacesRunnerError(t *testing.T) {
	want := errors.New("synthetic boot failure")
	runner := func(context.Context, io.Writer, sandbox.Config) error { return want }
	e, err := New(Config{Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	evt := addedEvent(testSandboxID, &sandboxv1.SandboxSpec{})
	got := e.HandleSandboxEvent(t.Context(), evt)
	if !errors.Is(got, want) {
		t.Errorf("err = %v, want wrapping %v", got, want)
	}
	if !strings.Contains(got.Error(), testSandboxID) {
		t.Errorf("err message %q should mention sandbox uid", got.Error())
	}
}

// TestLiveSnapshotTracksLifecycle pins the B4d heartbeat-side
// reporting: after a successful run the snapshot reports the
// sandbox with phase=Exited; on a runner error it reports
// phase=Killed. A second snapshot drops the terminal entry so
// future heartbeats don't keep re-reporting it.
func TestLiveSnapshotTracksLifecycle(t *testing.T) {
	okRunner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
	e, err := New(Config{Stdout: io.Discard, Runner: okRunner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.HandleSandboxEvent(t.Context(),
		addedEvent("sb-live-1", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	snap := e.LiveSnapshot()
	if len(snap) != 1 || snap[0].GetUid() != "sb-live-1" || snap[0].GetPhase() != "Exited" {
		t.Fatalf("snapshot[0] = %+v, want sb-live-1 Exited", snap[0])
	}
	// Terminal entries drop on the next snapshot — the
	// orchestrator infers "the sandbox is gone" from absence
	// going forward.
	if got := e.LiveSnapshot(); len(got) != 0 {
		t.Errorf("second snapshot = %+v, want empty", got)
	}
}

func TestLiveSnapshotReportsKilledOnRunnerError(t *testing.T) {
	want := errors.New("synthetic boot failure")
	failRunner := func(context.Context, io.Writer, sandbox.Config) error { return want }
	e, err := New(Config{Stdout: io.Discard, Runner: failRunner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = e.HandleSandboxEvent(t.Context(),
		addedEvent("sb-live-bad", &sandboxv1.SandboxSpec{}))
	snap := e.LiveSnapshot()
	if len(snap) != 1 || snap[0].GetPhase() != "Killed" {
		t.Fatalf("snapshot = %+v, want one Killed entry", snap)
	}
}

// TestLiveSnapshotIsSortedByUID pins the deterministic ordering
// the orchestrator relies on for stable trace and golden-file
// output.
func TestLiveSnapshotIsSortedByUID(t *testing.T) {
	okRunner := func(context.Context, io.Writer, sandbox.Config) error { return nil }
	e, err := New(Config{Stdout: io.Discard, Runner: okRunner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, uid := range []string{"sb-c", "sb-a", "sb-b"} {
		e.markPhase(uid, "Running", 0)
	}
	snap := e.LiveSnapshot()
	if len(snap) != 3 || snap[0].GetUid() != "sb-a" || snap[1].GetUid() != "sb-b" || snap[2].GetUid() != "sb-c" {
		t.Errorf("snapshot order = %+v, want sb-a, sb-b, sb-c", snap)
	}
}

// TestPortForwardsFromSpecAssignsSequentialVsockPorts pins the
// allocator: TCP entries get VsockPort = BaseVsockPort, +1, +2…
// in declaration order; UDP and zero-container-port entries are
// skipped silently (UDP because the CH hybrid-vsock CONNECT proxy
// is stream-only; zero because the orchestrator resolves "any"
// before dispatch).
func TestPortForwardsFromSpecAssignsSequentialVsockPorts(t *testing.T) {
	ports := []*sandboxv1.PortMapping{
		{ContainerPort: 8080, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		{ContainerPort: 53, Protocol: sandboxv1.Protocol_PROTOCOL_UDP}, // dropped
		{ContainerPort: 0, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},  // dropped
		{ContainerPort: 22, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
	}
	got := portForwardsFromSpec(ports)
	want := []sandbox.PortForward{
		{VsockPort: BaseVsockPort, ContainerPort: 8080},
		{VsockPort: BaseVsockPort + 1, ContainerPort: 22},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestApplyResourceSizingExplicit(t *testing.T) {
	cfg := sandbox.Config{}
	applyResourceSizing(&cfg, &sandboxv1.SandboxSpec{
		ImageRef:    "img:v1",
		MemoryBytes: 2 << 30, // 2 GiB
		CpuCores:    3,
	})
	if cfg.Memory != "2048M" {
		t.Errorf("Memory = %q, want 2048M", cfg.Memory)
	}
	if cfg.CPUs != 3 {
		t.Errorf("CPUs = %d, want 3", cfg.CPUs)
	}
}

func TestApplyResourceSizingDefaultsForImageDispatch(t *testing.T) {
	cfg := sandbox.Config{}
	applyResourceSizing(&cfg, &sandboxv1.SandboxSpec{ImageRef: "img:v1"})
	if want := defaultWorkspaceMemory(); cfg.Memory != want {
		t.Errorf("Memory = %q, want host-scaled default %q", cfg.Memory, want)
	}
	if want := defaultWorkspaceCPUs(); cfg.CPUs != want {
		t.Errorf("CPUs = %d, want default %d", cfg.CPUs, want)
	}
}

// TestApplyResourceSizingDefaultsForDevcontainerDispatch pins that a
// devcontainer-only workspace (no explicit image_ref) still gets the
// host-scaled defaults — the executor takes the image path for it too,
// so single-vCPU / 256 MiB would starve its build just as badly.
func TestApplyResourceSizingDefaultsForDevcontainerDispatch(t *testing.T) {
	cfg := sandbox.Config{}
	applyResourceSizing(&cfg, &sandboxv1.SandboxSpec{DevcontainerPath: devcontainerNestedPath})
	if want := defaultWorkspaceMemory(); cfg.Memory != want {
		t.Errorf("Memory = %q, want host-scaled default %q", cfg.Memory, want)
	}
	if want := defaultWorkspaceCPUs(); cfg.CPUs != want {
		t.Errorf("CPUs = %d, want default %d", cfg.CPUs, want)
	}
}

func TestApplyResourceSizingLeavesFallbackPathAlone(t *testing.T) {
	// No image ref and no devcontainer ⇒ the e.base smoke-test path;
	// don't impose the host-scaled defaults or touch a preset CPU count.
	cfg := sandbox.Config{CPUs: 1}
	applyResourceSizing(&cfg, &sandboxv1.SandboxSpec{})
	if cfg.CPUs != 1 {
		t.Errorf("CPUs = %d, want 1 (untouched on fallback path)", cfg.CPUs)
	}
	if cfg.Memory != "" {
		t.Errorf("Memory = %q, want empty (untouched on fallback path)", cfg.Memory)
	}
}

// TestDefaultWorkspaceMemoryScalesAndClamps drives defaultWorkspaceMemory
// through the host-RAM seam: a quarter of RAM in the normal range,
// clamped to the 1 GiB floor for a tiny host and the 8 GiB cap for a
// huge one, and the floor fallback when host detection fails.
func TestDefaultWorkspaceMemoryScalesAndClamps(t *testing.T) {
	const gib = 1 << 30
	cases := []struct {
		name    string
		memGiB  uint64
		fail    bool
		wantMiB string
	}{
		{"quarter", 16, false, "4096M"}, // 16 GiB / 4 = 4 GiB
		{"floor", 2, false, "1024M"},    // 2 GiB / 4 = 512 MiB → floor 1 GiB
		{"cap", 64, false, "8192M"},     // 64 GiB / 4 = 16 GiB → cap 8 GiB
		{"detect-fails", 16, true, "1024M"},
	}
	orig := detectHostResources
	t.Cleanup(func() { detectHostResources = orig })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detectHostResources = func() (nodectrl.Resources, error) {
				if tc.fail {
					return nodectrl.Resources{}, errors.New("no /proc/meminfo")
				}
				return nodectrl.Resources{MemoryBytes: tc.memGiB * gib}, nil
			}
			if got := defaultWorkspaceMemory(); got != tc.wantMiB {
				t.Errorf("defaultWorkspaceMemory() = %q, want %q", got, tc.wantMiB)
			}
		})
	}
}

func TestDefaultWorkspaceCPUsAtLeastOne(t *testing.T) {
	if got := defaultWorkspaceCPUs(); got < 1 {
		t.Errorf("defaultWorkspaceCPUs() = %d, want >= 1", got)
	}
}

func TestMemoryArgFromBytesRoundsUp(t *testing.T) {
	cases := map[uint64]string{
		1 << 20:       "1M",
		4 << 30:       "4096M",
		(1 << 20) + 1: "2M", // rounds up, never size=0
		1:             "1M",
	}
	for in, want := range cases {
		if got := memoryArgFromBytes(in); got != want {
			t.Errorf("memoryArgFromBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestPortForwardsFromSpecEmpty pins the no-ports path: a spec with no
// ports yields a nil PortForwards slice (so the GuestConfig carries no
// port-forward table), with no allocation churn.
func TestPortForwardsFromSpecEmpty(t *testing.T) {
	if got := portForwardsFromSpec(nil); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
	if got := portForwardsFromSpec([]*sandboxv1.PortMapping{}); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// TestHandleSandboxEventWiresPortForwardsToCfg covers the executor
// end-to-end: a Sandbox spec with ports[] arrives, the runner sees
// the resolved sandbox.Config carry the matching PortForwards.
func TestHandleSandboxEventWiresPortForwardsToCfg(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		Ports: []*sandboxv1.PortMapping{
			{ContainerPort: 8080, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
			{ContainerPort: 22, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-pf", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if len(seen.PortForwards) != 2 {
		t.Fatalf("PortForwards len = %d, want 2: %+v", len(seen.PortForwards), seen.PortForwards)
	}
	if seen.PortForwards[0].ContainerPort != 8080 || seen.PortForwards[1].ContainerPort != 22 {
		t.Errorf("PortForwards = %+v, want 8080 then 22", seen.PortForwards)
	}
	if seen.PortForwards[0].VsockPort != BaseVsockPort {
		t.Errorf("first VsockPort = %d, want %d", seen.PortForwards[0].VsockPort, BaseVsockPort)
	}
}

// TestNewRejectsBridgerWithoutVsockDir pins the configuration
// invariant: a Bridger needs somewhere to put per-Sandbox UDS
// files. New rejects the half-configured case at construction so
// the operator doesn't hit it at dispatch time.
func TestNewRejectsBridgerWithoutVsockDir(t *testing.T) {
	br := sandboxgateway.New()
	if _, err := New(Config{Stdout: io.Discard, Bridger: br}); err == nil {
		t.Fatal("expected error when Bridger is set but VsockDir is empty")
	}
	if _, err := New(Config{Stdout: io.Discard, Bridger: br, VsockDir: "relative"}); err == nil {
		t.Fatal("expected error when VsockDir is relative")
	}
}

// TestHandleSandboxEventAssignsPerSandboxVsock pins the
// per-dispatch Vsock allocation: when a Bridger is wired and the
// spec has ports, the executor sets cfg.Vsock with a fresh GuestCID
// and a UDS path under VsockDir named after the Sandbox uid.
func TestHandleSandboxEventAssignsPerSandboxVsock(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	br := sandboxgateway.New()
	e, err := New(Config{
		Stdout:   io.Discard,
		Runner:   runner,
		Bridger:  br,
		VsockDir: testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid: "ws-aaa",
		Ports: []*sandboxv1.PortMapping{
			{ContainerPort: 8080, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-xyz", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Vsock == nil {
		t.Fatal("cfg.Vsock not set")
	}
	if seen.Vsock.GuestCID < 3 {
		t.Errorf("GuestCID = %d, want >= 3", seen.Vsock.GuestCID)
	}
	wantPath := "/var/run/daft-sandboxd/sb-xyz.sock"
	if seen.Vsock.HostSocket != wantPath {
		t.Errorf("HostSocket = %q, want %q", seen.Vsock.HostSocket, wantPath)
	}
}

// TestHandleSandboxEventAssignsVsockAlwaysOn pins the always-on flip: an
// executor with a VsockDir assigns a vsock device on every dispatch, even
// one with no bridger/attach/net feature and no ports, so the agent
// always fetches its config over the control channel. The per-feature
// ports stay zero when their feature is off.
func TestHandleSandboxEventAssignsVsockAlwaysOn(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{
		Stdout:   io.Discard,
		Runner:   runner,
		VsockDir: testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-bare"}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-bare", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Vsock == nil {
		t.Fatal("cfg.Vsock not set for a featureless dispatch (always-on flip)")
	}
	if seen.Vsock.Attach || seen.Vsock.Net {
		t.Errorf("toggles = attach %v / net %v, want both off (no features)",
			seen.Vsock.Attach, seen.Vsock.Net)
	}
}

// TestHandleSandboxEventAssignsDistinctCIDsPerDispatch pins the
// counter behaviour: two dispatches get different GuestCIDs so two
// concurrent VMs don't collide on the AF_VSOCK ABI.
func TestHandleSandboxEventAssignsDistinctCIDsPerDispatch(t *testing.T) {
	var cfgs []sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		cfgs = append(cfgs, cfg)
		return nil
	}
	br := sandboxgateway.New()
	e, err := New(Config{
		Stdout:   io.Discard,
		Runner:   runner,
		Bridger:  br,
		VsockDir: testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i, uid := range []string{"sb-cid-1", "sb-cid-2"} {
		spec := &sandboxv1.SandboxSpec{
			WorkspaceUuid: fmt.Sprintf("ws-%d", i),
			Ports: []*sandboxv1.PortMapping{
				{ContainerPort: 80, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
			},
		}
		if err := e.HandleSandboxEvent(t.Context(), addedEvent(uid, spec)); err != nil {
			t.Fatalf("HandleSandboxEvent(%s): %v", uid, err)
		}
	}
	if len(cfgs) != 2 {
		t.Fatalf("captured %d cfgs, want 2", len(cfgs))
	}
	if cfgs[0].Vsock.GuestCID == cfgs[1].Vsock.GuestCID {
		t.Errorf("two dispatches got the same CID %d", cfgs[0].Vsock.GuestCID)
	}
}

// TestHandleSandboxEventRegistersAndUnregistersRoutes pins the
// Bridger lifecycle: during the dispatch, Bridge for the registered
// (ws,port) tuple finds a route; after the dispatch returns the
// route is gone (ErrNoRoute).
func TestHandleSandboxEventRegistersAndUnregistersRoutes(t *testing.T) {
	br := sandboxgateway.New()
	runner := noopRunner()
	e, err := New(Config{
		Stdout:   io.Discard,
		Runner:   runner,
		Bridger:  br,
		VsockDir: testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid: "ws-routes",
		Ports: []*sandboxv1.PortMapping{
			{ContainerPort: 8080, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
			{ContainerPort: 22, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-routes", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	// After dispatch returns the routes must be gone — a Bridge
	// call against the unregistered tuple errs with ErrNoRoute.
	upA, _ := net.Pipe()
	defer func() { _ = upA.Close() }()
	if err := br.Bridge(t.Context(), "ws-routes", 8080, upA); !errors.Is(err, sandboxgateway.ErrNoRoute) {
		t.Errorf("post-dispatch Bridge err = %v, want ErrNoRoute", err)
	}
}

// TestRegisterPortRoutesObservedDuringRun pins that the route is
// registered while the runner is executing. Uses a runner that
// calls Bridge.Bridge inline; without registration this would
// return ErrNoRoute.
func TestRegisterPortRoutesObservedDuringRun(t *testing.T) {
	br := sandboxgateway.New()
	var bridgeErr error
	runner := func(ctx context.Context, _ io.Writer, cfg sandbox.Config) error {
		// The DialFunc would normally dial CH; here we just verify
		// a route exists by calling Bridge with a Pipe that
		// immediately tears down.
		_ = cfg
		upA, upB := net.Pipe()
		_ = upA.Close()
		_ = upB.Close()
		bridgeErr = br.Bridge(ctx, "ws-live", 8080, upA)
		return nil
	}
	e, err := New(Config{
		Stdout:   io.Discard,
		Runner:   runner,
		Bridger:  br,
		VsockDir: testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid: "ws-live",
		Ports: []*sandboxv1.PortMapping{
			{ContainerPort: 8080, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-live", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	// During run, the route existed. Bridge wouldn't have returned
	// ErrNoRoute — it would have failed at the dial step (the
	// closure tries to DialGuestVsock against a non-existent UDS
	// path), which surfaces as a dial-error wrap.
	if errors.Is(bridgeErr, sandboxgateway.ErrNoRoute) {
		t.Errorf("Bridge returned ErrNoRoute during run; want dial-error wrap")
	}
}

// TestHandleSandboxEventSkipsRoutesWithoutWorkspaceUuid covers the
// smoke-test path: a spec with ports but no workspace_uuid is the
// orchestrator's "alive?" probe — no Bridger registration is
// possible without a workspace key, so the executor skips it
// silently.
func TestHandleSandboxEventSkipsRoutesWithoutWorkspaceUuid(t *testing.T) {
	br := sandboxgateway.New()
	runner := noopRunner()
	e, err := New(Config{
		Stdout:   io.Discard,
		Runner:   runner,
		Bridger:  br,
		VsockDir: testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		// no WorkspaceUuid
		Ports: []*sandboxv1.PortMapping{
			{ContainerPort: 80, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-anon", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	// Nothing should be registered — proxied by trying every plausible
	// (ws, port) tuple. Just spot-check the empty-workspace tuple.
	upA, _ := net.Pipe()
	defer func() { _ = upA.Close() }()
	if err := br.Bridge(t.Context(), "", 80, upA); !errors.Is(err, sandboxgateway.ErrNoRoute) {
		t.Errorf("err = %v, want ErrNoRoute (no registration possible without workspace)", err)
	}
}

// TestHandleSandboxEventNoBridgerLeavesCfgVsockNil pins the back-
// compat path: when no Bridger is configured, the executor must
// not touch cfg.Vsock — pre-C1.3b tests that don't set Bridger
// would otherwise see a surprise Vsock device on their config.
func TestHandleSandboxEventNoBridgerLeavesCfgVsockNil(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{Stdout: io.Discard, Runner: runner})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{
		WorkspaceUuid: "ws-noop",
		Ports: []*sandboxv1.PortMapping{
			{ContainerPort: 80, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-noop", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Vsock != nil {
		t.Errorf("Vsock = %+v, want nil (no Bridger configured)", seen.Vsock)
	}
}

// TestNewRejectsAttachRegistryWithoutVsockDir pins the same
// configuration invariant Bridger has: an AttachRegistry needs a
// VsockDir to mint per-Sandbox UDS files.
func TestNewRejectsAttachRegistryWithoutVsockDir(t *testing.T) {
	ar := sandboxgateway.NewAttachRegistry()
	if _, err := New(Config{Stdout: io.Discard, AttachRegistry: ar}); err == nil {
		t.Fatal("expected error when AttachRegistry is set but VsockDir is empty")
	}
	if _, err := New(Config{Stdout: io.Discard, AttachRegistry: ar, VsockDir: "relative"}); err == nil {
		t.Fatal("expected error when VsockDir is relative")
	}
}

func TestJobRegistryRequiresVsockAndEnablesGuestControl(t *testing.T) {
	jr := sandboxgateway.NewJobRegistry()
	if _, err := New(Config{Stdout: io.Discard, JobRegistry: jr}); err == nil {
		t.Fatal("expected error when JobRegistry is set but VsockDir is empty")
	}
	var seen sandbox.Config
	e, err := New(Config{
		Stdout: io.Discard,
		Runner: func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
			seen = cfg
			return nil
		},
		JobRegistry: jr,
		VsockDir:    testVsockDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-job", &sandboxv1.SandboxSpec{})); err != nil {
		t.Fatal(err)
	}
	if seen.Vsock == nil || !seen.Vsock.Attach {
		t.Fatalf("Vsock = %+v, want shared attach/job listener enabled", seen.Vsock)
	}
}

func TestRegisterJobRouteLifecycle(t *testing.T) {
	jr := sandboxgateway.NewJobRegistry()
	e, err := New(Config{Stdout: io.Discard, JobRegistry: jr, VsockDir: testVsockDir})
	if err != nil {
		t.Fatal(err)
	}
	cfg := sandbox.Config{Vsock: &sandbox.VsockConfig{
		HostSocket: filepath.Join(testVsockDir, "sb-job"), GuestCID: 3, Attach: true,
	}}
	unregister := e.registerJobRoute(cfg, "sb-job")
	if _, err := jr.Dial(t.Context(), "sb-job", agent.DefaultJobVsockPort); errors.Is(err, sandboxgateway.ErrJobNoRoute) {
		t.Fatalf("registered route was not found: %v", err)
	}
	unregister()
	if _, err := jr.Dial(t.Context(), "sb-job", agent.DefaultJobVsockPort); !errors.Is(err, sandboxgateway.ErrJobNoRoute) {
		t.Fatalf("route remained after unregister: %v", err)
	}
}

// TestHandleSandboxEventAssignsAttachPortAndVsock pins the
// attach-side allocation: when an AttachRegistry is wired, every
// dispatch gets a Vsock device with the attach feature enabled — even
// when the spec has no ports.
func TestHandleSandboxEventAssignsAttachPortAndVsock(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	ar := sandboxgateway.NewAttachRegistry()
	e, err := New(Config{
		Stdout:         io.Discard,
		Runner:         runner,
		AttachRegistry: ar,
		VsockDir:       testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// No ports → port-forward branch wouldn't allocate Vsock, but
	// the attach branch should.
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-attach"}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-attach", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Vsock == nil {
		t.Fatal("Vsock not set")
	}
	if !seen.Vsock.Attach {
		t.Error("Vsock.Attach = false, want true (AttachRegistry wired)")
	}
}

// TestHandleSandboxEventAssignsNetPortWhenNetEnabled pins the
// sandboxnet allocation: when NetEnabled is set, every dispatch gets a
// Vsock device with the net feature enabled.
func TestHandleSandboxEventAssignsNetPortWhenNetEnabled(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	e, err := New(Config{
		Stdout:     io.Discard,
		Runner:     runner,
		NetEnabled: true,
		VsockDir:   testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-net"}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-net", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if seen.Vsock == nil {
		t.Fatal("Vsock not set")
	}
	if !seen.Vsock.Net {
		t.Error("Vsock.Net = false, want true (NetEnabled set)")
	}
}

// TestHandleSandboxEventServesInStackProxy pins the J6 wiring: when an
// egress proxy is configured and the netstack is enabled, the dispatch
// config carries the proxy handler + fixed gateway port and flips to
// default-deny, so sandboxnet serves the proxy in-stack and drops every
// other egress.
func TestHandleSandboxEventServesInStackProxy(t *testing.T) {
	var seen sandbox.Config
	runner := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		seen = cfg
		return nil
	}
	asm := func(context.Context, string, sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		return sandbox.Config{Bundle: "/b"}, func() {}, nil
	}
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	e, err := New(Config{
		Stdout: io.Discard, Runner: runner, Assembler: asm,
		NetEnabled: true, VsockDir: testVsockDir,
		EgressProxy: &EgressProxy{Handler: handler, CACertPEM: []byte("CA")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{ImageRef: testImageRef}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-proxy-net", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if !seen.DenyEgress {
		t.Error("DenyEgress not set with an egress proxy + net enabled")
	}
	if seen.ProxyHandler == nil {
		t.Error("ProxyHandler not threaded into the boot config")
	}
	if seen.ProxyPort != sandboxnet.DefaultProxyPort {
		t.Errorf("ProxyPort = %d, want %d", seen.ProxyPort, sandboxnet.DefaultProxyPort)
	}
}

// TestNetEnabledRequiresVsockDir pins the validation rule mirroring
// Bridger / AttachRegistry: net listeners need a place to mint the
// per-Sandbox UDS path, so a missing VsockDir is a constructor
// error.
func TestNetEnabledRequiresVsockDir(t *testing.T) {
	_, err := New(Config{
		Stdout:     io.Discard,
		Runner:     func(context.Context, io.Writer, sandbox.Config) error { return nil },
		NetEnabled: true,
	})
	if err == nil {
		t.Fatal("expected error when NetEnabled is set without VsockDir")
	}
	if !strings.Contains(err.Error(), "VsockDir") {
		t.Fatalf("err = %v, want substring VsockDir", err)
	}
}

// TestHandleSandboxEventRegistersAndUnregistersAttachRoute pins
// the attach-route lifecycle: during the dispatch the route is
// reachable via the registry; after dispatch returns it's gone
// (Attach yields ErrAttachNoRoute).
func TestHandleSandboxEventRegistersAndUnregistersAttachRoute(t *testing.T) {
	ar := sandboxgateway.NewAttachRegistry()
	var attachErrDuringRun error
	runner := func(ctx context.Context, _ io.Writer, _ sandbox.Config) error {
		// Inside the run, Attach would normally dial the agent
		// vsock; here we just verify a route exists by calling
		// Attach. The dial will fail (UDS doesn't exist) but the
		// failure mode tells us the route was registered.
		_, attachErrDuringRun = ar.Attach(ctx, sandboxgateway.AttachRequest{SandboxUID: "sb-attached"})
		return nil
	}
	e, err := New(Config{
		Stdout:         io.Discard,
		Runner:         runner,
		AttachRegistry: ar,
		VsockDir:       testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-attach-lifecycle"}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-attached", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	// During run the registry knew the route — the failure must
	// not be ErrAttachNoRoute (dial-error wrap instead).
	if errors.Is(attachErrDuringRun, sandboxgateway.ErrAttachNoRoute) {
		t.Errorf("Attach returned ErrAttachNoRoute during run; want dial-error wrap")
	}
	// After dispatch returns the route is gone.
	if _, err := ar.Attach(t.Context(), sandboxgateway.AttachRequest{SandboxUID: "sb-attached"}); !errors.Is(err, sandboxgateway.ErrAttachNoRoute) {
		t.Errorf("post-dispatch Attach err = %v, want ErrAttachNoRoute", err)
	}
}

// TestRegisterAttachRouteDuplicateLogsAndSkipsRegistration pins
// the executor-bug-class branch: if a uid is already in the
// AttachRegistry (e.g. two concurrent dispatches racing on the
// same uid), Register returns ErrAttachRouteExists, the executor
// logs it to stdout, and the returned unregister is a no-op so
// the original registration outlives this dispatch's defer.
func TestRegisterAttachRouteDuplicateLogsAndSkipsRegistration(t *testing.T) {
	ar := sandboxgateway.NewAttachRegistry()
	// Pre-register sb-dup so the next registerAttachRoute call
	// races into the duplicate branch.
	originalDial := func(context.Context, sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error) {
		return nil, errors.New("original")
	}
	if err := ar.Register("sb-dup", originalDial); err != nil {
		t.Fatalf("pre-Register: %v", err)
	}
	var buf bytes.Buffer
	e, err := New(Config{
		Stdout:         &buf,
		Runner:         noopRunner(),
		AttachRegistry: ar,
		VsockDir:       testVsockDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spec := &sandboxv1.SandboxSpec{WorkspaceUuid: "ws-dup"}
	if err := e.HandleSandboxEvent(t.Context(), addedEvent("sb-dup", spec)); err != nil {
		t.Fatalf("HandleSandboxEvent: %v", err)
	}
	if !strings.Contains(buf.String(), "register attach route") {
		t.Errorf("stdout %q missing duplicate-route log", buf.String())
	}
	// Original registration must survive — the dispatch's defer
	// got a no-op unregister.
	if _, err := ar.Attach(t.Context(), sandboxgateway.AttachRequest{SandboxUID: "sb-dup"}); err == nil || errors.Is(err, sandboxgateway.ErrAttachNoRoute) {
		t.Errorf("Attach after duplicate-route dispatch: err = %v; original registration should still be reachable", err)
	}
}
