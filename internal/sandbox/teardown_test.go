package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/oops"
)

// safeBuf is a mutex-guarded io.Writer so a test can install it as the
// oops sink and poll it from the test goroutine while the watchdog
// goroutine writes to it, without racing.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestReapWithWatchdogWarnsWhileStuckThenReturns pins the mode-B
// behaviour: while the reap (cmd.Wait) is blocked — as it would be on a
// VMM wedged in uninterruptible kernel sleep — the watchdog keeps
// emitting a loud, attributable warning naming the process and pid, and
// never abandons the wait. Once the wait completes it returns promptly.
func TestReapWithWatchdogWarnsWhileStuckThenReturns(t *testing.T) {
	var buf safeBuf
	prev := oops.SetSink(&buf)
	defer oops.SetSink(prev)

	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Tiny intervals keep the test fast; first < max exercises the
		// doubling + cap branches across successive ticks.
		reapWithWatchdog("fake-vmm", 4242, time.Millisecond, 2*time.Millisecond,
			func() { <-release })
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "fake-vmm") {
		if time.Now().After(deadline) {
			t.Fatal("watchdog warning not emitted while reap blocked")
		}
		time.Sleep(time.Millisecond)
	}
	if got := buf.String(); !strings.Contains(got, "4242") ||
		!strings.Contains(got, "D-state") {
		t.Errorf("warning = %q, want it to name the pid and the D-state cause", got)
	}

	// The reap must still be running — the watchdog never gives up.
	select {
	case <-done:
		t.Fatal("reapWithWatchdog returned while the reap was still blocked")
	default:
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapWithWatchdog did not return after the wait completed")
	}
}

// TestReapWithWatchdogReturnsImmediatelyWhenWaitFast pins the common
// path: a wait that returns before the first watchdog interval emits no
// warning and returns at once.
func TestReapWithWatchdogReturnsImmediatelyWhenWaitFast(t *testing.T) {
	var buf safeBuf
	prev := oops.SetSink(&buf)
	defer oops.SetSink(prev)

	done := make(chan struct{})
	go func() {
		defer close(done)
		reapWithWatchdog("x", 1, time.Hour, time.Hour, func() {})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapWithWatchdog did not return promptly for a fast wait")
	}
	if s := buf.String(); s != "" {
		t.Errorf("unexpected watchdog warning for a fast reap: %q", s)
	}
}

// TestVMTeardownReapsSiblingsConcurrentlyDespiteWedge pins the
// no-stranding property: a proc stop wedged forever (a D-state VMM)
// does not prevent a healthy sibling's reap, run() blocks until every
// reap returns (it never abandons the wedge), and the func stops ran up
// front.
func TestVMTeardownReapsSiblingsConcurrentlyDespiteWedge(t *testing.T) {
	funcRan := make(chan struct{}, 1)
	var healthy atomic.Bool
	release := make(chan struct{})
	td := &vmTeardown{
		funcStops: []func(){func() { funcRan <- struct{}{} }},
		procStops: []func(){
			func() { <-release },           // wedged sibling
			func() { healthy.Store(true) }, // healthy sibling
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		td.run()
	}()

	select {
	case <-funcRan:
	case <-time.After(2 * time.Second):
		t.Fatal("func stop did not run")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !healthy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("healthy sibling not reaped while another proc stop was blocked")
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-done:
		t.Fatal("run() returned while a proc stop was still blocked")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after the wedge cleared")
	}
}

// TestVMTeardownRunsFuncStopsBeforeProcStops pins the phase ordering:
// the pure-Go receiver stops complete before any process reap begins.
func TestVMTeardownRunsFuncStopsBeforeProcStops(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	td := &vmTeardown{
		funcStops: []func(){func() { record("func") }},
		procStops: []func(){func() { record("proc") }},
	}
	td.run()
	if len(order) != 2 || order[0] != "func" || order[1] != "proc" {
		t.Errorf("teardown order = %v, want [func proc]", order)
	}
}

func TestBenignReapError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"exit error", &exec.ExitError{ProcessState: &os.ProcessState{}}, true},
		{"wait delay", exec.ErrWaitDelay, true},
		{"wrapped wait delay", fmt.Errorf("reap: %w", exec.ErrWaitDelay), true},
		{"ctx canceled", context.Canceled, true},
		{"ctx deadline", context.DeadlineExceeded, true},
		{"other", errors.New("ECHILD-like runtime failure"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := benignReapError(tc.err); got != tc.want {
				t.Errorf("benignReapError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestKillProcessGroupNilProcessNoop pins that group-killing an
// unstarted command is a no-op rather than a nil-deref.
func TestKillProcessGroupNilProcessNoop(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := killProcessGroup(cmd); err != nil {
		t.Errorf("killProcessGroup on an unstarted cmd = %v, want nil", err)
	}
}
