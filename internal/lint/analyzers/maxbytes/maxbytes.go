// Package maxbytes hosts the analyzer that requires every
// `io.ReadAll(req.Body)` call (where `req` is `*http.Request`) to
// be preceded by a `http.MaxBytesReader` wrap somewhere in the
// same function (Power of 10 rule 3, Go form).
//
// An unbounded read of an attacker-supplied body is the canonical
// DoS shape in HTTP handlers: a malicious client streams gigabytes
// of garbage, the server allocates indefinitely, the host OOMs.
// Wrapping the body in MaxBytesReader caps the read at a known
// ceiling and surfaces a clean error past it.
//
// The check is function-scoped, not flow-precise: any
// MaxBytesReader call inside the enclosing function silences the
// diagnostic. The simplification is intentional — flow-precise
// analysis would add a lot of code for marginal additional value,
// and a wrap on the wrong body inside the same function is rare
// enough that review catches it.
//
// Response bodies (`http.Response.Body`) are NOT checked — they
// come from a server we already trust enough to call. Only request
// bodies (`*http.Request.Body`) are gated.
//
// Suppression: `//vmjail:lint:disable=maxbytes reason="…"`. The
// reason should explain why bounded reading is genuinely not
// needed at this call site.
package maxbytes

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/vmjail/internal/lint/suppress"
)

// analyzerName drives Analyzer.Name and the //vmjail:lint:disable=
// match.
const analyzerName = "maxbytes"

// Analyzer is the maxbytes analyzer entry point.
//
//nolint:gochecknoglobals // go/analysis API contract.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "io.ReadAll on http.Request.Body must be preceded by http.MaxBytesReader (Power of 10 rule 3, Go form)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checkFunc(pass, sup, fn)
		}
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}

// checkFunc walks one function body. It collects every unsafe
// `io.ReadAll(*http.Request.Body)` call site and notes whether any
// MaxBytesReader call appears in the same function. If unsafe
// reads exist without a wrap, each unsafe site is reported.
func checkFunc(pass *analysis.Pass, sup *suppress.Set, fn *ast.FuncDecl) {
	var unsafeReads []token.Pos
	var hasMaxBytes bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case isHTTPMaxBytesReader(pass, call):
			hasMaxBytes = true
		case isIoReadAllOfRequestBody(pass, call):
			unsafeReads = append(unsafeReads, call.Pos())
		}
		return true
	})
	if hasMaxBytes {
		return
	}
	for _, p := range unsafeReads {
		if sup.Disabled(p, analyzerName) {
			continue
		}
		pass.Reportf(p,
			"io.ReadAll on http.Request.Body must be preceded by http.MaxBytesReader (Power of 10 rule 3, Go form)")
	}
}

// isHTTPMaxBytesReader reports whether call resolves to
// net/http.MaxBytesReader. Type-info-driven so a function with the
// same name in a different package doesn't trigger a false positive
// silencing of unrelated ReadAlls.
func isHTTPMaxBytesReader(pass *analysis.Pass, call *ast.CallExpr) bool {
	fn := calleeFunc(pass, call)
	if fn == nil {
		return false
	}
	return fn.Pkg() != nil && fn.Pkg().Path() == "net/http" && fn.Name() == "MaxBytesReader"
}

// isIoReadAllOfRequestBody reports whether call is io.ReadAll (or
// the deprecated io/ioutil.ReadAll) applied to something that
// resolves to *http.Request.Body. The argument-count guard is
// elided here because go/types rejects ReadAll() with zero args
// well before the analyzer runs — if the call type-checked, it has
// at least one argument.
func isIoReadAllOfRequestBody(pass *analysis.Pass, call *ast.CallExpr) bool {
	fn := calleeFunc(pass, call)
	if fn == nil || fn.Name() != "ReadAll" || fn.Pkg() == nil {
		return false
	}
	switch fn.Pkg().Path() {
	case "io", "io/ioutil":
	default:
		return false
	}
	return argIsRequestBody(pass, call.Args[0])
}

// calleeFunc returns the *types.Func behind a call expression, or
// nil for any non-resolvable, non-function callee.
func calleeFunc(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	obj, ok := pass.TypesInfo.Uses[sel.Sel]
	if !ok {
		return nil
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	return fn
}

// argIsRequestBody reports whether arg is a `*ast.SelectorExpr`
// whose Sel is named "Body" and whose receiver type resolves to
// (a pointer to) net/http.Request. The `t == nil` and `obj.Pkg()
// == nil` guards are elided here: pass.TypesInfo.TypeOf on a
// type-checked program never returns nil for a typed expression,
// and (*types.Named).Obj().Pkg() is non-nil for any non-universe
// type — Request is in net/http, never the universe scope.
func argIsRequestBody(pass *analysis.Pass, arg ast.Expr) bool {
	sel, ok := arg.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Body" {
		return false
	}
	t := pass.TypesInfo.TypeOf(sel.X)
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Name() == "Request" && obj.Pkg().Path() == "net/http"
}
