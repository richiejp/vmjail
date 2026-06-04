package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
)

// TestWaitForBootCompleteReturnsOnEvent: the agent reports boot-complete
// and prints its banner; waitForBootComplete drains the console up to the
// banner and returns nil with the console captured in w.
func TestWaitForBootCompleteReturnsOnEvent(t *testing.T) {
	pr, pw := io.Pipe()
	var buf bytes.Buffer
	bootComplete := make(chan struct{})
	close(bootComplete)

	go func() {
		_, _ = io.WriteString(pw, agent.Banner+"\n")
		_ = pw.Close()
	}()

	if err := waitForBootComplete(context.Background(), func() {}, &buf, pr, bootComplete); err != nil {
		t.Fatalf("waitForBootComplete: %v", err)
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Errorf("console not mirrored: %q", buf.String())
	}
}

// TestWaitForBootCompleteEarlyExit: the VM console EOFs without a banner
// or event (an early crash), so the scan returns ErrBannerNotSeen.
func TestWaitForBootCompleteEarlyExit(t *testing.T) {
	pr, pw := io.Pipe()
	_ = pw.Close() // immediate EOF, no banner
	var buf bytes.Buffer

	err := waitForBootComplete(context.Background(), func() {}, &buf, pr, make(chan struct{}))
	if !errors.Is(err, ErrBannerNotSeen) {
		t.Fatalf("got %v, want ErrBannerNotSeen", err)
	}
}

// TestWaitForBootCompleteEventThenTimeout: the event arrives but the
// banner never reaches the console, so the inner wait times out, kills
// the VM, and returns the ctx error.
func TestWaitForBootCompleteEventThenTimeout(t *testing.T) {
	pr, pw := io.Pipe() // stays open, no banner ever written
	var buf bytes.Buffer
	cancelVM := func() { _ = pw.Close() }
	bootComplete := make(chan struct{})
	close(bootComplete)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := waitForBootComplete(ctx, cancelVM, &buf, pr, bootComplete)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
}

// TestWaitForBootCompleteContextCancel: ctx fires before the event, so it
// kills the VM (closes the console writer) to unblock the scan and
// returns the ctx error.
func TestWaitForBootCompleteContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	var buf bytes.Buffer
	cancelVM := func() { _ = pw.Close() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForBootComplete(ctx, cancelVM, &buf, pr, make(chan struct{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
