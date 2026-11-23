package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/cli"

	"github.com/richiejp/vmjail/internal/nsjail"
)

const testCommand = "true"

func TestMainCode(t *testing.T) {
	oldEnsureDelegated, oldReexecDelegated := ensureDelegated, reexecDelegated
	oldPrepareSubtree, oldNamespaceRun, oldRunLocal := prepareSubtree, namespaceRun, runLocal
	t.Cleanup(func() {
		ensureDelegated, reexecDelegated = oldEnsureDelegated, oldReexecDelegated
		prepareSubtree, namespaceRun, runLocal = oldPrepareSubtree, oldNamespaceRun, oldRunLocal
	})
	t.Setenv(privateMountsEnv, "")

	const help = "help"
	vmArgs := []string{"--hostname", help, "--", testCommand}
	var stderr bytes.Buffer
	run := func(args []string) int {
		stderr.Reset()
		return mainCode(context.Background(), args, nil, io.Discard, &stderr)
	}

	ensureDelegated = func(io.Writer) error { return errors.New("delegate") }
	for _, args := range [][]string{{"-h"}, {help}, {"version"}} {
		if code := run(args); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, &stderr)
		}
	}
	for _, args := range [][]string{{}, {"--invalid-option"}} {
		if code := run(args); code != 125 || strings.Contains(stderr.String(), "delegate") {
			t.Fatalf("%v exited %d: %s", args, code, &stderr)
		}
	}
	if code := run(vmArgs); code != 125 || !strings.Contains(stderr.String(), "delegate") {
		t.Fatalf("failed delegation exited %d: %s", code, &stderr)
	}

	ensureDelegated = func(io.Writer) error { return nil }
	prepareSubtree = func(string) (string, error) { return "", errors.New("subtree") }
	if code := run(vmArgs); code != 125 || !strings.Contains(stderr.String(), "subtree") {
		t.Fatalf("failed subtree preparation exited %d: %s", code, &stderr)
	}

	prepareSubtree = func(string) (string, error) { return "", syscall.EBUSY }
	reexecDelegated = func(io.Writer) error { return errors.New("reexec") }
	if code := run(vmArgs); code != 125 || !strings.Contains(stderr.String(), "reexec") {
		t.Fatalf("failed cgroup re-exec exited %d: %s", code, &stderr)
	}

	prepareSubtree = func(string) (string, error) { return "/cgroup", nil }
	namespaceRun = func(_ context.Context, _ []string, parent string, _ io.Reader, _, _ io.Writer) error {
		if parent != "/cgroup" {
			t.Fatalf("VM cgroup parent = %q, want /cgroup", parent)
		}
		return &cli.ExitError{Code: 7}
	}
	if code := run(vmArgs); code != 7 || stderr.Len() != 0 {
		t.Fatalf("workload status = %d: %s", code, &stderr)
	}

	t.Setenv(privateMountsEnv, "1")
	runLocal = func(_ context.Context, opts nsjail.Options, _ io.Reader, _, _ io.Writer) error {
		if opts.Hostname != help || len(opts.Command) != 1 || opts.Command[0] != testCommand {
			t.Fatalf("workload options = %+v", opts)
		}
		return nil
	}
	if code := run(vmArgs); code != 0 {
		t.Fatalf("mount namespace child exited %d: %s", code, &stderr)
	}
}

func TestNamespaceCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := namespaceCommand(context.Background(), []string{"--", testCommand}, "/cgroup")
	if cmd.Path != "/proc/self/exe" {
		t.Fatalf("command path = %q, want /proc/self/exe", cmd.Path)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "--" || cmd.Args[2] != testCommand {
		t.Fatalf("command arguments = %q", cmd.Args)
	}
	for _, want := range []string{privateMountsEnv + "=1", vmCgroupParentEnv + "=/cgroup"} {
		if !slices.Contains(cmd.Environ(), want) {
			t.Errorf("command environment is missing %q", want)
		}
	}
	if cmd.SysProcAttr == nil || cmd.Cancel == nil || cmd.WaitDelay == 0 {
		t.Fatal("namespace process lacks mount setup or cancellation handling")
	}
}

func TestWaitNamespace(t *testing.T) {
	for _, tc := range []struct {
		script string
		code   int
	}{
		{script: "exit 0"},
		{script: "exit 7", code: 7},
		{script: "kill -TERM $$", code: 143},
	} {
		t.Run(tc.script, func(t *testing.T) {
			// #nosec G204 -- test scripts are fixed above.
			cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", tc.script)
			err := waitNamespace(cmd)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}

			var status *cli.ExitError
			if !errors.As(err, &status) || status.Code != tc.code {
				t.Fatalf("workload status = %v, want %d", err, tc.code)
			}
		})
	}

	// #nosec G204 -- the executable is deliberately missing.
	cmd := exec.CommandContext(context.Background(), t.TempDir()+"/missing", "--")
	if err := waitNamespace(cmd); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing executable error = %v", err)
	}
}

func TestNamespaceCancellationSignalsChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Exercise the namespace command's cancellation with a shell that
	// acknowledges SIGTERM by exiting with a specific status.
	cmd := namespaceCommand(ctx, nil, "")
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"/bin/sh", "-c", "trap 'exit 42' TERM; echo ready; while :; do sleep 0.01; done"}
	cmd.SysProcAttr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var ready [6]byte
	_, readErr := io.ReadFull(stdout, ready[:])
	cancel()
	var exit *exec.ExitError
	err = cmd.Wait()
	if readErr != nil || string(ready[:]) != "ready\n" || !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("child readiness = %q (%v), exit = %v", ready, readErr, err)
	}
}

func TestMountNamespaceCanceledBeforeLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mountNamespace(ctx, nil, "", nil, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled namespace launch = %v, want context.Canceled", err)
	}
}
