package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunPrintsBannerAndBlocksUntilContextCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	err := Run(ctx, &buf)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want DeadlineExceeded", err)
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
