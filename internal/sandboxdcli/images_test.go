package sandboxdcli

import (
	"strings"
	"testing"

	"github.com/richiejp/vmjail/internal/imagestore"
)

func TestImagesIsRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "images", "--help")
	if err != nil {
		t.Fatalf("vmjail-sandboxd images --help: %v", err)
	}
	for _, sub := range []string{"ls", "rm", "prune"} {
		if !strings.Contains(stdout, sub) {
			t.Errorf("`images --help` missing subcommand %q\n%s", sub, stdout)
		}
	}
}

func TestPullIsRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "--help")
	if err != nil {
		t.Fatalf("vmjail-sandboxd --help: %v", err)
	}
	if !strings.Contains(stdout, "pull") {
		t.Errorf("root help missing pull command:\n%s", stdout)
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in    string
		want  int64
		isErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"100", 100, false},
		{"4K", 4 << 10, false},
		{"4k", 4 << 10, false},
		{"50G", 50 << 30, false},
		{"1T", 1 << 40, false},
		{"-5", 0, true},
		{"5X", 0, true},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		got, err := parseBytes(tt.in)
		if tt.isErr {
			if err == nil {
				t.Errorf("parseBytes(%q): want error, got %d", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBytes(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseBytes(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// FuzzParseBytes exercises parseBytes with arbitrary inputs. The
// production caller reads from $VMJAIL_STORE_QUOTA — bytes-shaped
// untrusted input — so the parser must never panic on a malicious
// or just typo'd value.
//
// Run actively with:
//
//	go test -run='^$' -fuzz=FuzzParseBytes -fuzztime=30s ./internal/sandboxdcli
func FuzzParseBytes(f *testing.F) {
	for _, s := range []string{
		"", "0", "1", "100K", "50G", "1T", "10000000000000",
		"-5", "5x", "5KK", "K", "G", "abc", "100.5", "\x00",
		"1" + strings.Repeat("0", 100),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = parseBytes(raw)
	})
}

func TestStoreQuotaDefaultsTo50GiB(t *testing.T) {
	t.Setenv(EnvStoreQuota, "")
	q, err := storeQuota()
	if err != nil {
		t.Fatalf("storeQuota: %v", err)
	}
	if q != imagestore.DefaultQuota {
		t.Errorf("default quota = %d, want %d", q, imagestore.DefaultQuota)
	}
}

func TestStoreQuotaParsesEnv(t *testing.T) {
	t.Setenv(EnvStoreQuota, "10G")
	q, err := storeQuota()
	if err != nil {
		t.Fatalf("storeQuota: %v", err)
	}
	if q != 10<<30 {
		t.Errorf("quota = %d, want %d", q, int64(10)<<30)
	}
}

func TestOpenStoreRejectsBadQuota(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())
	t.Setenv(EnvStoreQuota, "not-a-number")
	if _, err := openStore(); err == nil {
		t.Fatal("openStore should reject malformed VMJAIL_STORE_QUOTA")
	}
}

func TestStoreRootHonoursEnv(t *testing.T) {
	t.Setenv(EnvStoreDir, "/tmp/forced/store")
	got, err := storeRoot()
	if err != nil {
		t.Fatalf("storeRoot: %v", err)
	}
	if got != "/tmp/forced/store" {
		t.Errorf("storeRoot = %q, want %q", got, "/tmp/forced/store")
	}
}

func TestStoreRootRejectsRelativeEnv(t *testing.T) {
	t.Setenv(EnvStoreDir, "relative/store")
	if _, err := storeRoot(); err == nil {
		t.Fatal("storeRoot should reject a relative VMJAIL_STORE_DIR")
	}
}

func TestImagesPruneRequiresOlderThan(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())
	_, _, err := runCmd(t, "images", "prune")
	if err == nil {
		t.Fatal("expected error when --older-than is missing")
	}
	if !strings.Contains(err.Error(), "older-than") {
		t.Errorf("err = %v, want mention of --older-than", err)
	}
}

func TestImagesLsOnEmptyStore(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())
	stdout, _, err := runCmd(t, "images", "ls")
	if err != nil {
		t.Fatalf("images ls: %v", err)
	}
	if !strings.Contains(stdout, "empty store") {
		t.Errorf("output = %q, want '(empty store)'", stdout)
	}
}

func TestImagesRmUnknownTargetErrors(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())
	_, _, err := runCmd(t, "images", "rm", "no-such-image")
	if err == nil {
		t.Fatal("expected error for unknown image")
	}
	if !strings.Contains(err.Error(), "no image matching") {
		t.Errorf("err = %v, want 'no image matching'", err)
	}
}

