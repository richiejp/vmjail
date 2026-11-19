// Package observertail is the read-only consumer side of a
// per-sandbox observer log: it streams the newline-delimited JSON
// Tier-1 trace (see internal/observability) to a writer, optionally
// following appended lines like `tail -f`. It never modifies the
// file. The sandbox host's ObserverStream handler uses it to relay a
// running sandbox's log to a `daft sandbox observer-tail` client.
package observertail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// DefaultPollInterval is how often the follow loop re-checks a
// quiescent log for new bytes or rotation. The observer samples at
// ~1 Hz, so a few hundred ms keeps the tail responsive without
// busy-spinning.
const DefaultPollInterval = 200 * time.Millisecond

// readBufSize is the per-Stream read buffer: big enough to bypass bufio's
// small-buffer path, small enough not to over-read a stalled log.
const readBufSize = 32 * 1024

// Stream copies the observer log at path to w. With follow=false it
// sends the current contents once and returns at EOF. With
// follow=true it keeps forwarding appended bytes — transparently
// reopening across the observer's rename-based rotation (see
// observability.Observer.rotateLocked) — until ctx is cancelled.
//
// It never writes to the file. ctx cancellation is the normal stop
// and returns nil; a non-nil error means an open/read/write failure.
func Stream(ctx context.Context, path string, follow bool, w io.Writer) error {
	return stream(ctx, path, follow, w, DefaultPollInterval)
}

func stream(ctx context.Context, path string, follow bool, w io.Writer, poll time.Duration) error {
	f, err := os.Open(path) //nolint:gosec // G304: caller-supplied per-sandbox log path, opened read-only.
	if err != nil {
		return fmt.Errorf("observertail: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("observertail: stat %s: %w", path, err)
	}

	buf := make([]byte, readBufSize)
	//daft:lint:disable=forloopbound reason="returns on ctx cancel, on EOF when !follow, or on a read/write error; an unbounded follow tail is the intended behaviour (tail -f)"
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return fmt.Errorf("observertail: write: %w", werr)
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("observertail: read %s: %w", path, readErr)
		}
		// Drained to EOF.
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
		nf, ninfo, rotated, rerr := reopenIfRotated(path, f, info)
		if rerr != nil {
			// The path is briefly absent mid-rotation (rename then
			// recreate); treat any stat/open hiccup as "keep waiting"
			// and retry on the next tick.
			continue
		}
		if rotated {
			// Flush any residual already-written bytes from the old
			// inode before switching — the renamed backup never grows
			// again, so this terminates.
			if _, werr := io.Copy(w, f); werr != nil {
				_ = nf.Close()
				return fmt.Errorf("observertail: flush before rotate: %w", werr)
			}
			_ = f.Close()
			f, info = nf, ninfo
		}
	}
}

// reopenIfRotated decides whether the log at path is still the file
// we have open. It is "rotated" when path now resolves to a different
// inode (the observer renamed our file to .1 and recreated path) or
// when the file shrank below our read offset (an in-place truncation).
// On rotation it returns a freshly-opened handle + its FileInfo.
func reopenIfRotated(path string, f *os.File, openInfo os.FileInfo) (newFile *os.File, newInfo os.FileInfo, rotated bool, err error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		return nil, nil, false, err
	}
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, false, err
	}
	if os.SameFile(openInfo, pathInfo) && pathInfo.Size() >= off {
		return nil, nil, false, nil
	}
	nf, err := os.Open(path) //nolint:gosec // G304: same caller-supplied path, read-only.
	if err != nil {
		return nil, nil, false, err
	}
	ninfo, err := nf.Stat()
	if err != nil {
		_ = nf.Close()
		return nil, nil, false, err
	}
	return nf, ninfo, true, nil
}
