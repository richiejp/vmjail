package nolinkname_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/richiejp/vmjail/internal/lint/analyzers/nolinkname"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nolinkname.Analyzer, "a")
}
