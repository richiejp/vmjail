package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
)

// TestMain doubles the test binary as a "fake crun" when invoked with
// the fakeCrunEnv var set — this lets execContainerLinux be covered
// by a unit test without needing the real crun binary or a privileged
// host. Mirrors the fake-VMM/fake-virtiofsd pattern in internal/sandbox.
//
// The check happens before m.Run, so flag.Parse never sees the
// `run --no-pivot --bundle ... <id>` argv we'd otherwise have to
// thread through the test binary's own flag set.
func TestMain(m *testing.M) {
	if v := os.Getenv(fakeCrunEnv); v != "" {
		runFakeCrun(v)
		return
	}
	// Shrink the config-fetch deadline so the production-path tests
	// (which call the real Run and dial a vsock host that doesn't
	// exist off-VM) degrade quickly instead of waiting the full
	// production timeout.
	configFetchTimeout = 200 * time.Millisecond
	os.Exit(m.Run())
}

// runFakeCrun emulates crun-as-child: write the requested stdout,
// then exit with the configured code. Defaults to 0.
func runFakeCrun(stdoutPayload string) {
	if stdoutPayload != "" {
		fmt.Println(stdoutPayload)
	}
	if v := os.Getenv(fakeCrunExitEnv); v != "" {
		code, err := strconv.Atoi(v)
		if err == nil {
			os.Exit(code)
		}
	}
}

func TestRunPrintsBannerAndBlocksUntilContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	// Drive runWith with an empty cmdline and zero-valued deps: every
	// share/gadget/forward/container path is gated on a cmdline token
	// or a non-nil dep, so none fire and the only thing written to w is
	// the banner. This makes the banner+block contract a test of Run's
	// own behaviour rather than of the ambient /proc/cmdline. The public
	// Run reads the real cmdline, which carries daft.* tokens when the
	// suite runs *inside* a daft sandbox VM (the D6c.4 dogfood); their
	// operational output would otherwise pollute this exact-match.
	err := runWith(ctx, &buf, runDeps{
		readCmdline: func() (string, error) { return "", nil },
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWith returned %v, want DeadlineExceeded", err)
	}
	if got := strings.TrimRight(buf.String(), "\n"); got != Banner {
		t.Fatalf("banner = %q, want %q", got, Banner)
	}
}

