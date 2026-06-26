package workspacebroker

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// ErrNoSession is returned by Pair when the session_id is unknown
// (never reserved, expired, or already paired and consumed).
var ErrNoSession = errors.New("workspacebroker: no such session")

// ErrWrongPeer is returned by Pair when the caller's fingerprint
// isn't one of the two peers Reserve recorded for the session.
var ErrWrongPeer = errors.New("workspacebroker: peer not expected for session")

// DefaultSessionTTL is how long a Reserve()'d session waits for
// both peers to call Pair before being garbage-collected. Long
// enough to cover an orchestrator-broker→peer notify hop plus the
// peer dialling Relay; short enough that abandoned sessions don't
// accumulate if a peer crashes between RequestAttach and Relay.
const DefaultSessionTTL = 30 * time.Second

// DataStream is the transport-neutral facade onto one half of a
// brokered Relay byte channel. internal/orchestrator wraps the
// generated Connect bidi stream in an adapter. RecvData returns
// io.EOF on clean end-of-stream and any other non-nil error on
// transport failure; SendData propagates the underlying stream's
// error verbatim.
type DataStream interface {
	RecvData() ([]byte, error)
	SendData([]byte) error
}

// Pairing is what Pair returns once two peers have rendezvoused
// on a session_id. Partner is the other side's DataStream. Done
// is closed when either side's handler calls Close; the splice
// loop watches Done to abort an idle Receive when its partner has
// already exited.
type Pairing struct {
	Partner DataStream
	Done    <-chan struct{}
	// Close is idempotent. Each Relay handler must defer it on
	// exit so the partner's splice loop unblocks promptly.
	Close func()
}

// SessionBroker rendezvouses two Relay halves by session_id.
// Reserve is called by Matcher.RequestAttach after a session_id
// has been minted; Pair is called by each side's Relay handler
// once it's read its peer's Hello. The first to call Pair parks
// until the second arrives (or the reservation expires, or the
// caller's context is cancelled).
type SessionBroker struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	// expected is the set of fingerprints permitted to Pair this
	// session — populated by Reserve from the two peers known to
	// the matcher at attach time. v1 always has exactly two
	// distinct entries.
	expected [2]identity.Fingerprint
	parked   *parkedRelay
	expiry   time.Time
}

type parkedRelay struct {
	stream DataStream
	// ready receives the second arrival's DataStream. Buffered
	// (1) so the second arrival doesn't block under the lock.
	ready chan DataStream
	// done is closed when either side's handler exits. Shared by
	// both halves of the eventual Pairing.
	done chan struct{}
	// closeOnce guards done's close.
	closeOnce *sync.Once
}

// NewSessionBroker returns an empty broker with DefaultSessionTTL.
func NewSessionBroker() *SessionBroker {
	return &SessionBroker{
		ttl:      DefaultSessionTTL,
		now:      time.Now,
		sessions: map[string]*session{},
	}
}

