// Package commandcontext rejects os/exec.Command in production Go files.
// Subprocesses must inherit a bounded/cancellable context, unless an explicit
// reviewed suppression explains why the process is intentionally long-lived.
package commandcontext

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/darkaifactory/internal/lint/suppress"
)

const analyzerName = "commandcontext"

// Analyzer is the commandcontext analyzer entry point.
//
//nolint:gochecknoglobals // go/analysis API contract.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "os/exec.Command requires CommandContext or a reasoned suppression",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, file := range pass.Files {
		if strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isExecCommand(pass, call.Fun) || sup.Disabled(call.Pos(), analyzerName) {
				return true
			}
			pass.Reportf(call.Pos(),
				"os/exec.Command has no cancellation path; use CommandContext or //daft:lint:disable=commandcontext reason=\"…\" for an intentionally long-lived supervised process")
			return true
		})
	}
	//nolint:nilnil // standard x/tools analyzer pattern.
	return nil, nil
}

func isExecCommand(pass *analysis.Pass, expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Command" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	packageName, ok := pass.TypesInfo.ObjectOf(identifier).(*types.PkgName)
	return ok && packageName.Imported().Path() == "os/exec"
}
