// Package norecursion hosts the analyzer that flags self-recursive
// and mutually-recursive functions in our Go code (Power of 10
// rule 1, recursion clause).
//
// Holzmann's original rationale: an acyclic call graph is a hard
// prerequisite for proving termination and for bounding stack
// usage statically. Go's `go` runtime grows stacks on demand, so
// stack-overflow is less of a hard limit than in embedded C — but
// the *termination* argument transfers cleanly. A function that
// recurses on attacker-controlled input is a denial-of-service
// surface; one that recurses on developer-controlled input is
// often hiding a missing iterative form. The right answer is
// almost always an explicit stack or a `for`-loop.
//
// What this analyzer covers:
//
//   - Self-recursion: `func f() { f() }`.
//   - Mutual recursion: `func a() { b() }; func b() { a() }`.
//   - Methods are tracked the same way as plain functions.
//   - Cycles via any number of intermediate functions, as long as
//     all the participants live in the same package.
//
// What it deliberately doesn't cover:
//
//   - Indirect recursion through function values, closures, or
//     interface dispatch. Catching those needs CHA / RTA on a
//     whole-program SSA model and runs *much* slower; we accept
//     the false-negative risk in exchange for a single-package
//     analyzer that runs in milliseconds.
//   - Recursion that spans packages. Same reasoning.
//
// Suppression: `//daft:lint:disable=norecursion reason="…"`. The
// reason should explain why iteration isn't viable for this
// shape — usually because the data structure is recursive (AST
// nodes, file trees) and the depth is structurally bounded by
// the source-side input we control.
package norecursion

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/localai-org/darkaifactory/internal/lint/suppress"
)

// analyzerName drives Analyzer.Name and the //daft:lint:disable=
// match.
const analyzerName = "norecursion"

// Analyzer is the norecursion analyzer entry point.
//
//nolint:gochecknoglobals // go/analysis API contract.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "flag self-recursive and mutually-recursive functions (Power of 10 rule 1)",
	Run:  run,
}

// fnNode is the per-function bookkeeping used during cycle
// detection. callees is the set of same-package functions this
// function calls; decl is kept so diagnostics can report at the
// original declaration position.
type fnNode struct {
	decl    *ast.FuncDecl
	callees map[*types.Func]struct{}
}

const (
	colourWhite = 0 // unvisited
	colourGrey  = 1 // currently on the DFS path
	colourBlack = 2 // fully explored
)

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	nodes := collectFuncs(pass)
	wireCallees(pass, nodes)
	cyclic := findCyclic(nodes)
	for fn, n := range nodes {
		if !cyclic[fn] {
			continue
		}
		if sup.Disabled(n.decl.Pos(), analyzerName) {
			continue
		}
		pass.Reportf(n.decl.Pos(),
			"function %s participates in a call cycle (Power of 10 rule 1); restructure with explicit iteration or a stack, or annotate //daft:lint:disable=norecursion reason=\"…\"",
			fn.Name())
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}

// collectFuncs walks every file in the pass and registers each
// concrete function declaration. Functions with no body
// (interface methods, assembly stubs) are skipped — they have no
// outgoing edges by construction.
func collectFuncs(pass *analysis.Pass) map[*types.Func]*fnNode {
	nodes := map[*types.Func]*fnNode{}
	for _, f := range pass.Files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			obj, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func)
			if !ok {
				continue
			}
			nodes[obj] = &fnNode{decl: fd, callees: map[*types.Func]struct{}{}}
		}
	}
	return nodes
}

// wireCallees fills each fnNode.callees with the set of
// same-package functions it directly calls. Only resolvable
// *types.Func references via *ast.Ident or *ast.SelectorExpr are
// followed — function values, interface dispatch, and closures
// are deliberately not tracked (see the package doc-comment).
func wireCallees(pass *analysis.Pass, nodes map[*types.Func]*fnNode) {
	for _, n := range nodes {
		ast.Inspect(n.decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := resolveCallee(pass, call)
			if callee == nil {
				return true
			}
			if _, sameOwner := nodes[callee]; !sameOwner {
				return true
			}
			n.callees[callee] = struct{}{}
			return true
		})
	}
}

// resolveCallee returns the *types.Func behind a call expression
// when the call is to a named, statically-known function in the
// program. Returns nil for function values, closures, interface
// method calls without a resolvable concrete type, and any
// non-function callee.
func resolveCallee(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	var ident *ast.Ident
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		ident = fn
	case *ast.SelectorExpr:
		ident = fn.Sel
	default:
		return nil
	}
	obj, ok := pass.TypesInfo.Uses[ident].(*types.Func)
	if !ok {
		return nil
	}
	return obj
}

// findCyclic returns the set of functions that participate in any
// call cycle within nodes. Uses three-colour DFS; on a back-edge
// to a grey ancestor every function on the current DFS path
// from that ancestor down is marked cyclic.
//
//nolint:gochecknoglobals // closure state is per-call, not package state — this comment is paranoia.
func findCyclic(nodes map[*types.Func]*fnNode) map[*types.Func]bool {
	colour := map[*types.Func]int{}
	cyclic := map[*types.Func]bool{}
	var stack []*types.Func
	// stackPos lets us slice the on-DFS-path segment in O(1) when a
	// back-edge fires.
	stackPos := map[*types.Func]int{}

	var visit func(*types.Func)
	//daft:lint:disable=norecursion reason="DFS over the package's own call graph; depth bounded by the number of source-declared functions, which is finite per package"
	visit = func(fn *types.Func) {
		colour[fn] = colourGrey
		stackPos[fn] = len(stack)
		stack = append(stack, fn)
		for callee := range nodes[fn].callees {
			switch colour[callee] {
			case colourWhite:
				visit(callee)
			case colourGrey:
				// Back-edge: callee is somewhere earlier on the
				// DFS path. Every function from that position
				// onwards is in a cycle with callee.
				for _, member := range stack[stackPos[callee]:] {
					cyclic[member] = true
				}
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackPos, fn)
		colour[fn] = colourBlack
	}

	for fn := range nodes {
		if colour[fn] == colourWhite {
			visit(fn)
		}
	}
	return cyclic
}
