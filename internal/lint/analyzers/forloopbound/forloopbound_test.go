package forloopbound_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/localai-org/darkaifactory/internal/lint/analyzers/forloopbound"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), forloopbound.Analyzer, "a")
}
