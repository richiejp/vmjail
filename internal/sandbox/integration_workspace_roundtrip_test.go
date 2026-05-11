//go:build linux && integration

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

// TestWorkspaceRoundTripE2E is a real-VM round-trip of the workspace
// push + pull-back pipeline. Unlike the integration test of the same
// name in internal/sandboxdcli, this one:
//
//   - boots a real Cloud Hypervisor VM against /dev/kvm
//   - runs a real container (daft-hello-container) inside it via crun
//   - lets the container itself write to /workspace via the rw virtiofs
//     share, instead of simulating that write from the host
//   - then serves the modified share back and pulls it as the user-host
//
// This is what the project calls "e2e" by the taxonomy in
// CONTRIBUTING.md: every component along the path is real.
//
// The flow:
//
//  1. user-host: workspace ./code with in.txt; runs workspace.Server
//     over mTLS in a goroutine.
//  2. sandbox-host: pulls workspace via workspacev1connect into a
//     tmpdir (simulating daft-sandboxd run --workspace-from).
//  3. sandbox-host: pulls a daft-hello-container OCI image from an
//     in-process registry, bundles it with /workspace ExtraMount.
//  4. sandbox-host: boots VM with two virtiofs shares — bundles +
//     workspace (rw). cfg.Bundle points at the bundle so the agent
//     execs crun on it.
//  5. The in-VM container reads /workspace/in.txt, appends a
//     marker, writes /workspace/out.txt, prints the banner, exits.
//  6. VM exits when the banner is seen; the modified file is on the
//     sandbox-host's tmpdir (because virtiofs share is rw).
//  7. sandbox-host: serves the modified tmpdir over mTLS.
//  8. pull-back-host: pulls into a separate dest dir, asserts that
//     /out.txt round-tripped both the original bytes and the
//     in-VM-applied suffix.
func TestWorkspaceRoundTripE2E(t *testing.T) {
	cfg := resolveArtifacts(t)

	// Step 1: user-host workspace and serve.
	userSrc := t.TempDir()
	const inputBody = "round-trip-via-real-vm"
	mustWriteFile(t, filepath.Join(userSrc, "in.txt"), []byte(inputBody))

	userCfgDir := t.TempDir()
	sbxCfgDir := t.TempDir()
	pullbackCfgDir := t.TempDir()

	userID := mustLoadOrGen(t, userCfgDir, identity.RoleClient)
	sbxID := mustLoadOrGen(t, sbxCfgDir, identity.RoleSandboxHost)
	pullbackID := mustLoadOrGen(t, pullbackCfgDir, identity.RoleClient)

	mutualApprove(t, userCfgDir, sbxID.Fingerprint())
	mutualApprove(t, sbxCfgDir, userID.Fingerprint())
	mutualApprove(t, sbxCfgDir, pullbackID.Fingerprint())
	mutualApprove(t, pullbackCfgDir, sbxID.Fingerprint())

	pushAddr := startWorkspaceServeOn(t, userCfgDir, userSrc, "ws")

	// Step 2: sandbox-host pulls the workspace into a tmpdir.
	workspaceTmp := t.TempDir()
	pullWorkspaceInto(t, sbxCfgDir, pushAddr, "ws", workspaceTmp)
	if got := mustRead(t, filepath.Join(workspaceTmp, "in.txt")); got != inputBody {
		t.Fatalf("post-push in.txt = %q, want %q", got, inputBody)
	}

	// The in-VM container runs as uid=0 with empty capabilities (the
	// hello-bundle's config.json drops everything). Without
	// CAP_DAC_OVERRIDE, root inside the container does not bypass DAC,
	// so a file owned by the host user with mode 0o600 is unreadable
	// from inside the VM. Make the workspace world-rw for the duration
	// of this test — production deployments will hand caps in via a
	// per-workspace policy.
	if err := os.Chmod(workspaceTmp, 0o777); err != nil { //nolint:gosec // see comment above
		t.Fatalf("Chmod workspaceTmp: %v", err)
	}
	if err := os.Chmod(filepath.Join(workspaceTmp, "in.txt"), 0o666); err != nil { //nolint:gosec // see comment above
		t.Fatalf("Chmod in.txt: %v", err)
	}

	// Step 3: pull + bundle the hello image; bundle with /workspace
	// ExtraMount so the in-VM crun bind-mounts the workspace share
	// into the container.
	_, ref := pushHelloIntegrationImage(t)
	store := newImagestore(t)
	pulled, err := store.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("imagestore.Pull: %v", err)
	}
	bundle, err := store.Bundle(t.Context(), pulled.Digest, imagestore.BundleOptions{
		ExtraMounts: []imagestore.BundleMount{{
			Destination: "/workspace",
			Type:        "bind",
			Source:      "/workspace",
			Options:     []string{"rbind", "rw"},
		}},
	})
	if err != nil {
		t.Fatalf("imagestore.Bundle: %v", err)
	}

	// Step 4: boot VM with bundles + workspace shares.
	cfg.Shares = []*ShareConfig{
		{
			HostDir:    filepath.Dir(bundle.Path),
			Tag:        "bundles",
			Mountpoint: "/bundles",
		},
		{
			HostDir:    workspaceTmp,
			Tag:        "workspace",
			Mountpoint: "/workspace",
		},
	}
	cfg.Bundle = filepath.Join("/bundles", filepath.Base(bundle.Path))

	var console bytes.Buffer
	if err := Hello(traceCtx(t), &console, cfg); err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, console.String())
	}

	// Step 6: the in-VM container's write should have landed on the
	// sandbox-host's tmpdir via the rw virtiofs share.
	outPath := filepath.Join(workspaceTmp, "out.txt")
	gotOut, err := os.ReadFile(outPath) //nolint:gosec // path under t.TempDir().
	if err != nil {
		t.Fatalf("read %s: %v\n--- console ---\n%s", outPath, err, console.String())
	}
	wantOut := inputBody + agent.WorkspaceSuffix
	if string(gotOut) != wantOut {
		t.Fatalf("post-VM out.txt = %q, want %q\n--- console ---\n%s",
			gotOut, wantOut, console.String())
	}

	// Step 7-8: sandbox-host serves the modified tmpdir back; the
	// pull-back peer reconstructs it and we re-verify.
	pullbackAddr := startWorkspaceServeOn(t, sbxCfgDir, workspaceTmp, "ws-modified")
	pullbackDest := t.TempDir()
	pullWorkspaceInto(t, pullbackCfgDir, pullbackAddr, "ws-modified", pullbackDest)

	if got := mustRead(t, filepath.Join(pullbackDest, "in.txt")); got != inputBody {
		t.Errorf("round-tripped in.txt = %q, want %q (unmodified file lost)", got, inputBody)
	}
	if got := mustRead(t, filepath.Join(pullbackDest, "out.txt")); got != wantOut {
		t.Errorf("round-tripped out.txt = %q, want %q", got, wantOut)
	}
}

