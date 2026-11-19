package commandcontext_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/richiejp/vmjail/internal/lint/analyzers/commandcontext"
)

func TestAnalyzer(t *testing.T) {
	t.Parallel()
	analysistest.Run(t, analysistest.TestData(), commandcontext.Analyzer, "a")
}
