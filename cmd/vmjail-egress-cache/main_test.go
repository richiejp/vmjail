package main

import (
	"errors"
	"os"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
)

// TestRun covers both arms of run() via the dispatch seam: a nil error
// exits 0, a non-nil error exits 1. The real dispatch is exercised in
// internal/egresscache; here we only prove the exit-code mapping.
func TestRun(t *testing.T) {
	orig := dispatch
	t.Cleanup(func() { dispatch = orig })

	dispatch = func([]string, func(string) string, ...crane.Option) error { return nil }
	if got := run([]string{"pull-unpack", "ref", "dir"}); got != 0 {
		t.Errorf("run(success) = %d, want 0", got)
	}

	dispatch = func([]string, func(string) string, ...crane.Option) error {
		return errors.New("boom")
	}
	if got := run([]string{"bogus"}); got != 1 {
		t.Errorf("run(error) = %d, want 1", got)
	}
}

// TestMainSeam drives main() with both seams swapped so it neither forks
// nor exits — covering main()'s single statement.
func TestMainSeam(t *testing.T) {
	origExit, origDispatch, origArgs := osExit, dispatch, os.Args
	t.Cleanup(func() { osExit, dispatch, os.Args = origExit, origDispatch, origArgs })

	var code int
	osExit = func(c int) { code = c }
	dispatch = func([]string, func(string) string, ...crane.Option) error { return nil }
	os.Args = []string{"vmjail-egress-cache", "pull-unpack", "ref", "dir"}
	main()
	if code != 0 {
		t.Errorf("main exit code = %d, want 0", code)
	}
}
