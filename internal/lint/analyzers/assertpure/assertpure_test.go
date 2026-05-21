package assertpure_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/localai-org/darkaifactory/internal/lint/analyzers/assertpure"
)

// TestAnalyzer drives the assertpure analyzer over the fixture
// under testdata/src/a. The "assert" stub at testdata/src/assert
// stands in for the real internal/assert package — the analyzer
// identifies its target by declared package name.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), assertpure.Analyzer, "a")
}
