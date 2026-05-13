// Package suppress is the shared library used by every custom
// analyzer to honour `//daft:lint:disable=<names> reason="…"`
// directives.
//
// Two surfaces:
//
//   - ParseDirective inspects one comment's raw text. Pure function,
//     trivially unit-testable. Used by lintreasons to flag malformed
//     directives, and by Set to index well-formed ones.
//   - Set indexes every well-formed directive in an analysis.Pass by
//     source line. Disabled(pos, name) reports whether the analyzer
//     `name` is suppressed at `pos` — same-line OR preceding-line, so
//     callers can put the directive above the problematic statement
//     (mirroring //nolint: conventions).
//
// A directive with a missing or empty reason does NOT suppress
// anything. lintreasons reports it separately. The project policy is
// that every override is reviewable; a directive without a justified
// reason is itself a bug.
//
// See CONTRIBUTING.md "Power of 10 (Go)" → "Escape-hatch convention".
package suppress

import (
	"go/token"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// directivePrefix is the prefix that, anchored at the start of a
// comment's body, marks the comment as a suppression directive.
const directivePrefix = "daft:lint:disable="

// reasonRE captures the contents of reason="…". The contents may
// be empty per regex; the caller distinguishes empty-reason from
// non-empty-reason.
var reasonRE = regexp.MustCompile(`reason="([^"]*)"`)

// ParseDirective parses raw — the literal comment text including
// its leading `//` or `/*` — as a daft:lint:disable= directive.
//
// Outcomes:
//
//   - ok=false: the comment is not a directive (no anchored prefix).
//     `names` and `reason` are zero.
//   - ok=true, reason != "": well-formed directive. `names` lists
//     the analyzers being suppressed; `reason` is the trimmed
//     reason text.
//   - ok=true, reason == "": directive is present but malformed
//     (missing reason="…" clause, or reason was empty / whitespace).
//     `names` is parsed best-effort so callers can include the
//     analyzer set in their diagnostics.
func ParseDirective(raw string) (names []string, reason string, ok bool) {
	text := strings.TrimPrefix(raw, "//")
	text = strings.TrimPrefix(text, "/*")
	text = strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(text, directivePrefix) {
		return nil, "", false
	}
	body := text[len(directivePrefix):]
	namesStr, rest := splitAtFirstSpace(body)
	if namesStr != "" {
		names = strings.Split(namesStr, ",")
	}
	if m := reasonRE.FindStringSubmatch(rest); m != nil {
		if r := strings.TrimSpace(m[1]); r != "" {
			return names, r, true
		}
	}
	return names, "", true
}

// splitAtFirstSpace splits s at the first space or tab, returning
// (head, tail) where tail does not include the separator. If no
// separator is present, head=s and tail="".
func splitAtFirstSpace(s string) (head, tail string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// lineKey identifies a single line in a single source file.
type lineKey struct {
	file string
	line int
}

// Set indexes every well-formed suppression directive in an
// analysis.Pass by (file, line). It is built once per analyzer
// invocation via New and queried via Disabled.
type Set struct {
	fset   *token.FileSet
	byLine map[lineKey]map[string]struct{}
}

// New scans pass.Files for well-formed daft:lint:disable=
// directives and returns a Set ready for Disabled queries.
// Malformed directives (missing or empty reason) are ignored here
// — they do not suppress diagnostics. lintreasons reports them
// separately.
func New(pass *analysis.Pass) *Set {
	s := &Set{
		fset:   pass.Fset,
		byLine: make(map[lineKey]map[string]struct{}),
	}
	for _, f := range pass.Files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				names, reason, ok := ParseDirective(c.Text)
				if !ok || reason == "" {
					continue
				}
				pos := pass.Fset.Position(c.Pos())
				s.add(pos.Filename, pos.Line, names)
			}
		}
	}
	return s
}

func (s *Set) add(file string, line int, names []string) {
	key := lineKey{file: file, line: line}
	bucket, ok := s.byLine[key]
	if !ok {
		bucket = make(map[string]struct{}, len(names))
		s.byLine[key] = bucket
	}
	for _, n := range names {
		bucket[n] = struct{}{}
	}
}

// Disabled reports whether the analyzer `name` is suppressed at
// `pos`. Both the same line and the line immediately above are
// consulted, so a directive may appear either trailing the line it
// suppresses or on its own line directly above. Lines further away
// do not suppress — locality keeps directives reviewable.
func (s *Set) Disabled(pos token.Pos, name string) bool {
	p := s.fset.Position(pos)
	if s.has(p.Filename, p.Line, name) {
		return true
	}
	return s.has(p.Filename, p.Line-1, name)
}

func (s *Set) has(file string, line int, name string) bool {
	m, ok := s.byLine[lineKey{file: file, line: line}]
	if !ok {
		return false
	}
	_, ok = m[name]
	return ok
}