// Reserve records a session_id and the two peer fingerprints that
// are expected to Pair. The session_id must have been minted by
// the caller (Matcher.RequestAttach) so it can be conveyed to
// both peers via their notify streams before either dials Relay.
// peerA and peerB may be equal (the common "attach my own
// workspace to my own sandbox" case is a same-identity session).
func (b *SessionBroker) Reserve(sessionID string, peerA, peerB identity.Fingerprint) error {
	if sessionID == "" {
		return errors.New("workspacebroker: empty session_id")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcLocked()
	if _, exists := b.sessions[sessionID]; exists {
		return errors.New("workspacebroker: session_id collision")
	}
	b.sessions[sessionID] = &session{
		expected: [2]identity.Fingerprint{peerA, peerB},
		expiry:   b.now().Add(b.ttl),
	}
	return nil
}

// Pair blocks until both peers have arrived on the same
// session_id, then returns a Pairing wrapping the partner's
// DataStream. The caller continues to own its own stream (mine);
// the splice loop reads from mine and writes to Partner.
//
// Returns ErrNoSession if session_id is unknown or expired,
// ErrWrongPeer if fp isn't one of the two expected peers. Context
// cancellation cancels the reservation and returns ctx.Err().
func (b *SessionBroker) Pair(ctx context.Context, sessionID string, fp identity.Fingerprint, mine DataStream) (*Pairing, error) {
	b.mu.Lock()
	sess, ok := b.sessions[sessionID]
	if !ok || b.now().After(sess.expiry) {
		if ok {
			delete(b.sessions, sessionID)
		}
		b.mu.Unlock()
		return nil, ErrNoSession
	}
	if fp != sess.expected[0] && fp != sess.expected[1] {
		b.mu.Unlock()
		return nil, ErrWrongPeer
	}
	if sess.parked != nil {
		// Second arrival: take the parked stream as our partner,
		// hand our own stream off via ready, share the done chan.
		// Note: a same-identity session (peerA == peerB) couples
		// fp with itself here. That's intentional — the orchestrator
		// can't tell which of two same-identity halves is which
		// (and doesn't need to: session_id is the credential, and
		// outer mTLS authenticated both peers).
		parked := sess.parked
		delete(b.sessions, sessionID)
		b.mu.Unlock()
		parked.ready <- mine
		return &Pairing{
			Partner: parked.stream,
			Done:    parked.done,
			Close:   makeCloser(parked.done, parked.closeOnce),
		}, nil
	}
	// First arrival: park and wait.
	ready := make(chan DataStream, 1)
	done := make(chan struct{})
	once := &sync.Once{}
	sess.parked = &parkedRelay{
		stream:    mine,
		ready:     ready,
		done:      done,
		closeOnce: once,
	}
	b.mu.Unlock()

	select {
	case partner := <-ready:
		return &Pairing{
			Partner: partner,
			Done:    done,
			Close:   makeCloser(done, once),
		}, nil
	case <-ctx.Done():
		b.cancelParked(sessionID, ready)
		return nil, ctx.Err()
	}
}

func makeCloser(done chan struct{}, once *sync.Once) func() {
	return func() {
		once.Do(func() { close(done) })
	}
}

func (b *SessionBroker) cancelParked(sessionID string, ready chan DataStream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sess, ok := b.sessions[sessionID]
	if !ok {
		return
	}
	if sess.parked != nil && sess.parked.ready == ready {
		delete(b.sessions, sessionID)
	}
}

// gcLocked sweeps expired reservations that nobody has Pair'd yet.
// Reservations whose first half has already parked are kept (the
// parked goroutine has its own ctx-cancel exit). Must be called
// under b.mu.
func (b *SessionBroker) gcLocked() {
	now := b.now()
	for sid, sess := range b.sessions {
		if now.After(sess.expiry) && sess.parked == nil {
			delete(b.sessions, sid)
		}
	}
}

// Count returns the number of active reservations; exposed for
// tests and a future debug surface. Not load-bearing for the
// pairing protocol.
func (b *SessionBroker) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

// Splice copies data from one DataStream to another until either
// side returns io.EOF, an error, the shared done channel closes,
// or ctx is cancelled. Returns nil on clean EOF or done; the
// underlying error on transport failures. Callers run this in
// each Relay handler with from=their own stream and to=partner's.
func Splice(ctx context.Context, from, to DataStream, done <-chan struct{}) error {
	// No ctx-cancellability assertion here: Splice's contract is
	// "terminate on EOF / recv error / done channel close / ctx
	// cancel" and any one of those suffices. Callers that don't
	// need ctx cancellation (typically tests that close `done`)
	// pass context.Background, which is a legitimate choice given
	// the alternative exit paths.
	type rcv struct {
		data []byte
		err  error
	}
	rcvCh := make(chan rcv, 1)
	startRecv := func() {
		go func() {
			data, err := from.RecvData()
			select {
			case rcvCh <- rcv{data, err}:
			case <-ctx.Done():
			case <-done:
			}
		}()
	}
	startRecv()
	//daft:lint:disable=forloopbound reason="exits on io.EOF / recv error, on done close, or on ctx.Done() — three independent termination paths, any one suffices"
	for {
		select {
		case r := <-rcvCh:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return nil
				}
				return r.err
			}
			if err := to.SendData(r.data); err != nil {
				return err
			}
			startRecv()
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
