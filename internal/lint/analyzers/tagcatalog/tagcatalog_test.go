package tagcatalog_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/richiejp/vmjail/internal/lint/analyzers/tagcatalog"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), tagcatalog.Analyzer, "a")
}
