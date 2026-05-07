package cli

import (
	"bytes"
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
