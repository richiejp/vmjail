package nodoubleptr_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/localai-org/darkaifactory/internal/lint/analyzers/nodoubleptr"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nodoubleptr.Analyzer, "a")
}