func mustLoadOrGen(t *testing.T, dir string, role identity.Role) *identity.Identity {
	t.Helper()
	id, err := identity.LoadOrGenerate(dir, role)
	if err != nil {
		t.Fatalf("LoadOrGenerate(%s): %v", role, err)
	}
	return id
}

func mutualApprove(t *testing.T, dir string, fp identity.Fingerprint) {
	t.Helper()
	peers, err := identity.OpenPeerStore(filepath.Join(dir, "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := peers.Add(identity.Peer{Fingerprint: fp}); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // t.TempDir-rooted.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}

// startWorkspaceServeOn boots an mTLS workspace.Server in a goroutine
// using the identity + peer-store in cfgDir, exposing path under
// the workspace name. Returns the listen address; the listener is
// shut down on test cleanup.
func startWorkspaceServeOn(t *testing.T, cfgDir, path, name string) string {
	t.Helper()
	id, err := identity.LoadOrGenerate(cfgDir, identity.RoleClient)
	if err != nil {
		// Try the sandbox-host role: the sandbox-host calls this too.
		id, err = identity.LoadOrGenerate(cfgDir, identity.RoleSandboxHost)
		if err != nil {
			t.Fatalf("LoadOrGenerate: %v", err)
		}
	}
	peers, err := identity.OpenPeerStore(filepath.Join(cfgDir, "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	store, err := cas.Open(filepath.Join(cfgDir, "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	manifest, err := workspace.BuildManifest(path, store, workspace.BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	wsServer, err := workspace.NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := wsServer.Add(name, manifest); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	wsPath, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer)
	mux.Handle(wsPath, handler)
	srv, err := mtls.New(mtls.Config{
		Identity: id, Peers: peers, Addr: freeAddr(t), Handler: mux,
	})
	if err != nil {
		t.Fatalf("mtls.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if serr := srv.Serve(); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			t.Logf("Serve: %v", serr)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		wg.Wait()
	})
	return srv.Addr()
}

// pullWorkspaceInto dials addr with the identity/peer-store in
// cfgDir and reconstructs the named workspace at dest.
func pullWorkspaceInto(t *testing.T, cfgDir, addr, name, dest string) {
	t.Helper()
	var id *identity.Identity
	for _, role := range []identity.Role{identity.RoleSandboxHost, identity.RoleClient} {
		got, err := identity.LoadOrGenerate(cfgDir, role)
		if err == nil {
			id = got
			break
		}
	}
	if id == nil {
		t.Fatalf("LoadOrGenerate failed for all roles in %s", cfgDir)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(cfgDir, "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	store, err := cas.Open(filepath.Join(cfgDir, "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	client := workspacev1connect.NewWorkspaceServiceClient(httpc, "https://"+addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.GetManifest(ctx,
		connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: name}))
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := workspace.ApplyManifest(ctx, client, store, resp.Msg.GetManifest(), dest,
		workspace.ApplyOptions{}); err != nil {
		t.Fatalf("ApplyManifest: %v", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
