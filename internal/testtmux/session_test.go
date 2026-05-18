package testtmux

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireTools fatals (not skips) if tmux or dash is missing.
// The project's Nix dev shell guarantees both; running outside
// it is unsupported, same convention as the KVM-requiring
// integration tests refuse to skip when /dev/kvm is missing.
// Skipping would zero the coverage gate.
func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"tmux", "dash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not on PATH (required by project dev shell): %v", bin, err)
		}
	}
}

// TestEnterRoundTripsCommand drives a literal command and waits
// for its output, the canonical use of the harness.
func TestEnterRoundTripsCommand(t *testing.T) {
	requireTools(t)
	s := New(t)
	s.Enter("echo hello-from-dash")
	got := s.WaitFor("hello-from-dash", 3*time.Second)
	if !strings.Contains(got, "hello-from-dash") {
		t.Errorf("pane missing hello: %s", got)
	}
}

// TestEnterPreservesKeyNameLiterals pins the split-send
// invariant: a line containing a token that matches a tmux key
// name ("Enter") must be transmitted verbatim, not reinterpreted.
func TestEnterPreservesKeyNameLiterals(t *testing.T) {
	requireTools(t)
	s := New(t)
	s.Enter("echo This is Enter literal")
	got := s.WaitFor("This is Enter literal", 3*time.Second)
	if !strings.Contains(got, "This is Enter literal") {
		t.Errorf("literal not preserved: %s", got)
	}
}

// TestSendDispatchesKeyNames exercises the key-vocabulary path:
// type a partial line, send C-u to clear it, then run a real
// command via shell arithmetic so the output token doesn't appear
// in either typed line. If Send didn't route C-u through tmux's
// key-name translation the partial echo would still execute and
// the "deleted" output would appear in the pane.
func TestSendDispatchesKeyNames(t *testing.T) {
	requireTools(t)
	s := New(t)
	s.Send("-l", "--", `printf 'DELETED-%d\n' $((1+2))`)
	s.Send("C-u")
	s.Enter(`printf 'KEPT-%d\n' $((4+5))`)
	s.WaitFor("KEPT-9", 3*time.Second)
	got := s.Capture()
	if strings.Contains(got, "DELETED-3") {
		t.Errorf("C-u failed to kill the line; pane: %s", got)
	}
}

// TestSanitizeReplacesUnsafeChars covers the t.Name() path —
// subtests give names like "TestX/sub/case" which tmux barfs on.
func TestSanitizeReplacesUnsafeChars(t *testing.T) {
	cases := map[string]string{
		"TestX":                "TestX",
		"TestX/sub":            "TestX-sub",
		"TestX/sub/case":       "TestX-sub-case",
		"pkg.subpkg:Test name": "pkg-subpkg-Test-name",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
