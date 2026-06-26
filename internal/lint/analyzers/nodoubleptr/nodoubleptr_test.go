package nodoubleptr_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/richiejp/darkaifactory/internal/lint/analyzers/nodoubleptr"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nodoubleptr.Analyzer, "a")
}
