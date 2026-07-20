package nodejob

import (
	"fmt"
	"sync"
)

// spool is an append-only byte stream with bounded retained history. Cursors
// are absolute byte offsets and therefore remain monotonic across truncation.
type spool struct {
	mu   sync.Mutex
	max  int
	base uint64
	data []byte
}

func newSpool(maxBytes int) *spool { return &spool{max: maxBytes} }

func (s *spool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if n >= s.max {
		s.base += uint64(len(s.data) + n - s.max) //nolint:gosec // non-negative bounded int lengths; conversion only widens.
		s.data = append(s.data[:0], p[n-s.max:]...)
		return n, nil
	}
	s.data = append(s.data, p...)
	if excess := len(s.data) - s.max; excess > 0 {
		copy(s.data, s.data[excess:])
		s.data = s.data[:s.max]
		s.base += uint64(excess)
	}
	return n, nil
}

func (s *spool) read(cursor uint64, maxBytes int) (Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.base + uint64(len(s.data))
	if cursor > next {
		return Chunk{}, fmt.Errorf("%w: cursor=%d next=%d", ErrInvalidCursor, cursor, next)
	}
	expired := cursor < s.base
	if expired {
		cursor = s.base
	}
	start := int(cursor - s.base) //nolint:gosec // cursor <= next proves delta <= len(data), which is an int.
	end := start + maxBytes
	if end > len(s.data) {
		end = len(s.data)
	}
	data := append([]byte(nil), s.data[start:end]...)
	return Chunk{Data: data, NextCursor: cursor + uint64(len(data)), CursorExpired: expired}, nil
}
