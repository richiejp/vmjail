// Package testtmux is a black-box test harness that drives a real
// CLI through tmux. Tests write commands and assert on captured
// pane state, exactly as a human user would type and read at a
// terminal.
//
// Each Session owns its own tmux server (bound to a temp-dir UDS
// via -S) so a test never shares state with the user's running
// tmux or with parallel tests on the same host. The pane shell
// is dash — POSIX-strict, predictable signal handling, no
// readline / history quirks that bash and zsh inject.
//
// The pane is always a real PTY because tmux allocates one, so
// any CLI driven through this harness exercises its raw-mode +
// winsize code path just like in production.
//
// Outside the project's Nix dev shell tmux and dash may not be
// on PATH; running these tests in that case is unsupported by
// design, same convention as the KVM-required integration tests
// in internal/sandbox.
package testtmux

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PromptMarker is the PS1 the harness installs into the pane.
// Documented for reference; tests should not WaitFor it
// directly because the literal "DAFT$ " appears in the echoed
// `PS1='DAFT$ '` setup command. For per-command synchronisation
// use a unique sentinel echoed after the command (see the
// `printf 'DAFT-DONE-%d\n' $((…))` pattern in tests).
const PromptMarker = "DAFT$ "

const (
	paneCols = "200"
	paneRows = "50"

	waitForPoll  = 100 * time.Millisecond
	setupTimeout = 5 * time.Second

	// setupSentinel is the string the boot handshake waits for.
	// It's built via shell arithmetic so the output token does
	// not appear verbatim in the typed-command echo — a plain
	// `echo TESTTMUX-READY` would be matched by WaitFor on the
	// typed line before dash had actually run it.
	setupSentinel    = "TESTTMUX-READY-3"
	setupSentinelCmd = `printf 'TESTTMUX-READY-%d\n' $((1+2))`
)

// Session is a live tmux session running dash in a single pane.
type Session struct {
	t    *testing.T
	sock string
	name string
}

// New spawns a fresh tmux server under t.TempDir(), opens a
// session running dash, installs PromptMarker as the prompt, and
// returns the harness ready for Send/Enter/WaitFor calls.
// Registers a Cleanup that kill-servers the tmux process, so
// even a hard test failure leaves no pane behind.
func New(t *testing.T) *Session {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	name := sanitize(t.Name())

	mustRun(t, "tmux", "-S", sock, "new-session", "-d", "-s", name,
		"-x", paneCols, "-y", paneRows, "dash")
	//nolint:gosec // G204: sock is t.TempDir-rooted; test-only.
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })

	s := &Session{t: t, sock: sock, name: name}
	// PS1 must be set as a shell command — dash doesn't inherit
	// PS1 from env in interactive mode unless a profile script
	// forwards it, and pinning the shell via new-session's last
	// arg deliberately skips /etc/profile.
	s.Enter("PS1='" + PromptMarker + "'")
	s.Enter(setupSentinelCmd)
	s.WaitFor(setupSentinel, setupTimeout)
	return s
}

// Send dispatches arguments to tmux send-keys. Each arg is
// interpreted by tmux's vocabulary: key names like Enter or C-c
// are translated, plain text is sent literally. For text whose
// tokens could collide with key names use Enter instead.
func (s *Session) Send(keys ...string) {
	s.t.Helper()
	args := append([]string{"-S", s.sock, "send-keys", "-t", s.name}, keys...)
	mustRun(s.t, "tmux", args...)
}

// Enter sends line as literal text followed by an Enter key.
// Split into two send-keys invocations so tokens in line that
// happen to match tmux key names ("Enter", "C-c", "Escape", ...)
// are not reinterpreted.
func (s *Session) Enter(line string) {
	s.t.Helper()
	mustRun(s.t, "tmux", "-S", s.sock, "send-keys", "-t", s.name, "-l", "--", line)
	mustRun(s.t, "tmux", "-S", s.sock, "send-keys", "-t", s.name, "Enter")
}

// Capture returns the pane contents including scrollback history,
// so long-running command output is not lost when it scrolls
// past the visible region.
func (s *Session) Capture() string {
	s.t.Helper()
	//nolint:gosec // G204: sock + name are t.TempDir / t.Name; test-only.
	out, err := exec.Command("tmux", "-S", s.sock, "capture-pane",
		"-t", s.name, "-p", "-S", "-").Output()
	if err != nil {
		s.t.Fatalf("tmux capture-pane: %v", err)
	}
	return string(out)
}

// WaitFor polls Capture every waitForPoll until substr appears,
// or fails the test on timeout (dumping the pane state for
// diagnosis). Returns the matching capture so callers can
// inspect surrounding context.
func (s *Session) WaitFor(substr string, timeout time.Duration) string {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by deadline via the if-fatal below"
	for {
		got := s.Capture()
		if strings.Contains(got, substr) {
			return got
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("WaitFor %q: timeout after %s\n--- pane ---\n%s",
				substr, timeout, got)
		}
		time.Sleep(waitForPoll)
	}
}

// sanitize converts t.Name() into a tmux-safe session-name token.
// Subtest names contain "/", file-based test names can carry "."
// and ":", any of which tmux rejects in session names.
func sanitize(name string) string {
	return strings.NewReplacer("/", "-", ".", "-", ":", "-", " ", "-").Replace(name)
}

// mustRun runs prog with args and fatals the test on any error,
// dumping CombinedOutput so the tmux diagnostic is visible.
func mustRun(t *testing.T, prog string, args ...string) {
	t.Helper()
	//nolint:gosec // G204: callers in this package pass constants + t.TempDir / t.Name; test-only.
	out, err := exec.Command(prog, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", prog, strings.Join(args, " "), err, out)
	}
}
