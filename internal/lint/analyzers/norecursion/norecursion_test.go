package norecursion_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/localai-org/darkaifactory/internal/lint/analyzers/norecursion"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), norecursion.Analyzer, "a")
}