// Test fixtures shared across the resolveTarget tests below. Promoted
// to constants/helpers to satisfy goconst (the digest is the same
// across all four tests).
const (
	fixtureDigest      = "sha256:abc123"
	fixtureShortDigest = "sha256:abc"
	fixtureRef         = "foo:1"
)

func fixtureEntries() []imagestore.Entry {
	return []imagestore.Entry{{Digest: fixtureDigest, Refs: []string{fixtureRef}}}
}

func TestResolveTargetByDigestExact(t *testing.T) {
	got, ok := resolveTarget(fixtureEntries(), fixtureDigest)
	if !ok || got != fixtureDigest {
		t.Errorf("got=%q ok=%v, want %s true", got, ok, fixtureDigest)
	}
}

func TestResolveTargetByRef(t *testing.T) {
	got, ok := resolveTarget(fixtureEntries(), fixtureRef)
	if !ok || got != fixtureDigest {
		t.Errorf("got=%q ok=%v, want %s true", got, ok, fixtureDigest)
	}
}

func TestResolveTargetByDigestPrefix(t *testing.T) {
	got, ok := resolveTarget(fixtureEntries(), fixtureShortDigest)
	if !ok || got != fixtureDigest {
		t.Errorf("got=%q ok=%v, want digest match by prefix", got, ok)
	}
}

func TestResolveTargetMissReturnsFalse(t *testing.T) {
	if _, ok := resolveTarget(fixtureEntries(), "bar:2"); ok {
		t.Error("resolveTarget for unknown target returned ok=true")
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{1 << 30, "1.0 GB"},
		{1 << 40, "1.0 TB"},
		{1 << 50, "1.0 PB"},
	}
	for _, tt := range tests {
		got := humanBytes(tt.in)
		if got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShortDigest(t *testing.T) {
	// 19-char window = "sha256:" (7) + 12 hex chars; everything past
	// that is dropped.
	const truncated = "sha256:abcdef012345"
	tests := map[string]string{
		"":                           "",
		fixtureShortDigest:           fixtureShortDigest,
		truncated:                    truncated,
		truncated + "6789xxxxxxxxxx": truncated,
	}
	for in, want := range tests {
		got := shortDigest(in)
		if got != want {
			t.Errorf("shortDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintEntriesPopulated(t *testing.T) {
	entries := []imagestore.Entry{
		{
			Digest: "sha256:abc1234567890abcdef",
			Refs:   []string{"foo:1"},
			Size:   2048,
			Locked: true,
		},
		{
			Digest: "sha256:def4567890abcdef1234",
			// no refs → "(orphaned)"
			Size: 0,
		},
	}
	var buf strings.Builder
	if err := printEntries(&buf, entries); err != nil {
		t.Fatalf("printEntries: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "foo:1") {
		t.Errorf("output missing first ref: %q", out)
	}
	if !strings.Contains(out, "(orphaned)") {
		t.Errorf("output missing orphaned marker: %q", out)
	}
	if !strings.Contains(out, "[locked]") {
		t.Errorf("output missing [locked] flag: %q", out)
	}
	if !strings.Contains(out, "2.0 KB") {
		t.Errorf("output missing humanBytes for 2048: %q", out)
	}
}

// TestPullCommandReportsRegistryFailure exercises the pull subcommand
// with an unreachable registry, covering the error path.
func TestPullCommandReportsRegistryFailure(t *testing.T) {
	t.Setenv(EnvStoreDir, t.TempDir())
	_, _, err := runCmd(t, "pull", "127.0.0.1:1/no/such/image:tag")
	if err == nil {
		t.Fatal("expected pull failure against unreachable registry")
	}
}
