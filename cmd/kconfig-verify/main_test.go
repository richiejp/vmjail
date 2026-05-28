package main

import (
	"io"
	"os"
	"testing"
)

func TestMainWiresRunAndOsExit(t *testing.T) {
	oldExit, oldRun := osExit, kconfigRun
	defer func() { osExit, kconfigRun = oldExit, oldRun }()

	gotCode := -1
	var (
		gotArgs   []string
		gotStderr io.Writer
	)
	kconfigRun = func(args []string, stderr io.Writer) int {
		gotArgs = args
		gotStderr = stderr
		return 5
	}
	osExit = func(c int) { gotCode = c }

	main()

	if len(gotArgs) == 0 || gotArgs[0] != os.Args[0] {
		t.Errorf("args = %v, want os.Args", gotArgs)
	}
	if gotStderr != os.Stderr {
		t.Errorf("stderr = %v, want os.Stderr", gotStderr)
	}
	if gotCode != 5 {
		t.Errorf("osExit called with %d, want 5 (kconfigRun return)", gotCode)
	}
}
