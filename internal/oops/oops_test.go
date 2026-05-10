package oops

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"sync"
	"testing"
)

// withTestSink redirects sink to buf and exitFn to a recorder for
// the duration of t. Returns the recorder so tests can inspect
// whether exit was called.
type exitRecorder struct {
	mu     sync.Mutex
	called bool
	code   int
}

func (e *exitRecorder) fn(code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.called = true
	e.code = code
}

func withTestSink(t *testing.T) (*bytes.Buffer, *exitRecorder) {
	t.Helper()
	buf := &bytes.Buffer{}
	rec := &exitRecorder{}
	prevSink := SetSink(buf)
	prevExit := SetExitFn(rec.fn)
	t.Cleanup(func() {
		SetSink(prevSink)
		SetExitFn(prevExit)
	})
	return buf, rec
}

func TestLogNilDoesNothing(t *testing.T) {
	buf, rec := withTestSink(t)
	Log("nothing-here", nil)
	if buf.Len() != 0 {
		t.Errorf("Log(nil) wrote: %q", buf.String())
	}
	if rec.called {
		t.Error("Log(nil) called exitFn")
	}
}

func TestLogWritesPrefixedLine(t *testing.T) {
	buf, rec := withTestSink(t)
	Log("kill virtiofsd", errors.New("synthetic"))
	if rec.called {
		t.Error("Log on healthy sink should not exit")
	}
	got := buf.String()
	if got != "oops: kill virtiofsd: synthetic\n" {
		t.Errorf("output = %q", got)
	}
}

// failingWriter implements io.Writer with a baked-in error.
type failingWriter struct{ err error }

func (f *failingWriter) Write(_ []byte) (int, error) { return 0, f.err }

func TestLogExitsWhenSinkFails(t *testing.T) {
	rec := &exitRecorder{}
	prevSink := SetSink(&failingWriter{err: io.ErrClosedPipe})
	prevExit := SetExitFn(rec.fn)
	t.Cleanup(func() {
		SetSink(prevSink)
		SetExitFn(prevExit)
	})
	Log("anything", errors.New("real"))
	if !rec.called {
		t.Fatal("expected exitFn called when sink writes fail")
	}
	if rec.code != FatalExitCode {
		t.Errorf("exit code = %d, want %d", rec.code, FatalExitCode)
	}
}

func TestSkipSilentlyContinuesOnExpectedError(t *testing.T) {
	buf, rec := withTestSink(t)
	Skip("rm bundle", fs.ErrNotExist, fs.ErrNotExist)
	if buf.Len() != 0 {
		t.Errorf("Skip on expected err wrote: %q", buf.String())
	}
	if rec.called {
		t.Error("Skip on expected err called exit")
	}
}

func TestSkipExitsOnUnexpectedError(t *testing.T) {
	buf, rec := withTestSink(t)
	Skip("rm bundle", fs.ErrPermission, fs.ErrNotExist)
	if rec.called != true {
		t.Fatal("expected exit on unexpected error")
	}
	if buf.Len() == 0 {
		t.Error("expected log output before exit")
	}
}

func TestSkipNilEntriesIgnored(t *testing.T) {
	_, rec := withTestSink(t)
	// nil expected entries should be skipped, not match anything.
	Skip("rm", fs.ErrNotExist, nil, fs.ErrNotExist)
	if rec.called {
		t.Error("nil expected entry caused exit")
	}
}

func TestMustExitsOnAnyError(t *testing.T) {
	_, rec := withTestSink(t)
	Must("hard-failure", errors.New("anything"))
	if !rec.called {
		t.Fatal("Must should always exit on non-nil err")
	}
	if rec.code != FatalExitCode {
		t.Errorf("exit code = %d, want %d", rec.code, FatalExitCode)
	}
}

func TestMustNilNoOp(t *testing.T) {
	_, rec := withTestSink(t)
	Must("no-error-here", nil)
	if rec.called {
		t.Error("Must(nil) should not exit")
	}
}
