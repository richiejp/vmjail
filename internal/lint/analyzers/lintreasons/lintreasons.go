// Package lintreasons hosts the analyzer that enforces the project's
// suppression-comment convention: every comment that opens with
// `//daft:lint:disable=` (after optional whitespace) must include a
// non-empty `reason="…"` clause.
//
// Suppressions exist; the project's policy is that they must be
// reviewable artefacts rather than invisible escape hatches. A
// disable line without a reason is the same failure mode the
// project forbids for `_ = f()` and `//nolint` without a comment:
// the override is recorded, but the *why* is lost.
//
// The actual directive parser lives in internal/lint/suppress —
// lintreasons is just the half that reports malformed directives
// while suppress.Set is the half that honours well-formed ones.
//
// See CONTRIBUTING.md "Power of 10 (Go)" → "Escape-hatch convention"
// for the directive grammar and policy.
package lintreasons

import (
	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/darkaifactory/internal/lint/suppress"
)

// Analyzer is the lintreasons analyzer entry point. Run via
// golang.org/x/tools/go/analysis or the project's cmd/daft-lint
// multichecker.
var Analyzer = &analysis.Analyzer{
	Name: "lintreasons",
	Doc:  `require non-empty reason="…" on every //daft:lint:disable= directive`,
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				names, reason, ok := suppress.ParseDirective(c.Text)
				if !ok {
					continue
				}
				if len(names) == 0 {
					pass.Reportf(c.Pos(),
						`daft:lint:disable= directive must list at least one analyzer name`)
					continue
				}
				if reason == "" {
					pass.Reportf(c.Pos(),
						`daft:lint:disable= directive missing or empty reason="…"`)
				}
			}
		}
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}
