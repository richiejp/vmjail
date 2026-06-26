package lintreasons_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/richiejp/darkaifactory/internal/lint/analyzers/lintreasons"
)

// TestAnalyzer drives the lintreasons analyzer over the testdata
// fixture under testdata/src/a. Diagnostic expectations are pinned
// via `// want \`…\“ markers in the fixture itself; see
// golang.org/x/tools/go/analysis/analysistest for the format.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), lintreasons.Analyzer, "a")
}
