package cli

import (
	"io"
	"testing"
)

// BenchmarkExecuteVersion measures cold-start CLI dispatch: build the
// cobra command tree, parse argv, run the version handler. This is what
// every `daft` invocation pays at startup, so a regression here means
// the CLI feels sluggish.
func BenchmarkExecuteVersion(b *testing.B) {
	for b.Loop() {
		_ = Execute([]string{versionCmdName}, io.Discard, io.Discard)
	}
}
