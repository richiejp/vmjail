package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
	"github.com/localai-org/darkaifactory/internal/orchestrator"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// loopbackAddrAny is the bind address used by every in-process
// orchestrator that this package spins up under test. Hoisted into
// a constant so goconst stops flagging the duplicate literal.
const loopbackAddrAny = "127.0.0.1:0"

// unreachableOrchestrator is a guaranteed-refused orchestrator URL
// used by negative-path tests; hoisted so goconst stops flagging it.
const unreachableOrchestrator = "https://127.0.0.1:1"

// runOrchestratorWithNodeRegistry boots an in-process orchestrator
// with an explicitly-supplied NodeRegistry, runs seed against the
// live registry before serving starts, and returns the listening
// address + its identity.
func runOrchestratorWithNodeRegistry(
	t *testing.T,
	srvStore *identity.PeerStore,
	seed func(*nodectrl.Registry),
) (addr string, srvID *identity.Identity) {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reg := nodectrl.NewRegistry(nil)
	if seed != nil {
		seed(reg)
	}
	srv, err := orchestrator.New(orchestrator.Config{
		Identity:     srvID,
		Peers:        srvStore,
		Addr:         loopbackAddrAny,
		NodeRegistry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv.Addr(), srvID
}

// TestNodeListSubcommandRegistered confirms the verb is wired
// into the `daft node` subtree.
func TestNodeListSubcommandRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "node", "--help")
	if err != nil {
		t.Fatalf("daft node --help: %v", err)
	}
	if !strings.Contains(stdout, nodeListSubName) {
		t.Errorf("`daft node --help` should advertise %q, got:\n%s", nodeListSubName, stdout)
	}
}

// TestNodeListRoundTripsAgainstOrchestrator exercises the full
// happy path: dial an in-process orchestrator over mTLS, hit
// ListNodes, render the table.
func TestNodeListRoundTripsAgainstOrchestrator(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	srvStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	addr, srvID := runOrchestratorWithNodeRegistry(t, srvStore, func(reg *nodectrl.Registry) {
		reg.Register(
			fakeFingerprintForList(t),
			"v1.2.3",
			nodectrl.Resources{CPUs: 4, MemoryBytes: 2 << 30},
		)
	})
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	stdout, _, err := runDaft(
		t, "node", nodeListSubName,
		"--orchestrator", "https://"+addr,
		"--config-dir", clientDir,
		"--timeout", "3s",
	)
	if err != nil {
		t.Fatalf("node list: %v", err)
	}
	if !strings.Contains(stdout, "FINGERPRINT") {
		t.Errorf("output missing header row, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "v1.2.3") {
		t.Errorf("output missing version, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2.0 GiB") {
		t.Errorf("output missing rendered memory, got:\n%s", stdout)
	}
}

// TestRunNodeListPropagatesIdentityLoadError covers the
// loadOrGenerateClient failure branch — corrupt cert.pem aborts
// before any dial.
func TestRunNodeListPropagatesIdentityLoadError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cert.pem"), "garbage")
	mustWrite(t, filepath.Join(dir, "key.pem"), "garbage")
	var out bytes.Buffer
	err := runNodeList(context.Background(), &out, unreachableOrchestrator, dir, time.Now)
	if err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// TestRunNodeListPropagatesPeerStoreError covers the peer-store
// load failure branch.
func TestRunNodeListPropagatesPeerStoreError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateClient(dir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	mustWrite(t, filepath.Join(dir, peersFilename), "not json")
	var out bytes.Buffer
	err := runNodeList(context.Background(), &out, unreachableOrchestrator, dir, time.Now)
	if err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}

// TestRunNodeListPropagatesDialError covers the failure path
// when the orchestrator is unreachable.
func TestRunNodeListPropagatesDialError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateClient(dir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := runNodeList(ctx, &out, unreachableOrchestrator, dir, time.Now)
	if err == nil {
		t.Fatal("expected error dialing unreachable orchestrator")
	}
}

// TestRunNodeListPropagatesStdoutWriteError covers writeNodeList
// returning an io error on the table flush.
func TestRunNodeListPropagatesStdoutWriteError(t *testing.T) {
	err := writeNodeList(failingWriter{}, []*nodectrlv1.Node{
		{Fingerprint: "fp", Version: "v0", Resources: &nodectrlv1.HostResources{Cpus: 1, MemoryBytes: 0}, LastSeenAt: timestamppb.New(time.Unix(0, 0))},
	}, time.Unix(0, 0))
	if err == nil {
		t.Fatal("expected error from failing writer")
	}
}

// TestWriteNodeListRendersAllRows pins the projection from the
// proto to the table.
func TestWriteNodeListRendersAllRows(t *testing.T) {
	ref := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	rows := []*nodectrlv1.Node{
		{
			Fingerprint: "aaa",
			Version:     "v1",
			Resources:   &nodectrlv1.HostResources{Cpus: 2, MemoryBytes: 512 << 20},
			LastSeenAt:  timestamppb.New(ref.Add(-90 * time.Second)),
		},
		{
			// Empty version → "—"; zero LastSeenAt → "—".
			Fingerprint: "bbb",
			Resources:   &nodectrlv1.HostResources{Cpus: 1, MemoryBytes: 0},
		},
	}
	var buf bytes.Buffer
	if err := writeNodeList(&buf, rows, ref); err != nil {
		t.Fatalf("writeNodeList: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"aaa", "bbb", "v1", "1m30s", "—", "512.0 MiB", "0 B"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, got:\n%s", want, out)
		}
	}
}

// TestFormatBytesUnits pins the rounding behaviour for each suffix
// boundary the column ever prints.
func TestFormatBytesUnits(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2 << 10, "2.0 KiB"},
		{2 << 20, "2.0 MiB"},
		{2 << 30, "2.0 GiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatAgeRanges pins each branch of the duration formatter.
func TestFormatAgeRanges(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{45 * time.Second, "45s"},
		{2*time.Minute + 5*time.Second, "2m5s"},
		{3*time.Hour + 4*time.Minute, "3h4m"},
		{48 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := formatAge(tc.in); got != tc.want {
			t.Errorf("formatAge(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTrimURLSchemeStrips covers all three branches.
func TestTrimURLSchemeStrips(t *testing.T) {
	cases := map[string]string{
		"https://host:1": "host:1",
		"http://host:2":  "host:2",
		"host:3":         "host:3",
	}
	for in, want := range cases {
		if got := trimURLScheme(in); got != want {
			t.Errorf("trimURLScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeFingerprintForList mints a fingerprint that doesn't have to
// belong to a connected identity — the test only checks that the
// row makes it through ListNodes onto stdout.
func fakeFingerprintForList(t *testing.T) identity.Fingerprint {
	t.Helper()
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id.Fingerprint()
}
