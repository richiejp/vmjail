package workspacebroker

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStream is a DataStream backed by two slice-channels, one
// for inbound (RecvData) and one for outbound (SendData). Used
// throughout sessions_test.go to drive Splice and Pair without
// pulling in the proto/Connect types.
type fakeStream struct {
	in     chan []byte // RecvData reads from this
	out    chan []byte // SendData writes to this
	closed atomic.Bool
}

func newFakeStream(bufIn, bufOut int) *fakeStream {
	return &fakeStream{
		in:  make(chan []byte, bufIn),
		out: make(chan []byte, bufOut),
	}
}

func (s *fakeStream) RecvData() ([]byte, error) {
	data, ok := <-s.in
	if !ok {
		return nil, io.EOF
	}
	return data, nil
}

func (s *fakeStream) SendData(data []byte) error {
	if s.closed.Load() {
		return errors.New("fakeStream: closed")
	}
	s.out <- data
	return nil
}

func TestReserveAndPairHappyPath(t *testing.T) {
	b := NewSessionBroker()
	if err := b.Reserve("sid-1", fp(1), fp(2)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if b.Count() != 1 {
		t.Errorf("Count = %d, want 1", b.Count())
	}

	aStream := newFakeStream(0, 0)
	bStream := newFakeStream(0, 0)

	type result struct {
		pairing *Pairing
		err     error
	}
	aDone := make(chan result, 1)
	go func() {
		p, err := b.Pair(context.Background(), "sid-1", fp(1), aStream)
		aDone <- result{p, err}
	}()
	// Give A time to park.
	time.Sleep(20 * time.Millisecond)

	bp, err := b.Pair(context.Background(), "sid-1", fp(2), bStream)
	if err != nil {
		t.Fatalf("B Pair: %v", err)
	}
	ar := <-aDone
	if ar.err != nil {
		t.Fatalf("A Pair: %v", ar.err)
	}
	// Partner crosswise:
	if ar.pairing.Partner != bStream {
		t.Errorf("A.Partner != bStream")
	}
	if bp.Partner != aStream {
		t.Errorf("B.Partner != aStream")
	}
	// Done channels are the same instance.
	if ar.pairing.Done != bp.Done {
		t.Errorf("Done channels diverge")
	}
	// Closing once closes both halves.
	ar.pairing.Close()
	select {
	case <-bp.Done:
	case <-time.After(time.Second):
		t.Fatal("B.Done never closed after A.Close")
	}
	// Idempotent.
	bp.Close()
	ar.pairing.Close()

	if b.Count() != 0 {
		t.Errorf("Count after pair = %d, want 0", b.Count())
	}
}

func TestPairSameIdentityAllowed(t *testing.T) {
	// Same-identity session: alice's own workspace → alice's own
	// sandbox. Both Relay halves carry alice's fingerprint.
	b := NewSessionBroker()
	alice := fp(7)
	if err := b.Reserve("sid", alice, alice); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	s1 := newFakeStream(0, 0)
	s2 := newFakeStream(0, 0)
	type result struct {
		pairing *Pairing
		err     error
	}
	done := make(chan result, 1)
	go func() {
		p, err := b.Pair(context.Background(), "sid", alice, s1)
		done <- result{p, err}
	}()
	time.Sleep(20 * time.Millisecond)
	p2, err := b.Pair(context.Background(), "sid", alice, s2)
	if err != nil {
		t.Fatalf("second Pair: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("first Pair: %v", r.err)
	}
	if r.pairing.Partner != s2 || p2.Partner != s1 {
		t.Error("partners not crossed for same-identity pair")
	}
}

func TestReserveRejectsBadInput(t *testing.T) {
	b := NewSessionBroker()
	if err := b.Reserve("", fp(1), fp(2)); err == nil {
		t.Error("empty session_id accepted")
	}
	if err := b.Reserve("a", fp(1), fp(2)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := b.Reserve("a", fp(3), fp(4)); err == nil {
		t.Error("duplicate session_id accepted")
	}
}

func TestPairUnknownSession(t *testing.T) {
	b := NewSessionBroker()
	_, err := b.Pair(context.Background(), "nope", fp(1), newFakeStream(0, 0))
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestPairWrongPeer(t *testing.T) {
	b := NewSessionBroker()
	_ = b.Reserve("sid", fp(1), fp(2))
	_, err := b.Pair(context.Background(), "sid", fp(99), newFakeStream(0, 0))
	if !errors.Is(err, ErrWrongPeer) {
		t.Errorf("err = %v, want ErrWrongPeer", err)
	}
}

func TestPairContextCancelReleasesParkedReservation(t *testing.T) {
	b := NewSessionBroker()
	_ = b.Reserve("sid", fp(1), fp(2))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.Pair(ctx, "sid", fp(1), newFakeStream(0, 0))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pair did not return after ctx cancel")
	}
	if b.Count() != 0 {
		t.Errorf("Count after cancel = %d, want 0", b.Count())
	}
}

func TestExpiredReservationIsSwept(t *testing.T) {
	b := NewSessionBroker()
	// Drive the clock manually.
	var clock atomic.Int64
	clock.Store(1000)
	b.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	b.ttl = 10 * time.Second
	if err := b.Reserve("sid", fp(1), fp(2)); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// Advance past TTL.
	clock.Add(11)
	// Pair should report ErrNoSession (treated as expired).
	_, err := b.Pair(context.Background(), "sid", fp(1), newFakeStream(0, 0))
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
	if b.Count() != 0 {
		t.Errorf("expired entry not deleted; Count = %d", b.Count())
	}
}

func TestExpiredReservationSweptOnNextReserve(t *testing.T) {
	b := NewSessionBroker()
	var clock atomic.Int64
	clock.Store(1000)
	b.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	b.ttl = 5 * time.Second
	_ = b.Reserve("old", fp(1), fp(2))
	clock.Add(10)
	// Reserving a new id triggers gcLocked, dropping the expired one.
	_ = b.Reserve("new", fp(3), fp(4))
	if b.Count() != 1 {
		t.Errorf("Count = %d, want 1 (old swept)", b.Count())
	}
}

// TestSpliceCopiesUntilEOF: when the source closes its in-channel,
// Splice returns nil and the destination has seen all frames.
func TestSpliceCopiesUntilEOF(t *testing.T) {
	from := newFakeStream(4, 0)
	to := newFakeStream(0, 4)
	from.in <- []byte("hello")
	from.in <- []byte("world")
	close(from.in)

	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- Splice(context.Background(), from, to, done)
	}()
	select {
	case got := <-to.out:
		if string(got) != "hello" {
			t.Errorf("first frame = %q, want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first frame never arrived")
	}
	select {
	case got := <-to.out:
		if string(got) != "world" {
			t.Errorf("second frame = %q, want world", got)
		}
	case <-time.After(time.Second):
		t.Fatal("second frame never arrived")
	}
	if err := <-errCh; err != nil {
		t.Errorf("Splice err = %v, want nil", err)
	}
}

func TestSpliceStopsOnDone(t *testing.T) {
	from := newFakeStream(0, 0) // blocks forever on RecvData
	to := newFakeStream(0, 0)
	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- Splice(context.Background(), from, to, done)
	}()
	time.Sleep(20 * time.Millisecond)
	close(done)
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Splice didn't exit on done")
	}
}

func TestSpliceStopsOnCtxCancel(t *testing.T) {
	from := newFakeStream(0, 0)
	to := newFakeStream(0, 0)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Splice(ctx, from, to, done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Splice didn't exit on ctx cancel")
	}
}

func TestSplicePropagatesSendError(t *testing.T) {
	from := newFakeStream(2, 0)
	to := newFakeStream(0, 0)
	to.closed.Store(true) // any Send will fail
	from.in <- []byte("x")
	done := make(chan struct{})
	err := Splice(context.Background(), from, to, done)
	if err == nil {
		t.Error("expected send error")
	}
}

// TestPairTwoConcurrentSessions verifies the broker handles two
// independent sessions without cross-talk.
func TestPairTwoConcurrentSessions(t *testing.T) {
	b := NewSessionBroker()
	_ = b.Reserve("s1", fp(1), fp(2))
	_ = b.Reserve("s2", fp(3), fp(4))

	streams := map[string]*fakeStream{
		"s1a": newFakeStream(0, 0), "s1b": newFakeStream(0, 0),
		"s2a": newFakeStream(0, 0), "s2b": newFakeStream(0, 0),
	}
	var wg sync.WaitGroup
	results := map[string]*Pairing{}
	var mu sync.Mutex
	run := func(sid string, peer byte, key string) {
		defer wg.Done()
		p, err := b.Pair(context.Background(), sid, fp(peer), streams[key])
		if err != nil {
			t.Errorf("Pair %s: %v", key, err)
			return
		}
		mu.Lock()
		results[key] = p
		mu.Unlock()
	}
	wg.Add(4)
	go run("s1", 1, "s1a")
	go run("s1", 2, "s1b")
	go run("s2", 3, "s2a")
	go run("s2", 4, "s2b")
	wg.Wait()

	if results["s1a"].Partner != streams["s1b"] {
		t.Error("s1a.Partner != s1b")
	}
	if results["s1b"].Partner != streams["s1a"] {
		t.Error("s1b.Partner != s1a")
	}
	if results["s2a"].Partner != streams["s2b"] {
		t.Error("s2a.Partner != s2b")
	}
	if results["s2b"].Partner != streams["s2a"] {
		t.Error("s2b.Partner != s2a")
	}
	// Done channels diverge between sessions.
	if results["s1a"].Done == results["s2a"].Done {
		t.Error("s1 and s2 share a Done chan")
	}
}
