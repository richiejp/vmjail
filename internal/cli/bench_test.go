package cli

import (
	"io"
	"testing"
)

// BenchmarkExecuteVersion is the cold-start CLI dispatch cost every
// `daft` invocation pays — a regression here means the CLI feels
// sluggish to users.
func BenchmarkExecuteVersion(b *testing.B) {
	for b.Loop() {
		_ = Execute([]string{versionCmdName}, io.Discard, io.Discard)
	}
}
