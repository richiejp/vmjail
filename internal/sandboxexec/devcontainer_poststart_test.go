package sandboxexec

import (
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/devcontainer"
)

func TestNormalizePostStart(t *testing.T) {
	got := normalizePostStart([]devcontainer.CommandEntry{
		{Shell: "bun run dev"},                // shell form → /bin/sh -c
		{Argv: []string{"npm", "install"}},    // argv form → passthrough
		{},                                    // empty → dropped
		{Name: "label", Argv: []string{"ok"}}, // named object entry → argv
	})
	if len(got) != 3 {
		t.Fatalf("want 3 commands, got %d: %v", len(got), got)
	}
	if strings.Join(got[0], " ") != "/bin/sh -c bun run dev" {
		t.Errorf("shell form not normalised: %v", got[0])
	}
	if strings.Join(got[1], " ") != "npm install" {
		t.Errorf("argv form not preserved: %v", got[1])
	}
	if strings.Join(got[2], " ") != "ok" {
		t.Errorf("named argv entry wrong: %v", got[2])
	}
}

func TestNormalizePostStartEmpty(t *testing.T) {
	if got := normalizePostStart(nil); got != nil {
		t.Errorf("nil hooks → %v, want nil", got)
	}
	if got := normalizePostStart([]devcontainer.CommandEntry{{}, {Shell: ""}}); got != nil {
		t.Errorf("all-blank hooks → %v, want nil", got)
	}
}

func TestDevcontainerArgs(t *testing.T) {
	if got := devcontainerArgs(false); got != nil {
		t.Fatalf("overrideCommand=false args = %v, want nil", got)
	}
	got := devcontainerArgs(true)
	want := []string{devcontainerShell, "-c", devcontainerKeepaliveShell}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("overrideCommand=true args = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if devcontainerArgs(true)[0] == "mutated" {
		t.Fatal("devcontainerArgs returned mutable package storage")
	}
}
