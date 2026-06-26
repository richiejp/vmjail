//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceDevcontainerBuildRunTLS is the PLAN 3e proof: a
// build-based devcontainer's RUN step performs a FULLY-VERIFIED HTTPS GET
// through the default-deny egress proxy, and succeeds — proving the egress
// proxy CA is trusted inside the buildah RUN container, not merely available
// to buildah itself for the FROM pull.
//
// Why this isolates the thing under test (the buildScript --volume overlay):
//
//   - buildah-the-process trusts the proxy CA for the FROM pull via
//     SSL_CERT_FILE on the build VM (proven separately by
//     TestClusterWorkspaceBuildahPullEgress). But RUN steps execute in the
//     built image's own rootfs with their own env, and buildah's --http-proxy
//     (default on) propagates ONLY the proxy vars into RUN — NOT SSL_CERT_FILE.
//     So a RUN-step TLS client cannot inherit daft's file-path CA env.
//   - The only way a RUN-step client trusts the proxy leaf is the overlay
//     buildScript adds when egress is active: --volume <egress-CA>:
//     /etc/ssl/certs/ca-certificates.crt:ro, mounted over the system trust
//     store for the RUN duration.
//   - daft-http-fetch (baked into the nix base) with no --insecure uses Go's
//     default TLS config, whose system cert pool reads exactly that Debian
//     path first. It routes through the proxy via the explicit --proxy guest
//     URL, so the variable left under test is solely "does the RUN container
//     trust the egress CA at the system path".
//
// The RUN writes daft-http-fetch's success line to /tls-proof.txt, baked into
// the image; the workload entrypoint cats it. Waiting for that line proves the
// verified-TLS-through-proxy GET returned 200 during the build — a positive,
// not merely no-harm, proof. A regression in the overlay drops the line (the
// build fails when daft-http-fetch exits non-zero on a cert error).
//
// Sibling of TestClusterWorkspaceDevcontainerBuild, which uses an echo RUN
// that needs no network and so cannot exercise CA trust.
func TestClusterWorkspaceDevcontainerBuildRunTLS(t *testing.T) {
	buildImgPath := requireEnvPath(t, "DAFT_BUILD_IMAGE",
		"nix build .#daft-build-image -o bin/daft-build-image")
	baseImgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	// Builder VM image: pulled host-side, so a loopback test registry is fine.
	buildTar := decompressDockerTarball(t, buildImgPath)
	builder, err := tarball.ImageFromPath(buildTar, nil)
	if err != nil {
		t.Fatalf("load build image %s: %v", buildTar, err)
	}
	buildRef := pushImageToRegistry(t, builder, "test/d101-runtls-builder:v1")

	// The base the Dockerfile builds FROM, on a NON-loopback host IP so both
	// the FROM pull and the RUN-step GET route through the egress proxy (Go's
	// ProxyFromEnvironment bypasses loopback). Its host:port doubles as the
	// RUN step's verified-HTTPS target — the registry /v2/ endpoint answers
	// 200, and the proxy mints an IP-SAN leaf for it, so the only thing that
	// can make the verified GET succeed is trusting the proxy CA.
	hostIP := hostNonLoopbackIPv4(t)
	baseRef := pushImageToHostIPRegistry(t, baseImgPath, hostIP, "test/d101-runtls-base:v1")
	regHost := strings.SplitN(baseRef, "/", 2)[0]

	c := testdaemons.NewCluster(t,
		testdaemons.WithEgressProxy(),
		testdaemons.WithBuildImage(buildRef))
	s := testtmux.New(t)

	// Written after NewCluster so the RUN step can name the guest-facing proxy
	// URL explicitly (--proxy), removing any dependence on buildah's proxy-var
	// propagation and pinning the test to the CA-trust question alone.
	content := t.TempDir()
	dcDir := filepath.Join(content, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o750); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	writeFile(t, filepath.Join(dcDir, "devcontainer.json"),
		`{"name":"d101-runtls","build":{"dockerfile":"Dockerfile"}}`)
	// Exec-form RUN/ENTRYPOINT: the nix base ships /bin/dash, not /bin/sh. The
	// RUN does a verified (no --insecure) HTTPS GET through the proxy and
	// records the result; a cert failure makes daft-http-fetch exit 1, so the
	// build fails and the workload never boots.
	writeFile(t, filepath.Join(dcDir, "Dockerfile"), fmt.Sprintf(
		"FROM %s\n"+
			"RUN [\"/bin/dash\", \"-c\", \"/bin/daft-http-fetch --proxy %s "+
			"--url=https://%s/v2/ > /tls-proof.txt 2>&1\"]\n"+
			"ENTRYPOINT [\"/bin/dash\", \"-c\", \"cat /tls-proof.txt; exec /bin/sleep infinity\"]\n",
		baseRef, c.EgressProxyURL, regHost,
	))

	wsName := "d101-runtls"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101T", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, content, wsName, orch,
		)},
		{Label: "devcontainer", Cmd: fmt.Sprintf(
			"%s workspace devcontainer .devcontainer/devcontainer.json --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	// Boots the builder VM, pulls a multi-layer base through the proxy, runs
	// the verified-TLS RUN step, then boots the built workload which cats the
	// proof. status=200 proves the RUN-step GET completed full TLS
	// verification against the proxy leaf — i.e. the egress CA was trusted at
	// the system path inside the RUN container.
	c.WaitForSandboxdOutput(t, "daft-http-fetch ok status=200", 300*time.Second)
}
