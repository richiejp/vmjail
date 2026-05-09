package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// fakeCrunEnv is the env var that turns the test binary into a fake
// crun for the duration of one exec — see runFakeCrun. Tests set the
// env var, point execContainerLinux at os.Args[0], and assert that
// the Wait/stdout-pipe path is exercised end-to-end without needing
// the real crun binary or root.
const fakeCrunEnv = "DAFT_TEST_FAKE_CRUN_OUTPUT"

// fakeCrunExitEnv asks the fake crun to exit non-zero so the
// "wait: exit status N" error path is exercised. The value is the
// desired exit code as a literal integer.
const fakeCrunExitEnv = "DAFT_TEST_FAKE_CRUN_EXIT"

// fakeContainerDeps records exec invocations and lets tests script
// stdout / errors per call.
type fakeContainerDeps struct {
	gotBundle string
	stdout    string
	err       error
	calls     int
}

func (f *fakeContainerDeps) toContainerDeps() ContainerDeps {
	return ContainerDeps{
		Exec: func(_ context.Context, w io.Writer, bundle string) error {
			f.calls++
			f.gotBundle = bundle
			if f.stdout != "" {
				_, _ = io.WriteString(w, f.stdout)
			}
			return f.err
		},
	}
}

// withTestRuntime swaps ContainerRuntime for the duration of t to point
// at the given binary. Used by execContainerLinux tests that re-exec
// the test binary as a fake crun.
func withTestRuntime(t *testing.T, path string) {
	t.Helper()
	orig := ContainerRuntime
	ContainerRuntime = path
	t.Cleanup(func() { ContainerRuntime = orig })
}

// TestExecContainerLinuxViaFakeBinary points execContainerLinux at the
// test binary itself, which TestMain reroutes into a fake crun. Covers
// the production exec wiring (StdoutPipe, prefixCopy, cmd.Wait) without
// needing the real OCI runtime or a privileged host.
func TestExecContainerLinuxViaFakeBinary(t *testing.T) {
	withTestRuntime(t, os.Args[0])
	t.Setenv(fakeCrunEnv, "stdout-from-fake-crun")

	var buf bytes.Buffer
	if err := execContainerLinux(t.Context(), &buf, "/unused-bundle"); err != nil {
		t.Fatalf("execContainerLinux: %v", err)
	}
	if !strings.Contains(buf.String(), ContainerReportPrefix+"stdout-from-fake-crun") {
		t.Errorf("output missing prefixed line: %q", buf.String())
	}
}

func TestExecContainerLinuxReportsNonZeroExit(t *testing.T) {
	withTestRuntime(t, os.Args[0])
	t.Setenv(fakeCrunEnv, "before-exit")
	t.Setenv(fakeCrunExitEnv, "42")

	var buf bytes.Buffer
	err := execContainerLinux(t.Context(), &buf, "/unused")
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "wait:") {
		t.Errorf("err = %v, want wait: prefix", err)
	}
	// Stdout written before the exit should still have been forwarded.
	if !strings.Contains(buf.String(), "before-exit") {
		t.Errorf("output missing pre-exit stdout: %q", buf.String())
	}
}

func TestExecContainerLinuxStartFailureReportsError(t *testing.T) {
	withTestRuntime(t, "/nonexistent/crun")

	var buf bytes.Buffer
	err := execContainerLinux(t.Context(), &buf, "/b")
	if err == nil {
		t.Fatal("expected error for missing runtime binary")
	}
	if !strings.Contains(err.Error(), "start:") {
		t.Errorf("err = %v, want start: prefix", err)
	}
}

func TestRunContainerHappyPath(t *testing.T) {
	f := &fakeContainerDeps{stdout: ContainerReportPrefix + "hi\n"}

	var buf bytes.Buffer
	RunContainer(t.Context(), &buf, f.toContainerDeps(), testBundlePath)

	if f.calls != 1 {
		t.Fatalf("Exec called %d times, want 1", f.calls)
	}
	if f.gotBundle != testBundlePath {
		t.Errorf("Exec bundle = %q, want %q", f.gotBundle, testBundlePath)
	}
	if !strings.Contains(buf.String(), ContainerReportPrefix+"hi") {
		t.Errorf("output missing container line: %q", buf.String())
	}
}

func TestRunContainerReportsExecError(t *testing.T) {
	f := &fakeContainerDeps{err: errors.New("crun bang")}

	var buf bytes.Buffer
	RunContainer(t.Context(), &buf, f.toContainerDeps(), testBundlePath)

	if !strings.Contains(buf.String(), ContainerReportPrefix+"exec: crun bang") {
		t.Errorf("output missing exec error: %q", buf.String())
	}
}

// TestRunWithBundleCmdlineExecsContainer wires the runWith deps to a
// fake cmdline + container and asserts the cmdline → exec → report
// path runs once with the parsed bundle path.
func TestRunWithBundleCmdlineExecsContainer(t *testing.T) {
	f := &fakeContainerDeps{stdout: ContainerReportPrefix + "from-the-fake\n"}

	deps := runDeps{
		readCmdline: func() (string, error) {
			return "console=hvc0 daft.bundle=/share/bundle", nil
		},
		share:     newFakeShareDeps().toShareDeps(),
		container: f.toContainerDeps(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if f.calls != 1 {
		t.Errorf("container Exec called %d times, want 1", f.calls)
	}
	if f.gotBundle != testBundlePath {
		t.Errorf("bundle = %q, want /share/bundle", f.gotBundle)
	}
	if !strings.Contains(buf.String(), Banner) {
		t.Errorf("banner missing: %q", buf.String())
	}
	if !strings.Contains(buf.String(), ContainerReportPrefix+"from-the-fake") {
		t.Errorf("container output missing: %q", buf.String())
	}
}

func TestRunWithMalformedBundleReportsError(t *testing.T) {
	f := &fakeContainerDeps{}
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "daft.bundle=relative-path", nil
		},
		share:     newFakeShareDeps().toShareDeps(),
		container: f.toContainerDeps(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if f.calls != 0 {
		t.Errorf("Exec must not be called for malformed bundle, got %d calls", f.calls)
	}
	if !strings.Contains(buf.String(), ContainerReportPrefix+"cmdline:") {
		t.Errorf("expected cmdline error line: %q", buf.String())
	}
}

func TestRunWithoutBundleSkipsContainer(t *testing.T) {
	f := &fakeContainerDeps{}
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "console=hvc0", nil // no daft.bundle
		},
		share:     newFakeShareDeps().toShareDeps(),
		container: f.toContainerDeps(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if f.calls != 0 {
		t.Errorf("Exec called %d times, want 0 when daft.bundle absent", f.calls)
	}
	if strings.Contains(buf.String(), ContainerReportPrefix) {
		t.Errorf("unexpected container line: %q", buf.String())
	}
}
