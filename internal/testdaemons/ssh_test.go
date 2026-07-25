//go:build linux && integration

package testdaemons_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
)

func TestSSHServer(t *testing.T) {
	server := testdaemons.NewSSHServer(t)
	args := append(server.ClientArgs(), "printf SSH-FIXTURE-OK")
	command := exec.CommandContext(t.Context(), "ssh", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("SSH fixture command: %v\n%s", err, output)
	}
	if strings.TrimSpace(string(output)) != "SSH-FIXTURE-OK" {
		t.Fatalf("SSH fixture output = %q", output)
	}
}
