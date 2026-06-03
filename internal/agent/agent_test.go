package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
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
