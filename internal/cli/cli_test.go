package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/version"
)

// runCmd executes the root command with the given args, capturing stdout and
// stderr. It returns the captured streams plus the Execute error so tests can
// assert on all three.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	err = Execute(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), err
}

func TestVersionSubcommandPrintsVersion(t *testing.T) {
	stdout, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatalf("daft version: unexpected error: %v", err)
	}
	got := strings.TrimSpace(stdout)
	if got != version.Version {
		t.Fatalf("daft version output = %q, want %q", got, version.Version)
	}
}

func TestVersionSubcommandRejectsArgs(t *testing.T) {
	_, _, err := runCmd(t, "version", "extra")
	if err == nil {
		t.Fatalf("daft version extra: want error for unexpected positional arg, got nil")
	}
}

// TestCompletionSubcommandRegistered verifies cobra's auto-generated
// `completion` sub-command is wired up. We don't try to execute the
// completion script generator here (that's cobra's responsibility) — we
// just confirm the sub-command exists and responds to --help, which is what
// users discover the command through.
func TestCompletionSubcommandRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "completion", "--help")
	if err != nil {
		t.Fatalf("daft completion --help: unexpected error: %v", err)
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		if !strings.Contains(stdout, shell) {
			t.Errorf("completion --help output missing shell %q\nfull output:\n%s", shell, stdout)
		}
	}
}

func TestCompletionGeneratesScript(t *testing.T) {
	stdout, _, err := runCmd(t, "completion", "bash")
	if err != nil {
		t.Fatalf("daft completion bash: unexpected error: %v", err)
	}
	if len(stdout) == 0 {
		t.Fatal("daft completion bash produced no output")
	}
}

func TestRootHelpRunsWithoutError(t *testing.T) {
	stdout, _, err := runCmd(t, "--help")
	if err != nil {
		t.Fatalf("daft --help: unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "daft") {
		t.Errorf("root help output should mention 'daft', got:\n%s", stdout)
	}
}

func TestUnknownCommandReturnsError(t *testing.T) {
	_, _, err := runCmd(t, "definitely-not-a-real-subcommand")
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
}

func TestVerboseFlagFromEnv(t *testing.T) {
	t.Setenv("DAFT_VERBOSE", "true")
	// We don't yet have a behavior driven by --verbose, so just confirm that
	// PersistentPreRunE accepts the env value without error.
	_, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatalf("with DAFT_VERBOSE=true: unexpected error: %v", err)
	}
}

func TestVerboseFlagRejectsBadEnv(t *testing.T) {
	t.Setenv("DAFT_VERBOSE", "not-a-bool")
	_, _, err := runCmd(t, "version")
	if err == nil {
		t.Fatal("with DAFT_VERBOSE=not-a-bool: want error, got nil")
	}
	if !strings.Contains(err.Error(), "DAFT_VERBOSE") {
		t.Errorf("error should mention DAFT_VERBOSE, got: %v", err)
	}
}

func TestVersionInvariantNonEmpty(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })

	version.Version = ""
	_, _, err := runCmd(t, "version")
	if err == nil {
		t.Fatal("expected error when version.Version is empty, got nil")
	}
}

// FuzzExecute exercises the CLI dispatcher with arbitrary argv slices.
//
// Go's native fuzzer (go test -fuzz=FuzzExecute) only supports primitive
// argument types, so we pass a single string and split on NUL to derive
// argv. This lets the fuzzer explore both single-arg invocations
// ("version") and multi-arg ones ("completion\x00bash", "-v\x00version", ...)
// without inventing a binary protocol.
//
// Invariant tested: Execute must never panic. *Errors* are fine — most
// random argv combinations correctly produce "unknown command" or a flag
// parse failure. We only fail the fuzz iteration if the process would
// crash (Go's fuzzer reports panics as failures automatically).
//
// Reproducers for any failure are written to
// testdata/fuzz/FuzzExecute/<hash> and become permanent regression cases
// replayed by every future `go test` run — commit those files.
//
// Run locally with:
//
//	go test -run='^$' -fuzz=FuzzExecute -fuzztime=30s ./internal/cli
//
// Without -fuzz, only the seed corpus below is exercised, behaving as
// regular table-driven test cases.
func FuzzExecute(f *testing.F) {
	seeds := []string{
		"",
		"version",
		"completion\x00bash",
		"completion\x00zsh",
		"completion\x00fish",
		"completion\x00powershell",
		"--help",
		"-v\x00version",
		"--verbose\x00version",
		"--unknown-flag",
		"definitely-not-a-real-subcommand",
		// Pathological seeds — long strings, embedded NULs at boundaries,
		// non-ASCII. The fuzzer mutates these into stranger inputs.
		"version\x00\x00\x00",
		"\x00",
		"日本語",
		strings.Repeat("a", 1024),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		// Hermetically clear the env-var that PersistentPreRunE consults,
		// otherwise host environment leakage could change behavior between
		// runs and turn the fuzzer non-deterministic.
		t.Setenv("DAFT_VERBOSE", "")

		var args []string
		if raw != "" {
			args = strings.Split(raw, "\x00")
		}

		// Errors are expected for ill-formed input; we only care that
		// Execute returns rather than panicking. Output is discarded —
		// we're testing the dispatcher, not the rendering.
		_ = Execute(args, io.Discard, io.Discard)
	})
}
