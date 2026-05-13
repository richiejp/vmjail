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

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// loopbackAddrAny is the bind address used by every in-process
// orchestrator that this package spins up under test. Hoisted into
// a constant so goconst stops flagging the duplicate literal.
const loopbackAddrAny = "127.0.0.1:0"

// runOrchestratorWithSandboxRegistry boots an in-process orchestrator
// with an explicitly-supplied SandboxRegistry, runs seed against the
// live registry before serving starts, and returns the listening
// address + its identity.
func runOrchestratorWithSandboxRegistry(
	t *testing.T,
	srvStore *identity.PeerStore,
	seed func(*sandboxctrl.Registry),
) (addr string, srvID *identity.Identity) {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	reg := sandboxctrl.NewRegistry(nil)
	if seed != nil {
		seed(reg)
	}
	srv, err := orchestrator.New(orchestrator.Config{
		Identity:        srvID,
		Peers:           srvStore,
		Addr:            loopbackAddrAny,
		SandboxRegistry: reg,
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

// TestSandboxListSubcommandRegistered confirms the verb is wired
// into the `daft sandbox` subtree.
func TestSandboxListSubcommandRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "sandbox", "--help")
	if err != nil {
		t.Fatalf("daft sandbox --help: %v", err)
	}
	if !strings.Contains(stdout, sandboxListSubName) {
		t.Errorf("`daft sandbox --help` should advertise %q, got:\n%s", sandboxListSubName, stdout)
	}
}

// TestSandboxListRequiresOrchestratorFlag verifies the up-front guard
// — running without --orchestrator should fail fast, not dial.
func TestSandboxListRequiresOrchestratorFlag(t *testing.T) {
	_, _, err := runDaft(t, "sandbox", sandboxListSubName)
	if err == nil || !strings.Contains(err.Error(), "--orchestrator is required") {
		t.Errorf("err = %v, want --orchestrator required", err)
	}
}

// TestSandboxListRoundTripsAgainstOrchestrator exercises the full
// happy path: dial an in-process orchestrator over mTLS, hit
// ListSandboxes, render the table.
func TestSandboxListRoundTripsAgainstOrchestrator(t *testing.T) {
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
	addr, srvID := runOrchestratorWithSandboxRegistry(t, srvStore, func(reg *sandboxctrl.Registry) {
		reg.Register(
			fakeFingerprintForList(t),
			"v1.2.3",
			sandboxctrl.Resources{CPUs: 4, MemoryBytes: 2 << 30},
		)
	})
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	stdout, _, err := runDaft(
		t, "sandbox", sandboxListSubName,
		"--orchestrator", "https://"+addr,
		"--config-dir", clientDir,
		"--timeout", "3s",
	)
	if err != nil {
		t.Fatalf("sandbox list: %v", err)
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

// TestRunSandboxListPropagatesIdentityLoadError covers the
// loadOrGenerateClient failure branch — corrupt cert.pem aborts
// before any dial.
func TestRunSandboxListPropagatesIdentityLoadError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cert.pem"), "garbage")
	mustWrite(t, filepath.Join(dir, "key.pem"), "garbage")
	var out bytes.Buffer
	err := runSandboxList(context.Background(), &out, "https://127.0.0.1:1", dir, time.Now)
	if err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// TestRunSandboxListPropagatesPeerStoreError covers the peer-store
// load failure branch.
func TestRunSandboxListPropagatesPeerStoreError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateClient(dir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	mustWrite(t, filepath.Join(dir, peersFilename), "not json")
	var out bytes.Buffer
	err := runSandboxList(context.Background(), &out, "https://127.0.0.1:1", dir, time.Now)
	if err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}

// TestRunSandboxListPropagatesDialError covers the failure path
// when the orchestrator is unreachable.
func TestRunSandboxListPropagatesDialError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateClient(dir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := runSandboxList(ctx, &out, "https://127.0.0.1:1", dir, time.Now)
	if err == nil {
		t.Fatal("expected error dialing unreachable orchestrator")
	}
}

// TestRunSandboxListPropagatesStdoutWriteError covers writeSandboxList
// returning an io error on the table flush.
func TestRunSandboxListPropagatesStdoutWriteError(t *testing.T) {
	err := writeSandboxList(failingWriter{}, []*sandboxctrlv1.SandboxRegistration{
		{Fingerprint: "fp", Version: "v0", Resources: &sandboxctrlv1.HostResources{Cpus: 1, MemoryBytes: 0}, LastSeenAt: timestamppb.New(time.Unix(0, 0))},
	}, time.Unix(0, 0))
	if err == nil {
		t.Fatal("expected error from failing writer")
	}
}

// TestWriteSandboxListRendersAllRows pins the projection from the
// proto to the table.
func TestWriteSandboxListRendersAllRows(t *testing.T) {
	ref := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	rows := []*sandboxctrlv1.SandboxRegistration{
		{
			Fingerprint: "aaa",
			Version:     "v1",
			Resources:   &sandboxctrlv1.HostResources{Cpus: 2, MemoryBytes: 512 << 20},
			LastSeenAt:  timestamppb.New(ref.Add(-90 * time.Second)),
		},
		{
			// Empty version → "—"; zero LastSeenAt → "—".
			Fingerprint: "bbb",
			Resources:   &sandboxctrlv1.HostResources{Cpus: 1, MemoryBytes: 0},
		},
	}
	var buf bytes.Buffer
	if err := writeSandboxList(&buf, rows, ref); err != nil {
		t.Fatalf("writeSandboxList: %v", err)
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
// row makes it through ListSandboxes onto stdout.
func fakeFingerprintForList(t *testing.T) identity.Fingerprint {
	t.Helper()
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id.Fingerprint()
}
