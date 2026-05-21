package maxbytes_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/localai-org/darkaifactory/internal/lint/analyzers/maxbytes"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), maxbytes.Analyzer, "a")
}
