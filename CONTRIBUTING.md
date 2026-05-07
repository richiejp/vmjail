# Contributing to darkaifactory

Thanks for working on this project. These rules apply to humans and AI agents
alike. They exist because they were learned the hard way — please respect
them, and if you have a good reason to break one, say so in the commit message.

## Workflow

### Commit early and often

Small, frequent commits are cheap to review, easy to revert, and force you to
keep work in shippable states. If you're hesitating because "it isn't done
yet" — commit it anyway. A WIP commit is better than a 600-line wall.

A useful guideline: if you can write a one-line description of what changed,
that's a commit. If you find yourself writing "and also..." three times,
you needed to commit earlier.

### Use the git hooks

Run `make hooks` once per clone. The pre-commit hook checks formatting,
linting, tests, and coverage regression. If a commit is blocked, fix the
underlying issue rather than bypassing the hook with `--no-verify`.

### Keep `PLAN.md` current

Project direction lives in [`PLAN.md`](PLAN.md). When scope changes, when a
milestone lands, when an open question gets answered — update `PLAN.md` in
the same change. Stale plans actively mislead future contributors (and agents).

## Code layout

### No Go files at the repository root

All Go code lives under `cmd/` (binary entry points) or `internal/`
(library code). Anything at the top level is for project metadata, build
config, or documentation only.

If you find yourself wanting to write `foo.go` at the root: it belongs in
`internal/foo` (or a sub-package thereof).

## Testing

### Test everything

> Any feature that is not tested will get overwritten or deleted by accident.

That isn't hyperbole — it's the lived experience that motivated this rule.
Untested code carries no record of its intended behavior, so future changes
have nothing to push back against. If a feature is worth adding, it's worth
a test that fails when the feature breaks.

This includes negative paths (does the error case actually return an error?)
and edge cases (empty input, zero, max int, unicode, etc).

### Coverage regression check

The pre-commit hook compares total Go test coverage against `coverage.txt`
and refuses commits that drop coverage by more than 1.0 percentage point. If
you have a legitimate reason to drop coverage (e.g. you removed a heavily-
tested package), update `coverage.txt` (`make cover`) in the same commit and
explain why in the commit message.

## Code style

### Document with comments the toolchain picks up

Write comments that `go doc` and `golangci-lint` recognise — i.e. doc
comments on exported identifiers in the `// Identifier ...` form, not random
prose elsewhere. Comments that aren't picked up as documentation tend to
rot; comments tied to a specific symbol get noticed when that symbol moves.

```go
// Foo does the foo. Brief, complete sentences, starting with the identifier.
func Foo() error { ... }
```

Avoid comments that just restate the code. If a comment would still be true
after a refactor that makes it obviously wrong, the comment isn't pulling
its weight.

### Use runtime assertions liberally

If a comment claims something to be true (e.g. "x is always non-nil here",
"the slice is sorted by this point"), assert it at runtime where reasonable.
Assertions are documentation that breaks loudly when violated, and they pay
for themselves the first time they catch a bug that a static type couldn't.

```go
// Caller guarantees nodes is non-empty and sorted ascending by ID.
func process(nodes []Node) {
    if len(nodes) == 0 {
        panic("process: nodes must be non-empty")
    }
    for i := 1; i < len(nodes); i++ {
        if nodes[i-1].ID > nodes[i].ID {
            panic("process: nodes must be sorted ascending by ID")
        }
    }
    // ... real work ...
}
```

For invariants in hot loops where panics are too expensive, hide them behind
a build tag or debug flag — but the default position is "assert it."

## Pull request expectations

- Describe the *why*, not just the *what* (the diff already shows the what).
- Update `PLAN.md` if the change affects scope or direction.
- Update `coverage.txt` only when intentional.
- Run `make` (the default target) locally before pushing.
