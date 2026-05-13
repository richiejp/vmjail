// Command daft-lint bundles the project's custom Go analyzers behind
// one invocation, served via golang.org/x/tools/go/analysis/multichecker.
// It runs alongside (after) golangci-lint as part of `make lint`.
//
// Adding a new analyzer:
//  1. Implement it under internal/lint/analyzers/<name>/.
//  2. Add its package to the imports below.
//  3. Add its Analyzer to the analyzers slice.
//  4. Document it in CONTRIBUTING.md "Power of 10 (Go)".
//
// New analyzers ship with analysistest-driven unit tests in their
// own package. This binary's own tests verify only that every
// shipped analyzer is bundled into the multichecker call.
package main

import (
	"os"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/richiejp/darkaifactory/internal/lint/analyzers/assertpure"
	"github.com/richiejp/darkaifactory/internal/lint/analyzers/chanconst"
	"github.com/richiejp/darkaifactory/internal/lint/analyzers/lintreasons"
	"github.com/richiejp/darkaifactory/internal/lint/analyzers/maxbytes"
	"github.com/richiejp/darkaifactory/internal/lint/analyzers/nodoubleptr"
	"github.com/richiejp/darkaifactory/internal/lint/analyzers/nogoto"
	"github.com/richiejp/darkaifactory/internal/lint/analyzers/nolinkname"
)

// analyzers is the bundled set. Package-scoped so tests can verify
// the bundle without invoking main() (which would call os.Exit via
// multichecker.Main).
var analyzers = []*analysis.Analyzer{
	assertpure.Analyzer,
	lintreasons.Analyzer,
	nogoto.Analyzer,
	nolinkname.Analyzer,
	nodoubleptr.Analyzer,
	chanconst.Analyzer,
	maxbytes.Analyzer,
}

// osExit and checkerMain are indirection points that let the
// in-package test drive main() without forking a subprocess or
// actually exiting. Same idiom as cmd/daft.
var (
	osExit      = os.Exit
	checkerMain = multichecker.Main
)

func main() {
	osExit(run())
}

// run dispatches the analyzers to multichecker. multichecker.Main
// itself calls os.Exit, so the return statement is unreachable in
// production; it exists so the function shape matches the project's
// testable-main convention for cover-check.
func run() int {
	checkerMain(analyzers...)
	return 0
}