// TestRunDegradesToBannerWhenConfigFetchFails asserts the failure
// contract: when the config channel can't be reached (the fetch errors
// — no vsock device, AF_VSOCK seccomp-denied, wedged listener), the
// agent reports it, runs no container, and blocks until ctx fires.
// With an empty cmdline there's nothing to fall back to, so a clean
// banner-only boot is the result.
func TestRunDegradesToBannerWhenConfigFetchFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	containerRan := false
	err := runWith(ctx, &buf, runDeps{
		readCmdline: func() (string, error) { return "", nil },
		openControl: func(context.Context) (controlChannel, error) {
			return nil, errors.New("dial config port: no vsock device")
		},
		container: ContainerDeps{
			Exec: func(context.Context, io.Writer, string) error {
				containerRan = true
				return nil
			},
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWith returned %v, want DeadlineExceeded", err)
	}
	if containerRan {
		t.Error("container ran despite config-fetch failure")
	}
	out := buf.String()
	if !strings.Contains(out, ConfigReportPrefix) {
		t.Errorf("missing config report line in %q", out)
	}
	if !strings.Contains(out, Banner) {
		t.Errorf("missing banner in %q", out)
	}
}

// TestRunAppliesConfigBundleAndShares asserts the happy path: settings
// delivered over the config channel (not the cmdline — which is empty
// here) are what the agent mounts and runs.
func TestRunAppliesConfigBundleAndShares(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	var mountedTargets []string
	var ranBundle string
	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{
		Bundle: testBundlePath,
		Shares: []*agentv1.GuestConfig_Share{{Tag: testTag, Mountpoint: testMountpoint}},
	}}
	err := runWith(ctx, &buf, runDeps{
		readCmdline: func() (string, error) { return "", nil },
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
		share: ShareDeps{
			Mount:    func(_, target, _, _ string) error { mountedTargets = append(mountedTargets, target); return nil },
			MkdirAll: func(string, os.FileMode) error { return nil },
			ReadFile: func(string) ([]byte, error) { return nil, nil },
		},
		container: ContainerDeps{
			Exec: func(_ context.Context, _ io.Writer, bundle string) error { ranBundle = bundle; return nil },
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWith returned %v, want DeadlineExceeded", err)
	}
	if ranBundle != testBundlePath {
		t.Errorf("ran bundle %q, want %q", ranBundle, testBundlePath)
	}
	if len(mountedTargets) != 1 || mountedTargets[0] != testMountpoint {
		t.Errorf("mounted %v, want [%q]", mountedTargets, testMountpoint)
	}
	if len(ctrl.events) != 1 || ctrl.events[0] != agentv1.AgentEvent_BOOT_COMPLETE {
		t.Errorf("events = %v, want one BOOT_COMPLETE", ctrl.events)
	}
	if !ctrl.closed {
		t.Error("control channel not closed")
	}
}

// TestRunReportsBootCompleteSendError covers the best-effort error path
// when streaming the boot-complete event fails (a half-open channel):
// the agent reports it and keeps running, not aborting.
func TestRunReportsBootCompleteSendError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	ctrl := &fakeControl{cfg: &agentv1.GuestConfig{}, sendErr: errors.New("send boom")}
	err := runWith(ctx, &buf, runDeps{
		readCmdline: func() (string, error) { return "", nil },
		openControl: func(context.Context) (controlChannel, error) { return ctrl, nil },
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWith returned %v, want DeadlineExceeded", err)
	}
	if !strings.Contains(buf.String(), ConfigReportPrefix+"send boot-complete:") {
		t.Errorf("missing boot-complete send-error report in %q", buf.String())
	}
}

// fakeControl is a test controlChannel: it records the events sent and
// whether it was closed, serves a fixed GuestConfig, and can fail sends.
type fakeControl struct {
	cfg     *agentv1.GuestConfig
	events  []agentv1.AgentEvent_Kind
	closed  bool
	sendErr error
}

func (f *fakeControl) Config() *agentv1.GuestConfig { return f.cfg }

func (f *fakeControl) SendEvent(k agentv1.AgentEvent_Kind) error {
	f.events = append(f.events, k)
	return f.sendErr
}

func (f *fakeControl) Close() error {
	f.closed = true
	return nil
}

func TestRunReturnsImmediatelyIfContextAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	err := Run(ctx, &buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want Canceled", err)
	}
	// Banner is printed unconditionally before the Done check, so an
	// already-cancelled context still yields the banner. We assert this so
	// the integration test can rely on the banner appearing even if the
	// host signals shutdown unusually quickly.
	if !strings.Contains(buf.String(), Banner) {
		t.Fatalf("banner missing from output: %q", buf.String())
	}
}

func TestRunPropagatesWriteError(t *testing.T) {
	err := Run(t.Context(), errWriter{})
	if err == nil {
		t.Fatal("Run with failing writer returned nil, want error")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestMainReturnsZeroOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var out, errOut bytes.Buffer
	code := Main(ctx, &out, &errOut)
	if code != 0 {
		t.Fatalf("Main exit code = %d, want 0 on cancellation", code)
	}
	if !strings.Contains(out.String(), Banner) {
		t.Fatalf("stdout missing banner: %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr non-empty on graceful shutdown: %q", errOut.String())
	}
}

func TestMainReturnsOneOnRunError(t *testing.T) {
	var errOut bytes.Buffer
	code := Main(t.Context(), errWriter{}, &errOut)
	if code != 1 {
		t.Fatalf("Main exit code = %d, want 1 when Run fails", code)
	}
	if !strings.Contains(errOut.String(), "daft-agent:") {
		t.Fatalf("stderr should report agent error, got: %q", errOut.String())
	}
}
