//go:build linux && integration

package testdaemons_test

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceBuildahPullEgress is the #101 slice-3 egress spike:
// prove buildah can pull a base image `FROM <registry>` through daft's
// default-deny egress proxy — the last build-path unknown. It composes the
// buildah-build path (privileged-build grant) with the egress-proxy path
// (WithEgressProxy), so a real `FROM` pull leaves the VM only via the
// injected proxy.
//
// Hermetic, mirroring TestClusterWorkspaceEgressProxyEnv: a local OCI
// registry (go-containerregistry registry.New over httptest TLS) is bound
// to a NON-loopback host IP — Go's ProxyFromEnvironment bypasses the proxy
// for loopback, so a loopback registry would pull direct and never test
// the proxy. The guest's injected https_proxy routes the pull through the
// in-stack MITM proxy, which mints an IP-SAN leaf for the registry host.
//
// TLS is verified for real (no --tls-verify=false), so this exercises CA
// trust too: buildah/containers-image reads the system trust store
// (/etc/ssl/certs/ca-certificates.crt via Go's crypto/x509), NOT the
// SSL_CERT_FILE daft injects for nix/curl-style tools — so the
// daft-build-image ships that bundle and daft's installProxyCA overwrites
// it with the egress-proxy CA at boot, exactly as a real `FROM
// docker.io/...` pull through the proxy would resolve trust.
func TestClusterWorkspaceBuildahPullEgress(t *testing.T) {
	buildImgPath := requireEnvPath(t, "DAFT_BUILD_IMAGE",
		"nix build .#daft-build-image -o bin/daft-build-image")
	baseImgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	// Registry on a non-loopback host IP so the guest's env-proxy doesn't
	// bypass it (see hostNonLoopbackIPv4 / the EgressProxyEnv test).
	hostIP := hostNonLoopbackIPv4(t)
	baseRef := pushImageToHostIPRegistry(t, baseImgPath, hostIP, "test/d101-egress-base:v1")

	const (
		okSentinel   = "BUILDAH-PULL-EGRESS-OK"
		failSentinel = "BUILDAH-PULL-EGRESS-FAIL"
		runProof     = "d101-pull-egress-proof"
	)
	// No --tls-verify=false: buildah must validate the proxy's MITM leaf
	// against the system trust store, which the daft-build-image ships at
	// /etc/ssl/certs/ca-certificates.crt and daft's installProxyCA
	// overwrites with the egress-proxy CA at boot. The pull routes through
	// https_proxy (the registry host is non-loopback, so not in no_proxy).
	script := fmt.Sprintf(
		"set -e; export HOME=/tmp; mkdir -p /tmp/cstore /tmp/crun; cd /build/ctx; "+
			"if buildah --root /tmp/cstore --runroot /tmp/crun build "+
			"--isolation chroot --storage-driver vfs -t pulled -f Dockerfile . && "+
			"buildah --root /tmp/cstore --runroot /tmp/crun push pulled "+
			"docker-archive:/build/out/image.tar; "+
			"then echo %s; else echo %s; buildah --version; fi; exec sleep infinity",
		okSentinel, failSentinel,
	)

	buildTar := decompressDockerTarball(t, buildImgPath)
	builder, err := tarball.ImageFromPath(buildTar, nil)
	if err != nil {
		t.Fatalf("load build image %s: %v", buildTar, err)
	}
	img := withUserAndEntrypoint(t, builder, "", []string{"/bin/dash", "-c", script})
	ref := pushImageToRegistry(t, img, "test/d101-buildah-pull:v1")

	// FROM the proxied registry + an exec-form RUN proving the pulled
	// base's shell really ran.
	ctx := t.TempDir()
	writeFile(t, filepath.Join(ctx, "Dockerfile"), fmt.Sprintf(
		"FROM %s\nRUN [\"/bin/dash\", \"-c\", \"echo %s > /run-proof.txt\"]\n",
		baseRef, runProof,
	))
	out := t.TempDir()

	c := testdaemons.NewCluster(t, testdaemons.WithEgressProxy())
	s := testtmux.New(t)

	wsName := "d101-buildah-pull"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101P", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --preserve-command --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "ctx", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/ctx --source %s --read-only --name %s %s",
			prefix, ctx, wsName, orch,
		)},
		{Label: "out", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/out --source %s --name %s %s",
			prefix, out, wsName, orch,
		)},
		{Label: "build-caps", Cmd: fmt.Sprintf(
			"%s workspace privileged-build --enable --name %s %s", prefix, wsName, orch,
		)},
		// vfs copies each pulled layer on the RAM-backed /tmp; the
		// ~73 MiB base + its many layers needs headroom over the default.
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 4G --cpus 2 --name %s %s", prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, okSentinel, 240*time.Second)

	// The built image's layers must carry the RUN-created file — proving
	// the base was really pulled (through the proxy) and its shell ran.
	built, err := tarball.ImageFromPath(filepath.Join(out, "image.tar"), nil)
	if err != nil {
		t.Fatalf("load built image: %v", err)
	}
	if got := extractFileFromImage(t, built, "run-proof.txt"); strings.TrimSpace(got) != runProof {
		t.Fatalf("run-proof.txt = %q, want %q", got, runProof)
	}
}

// pushImageToHostIPRegistry stands up a hermetic OCI registry (httptest
// TLS, self-signed) bound to hostIP:0 and pushes the image at imgPath
// (a docker .tar.gz) to refName on it, returning the full "host:port/ref".
// Binding to a non-loopback host IP is what lets the guest's env-proxy
// route a FROM pull to it through the egress proxy.
func pushImageToHostIPRegistry(t *testing.T, imgPath, hostIP, refName string) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(registry.New())
	ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", hostIP, err)
	}
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL %q: %v", srv.URL, err)
	}
	tarPath := decompressDockerTarball(t, imgPath)
	im, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("load image %s: %v", tarPath, err)
	}
	ref := u.Host + "/" + refName
	// httptest's self-signed cert is valid for 127.0.0.1, not the
	// non-loopback host IP it's bound to, so the host-side push skips
	// verification. (The guest's pull goes through the proxy, whose
	// upstream leg is INSECURE_UPSTREAM, so it doesn't verify either.)
	insecure := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only local registry.
	if err := crane.Push(im, ref, crane.WithTransport(insecure)); err != nil {
		t.Fatalf("push base image %s: %v", ref, err)
	}
	return ref
}
