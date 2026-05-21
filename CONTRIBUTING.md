# Contributing to darkaifactory

Thanks for working on this project. These rules apply to humans and AI agents
alike. They exist because they were learned the hard way — please respect
them, and if you have a good reason to break one, say so in the commit message.

## Project ethos

This project is **not** managed like a typical software project. The
defaults are different. Read this before opening a PR.

### Correctness, security, and performance over shipping speed

The thing being built is a sandbox for fully-automated AI coding
agents. The agents will be untrusted. Bugs in the harness become CVEs;
performance regressions become wall-clock penalties on every job;
incorrect feature semantics become silent data leaks. None of these are
acceptable to "iterate on later."

The trade-off goes one way: when in doubt, slow down and get it right.
A delayed feature is recoverable; a shipped feature with the wrong
guarantees is not.

### Fail fast, fail loud

Errors get handled, not swallowed. Invariants get asserted, not
hoped for. Tests fail with a clear `t.Fatalf`, not a quiet `t.Skip`.
A broken environment fails the build, not the next user. Anything
that defers a problem to "later" is a future bug report waiting to
happen.

### Reduce scope, never correctness

If a feature is too big, cut it in half and ship the half that's
correct. Do not ship a feature with weakened guarantees, optional
validation, or a "we'll harden it later" note. The half-feature
will be the load-bearing one in production by the time anyone gets
back to it. Either the guarantee is real or the feature isn't
shipped.

### Verification monotonically increases

Coverage goes up, never down. Lint rules get tighter, never looser.
Gates added, not removed. If a gate is wrong for your change, fix
the gate and explain why in the same commit — but the resulting
gate must still be at least as strict overall as what it replaced.
The codebase's verification surface is a ratchet: every commit
either holds it or tightens it.

This applies to the `coverage.txt` baseline (only rises), the
`bench.txt` baseline (per-bench numbers can move; the gate's
fail/warn thresholds tighten), the per-file 80% floor (only rises),
and the lint config in `.golangci.yml` (linters only get added).
`make cover` and `make bench` refuse to write a worse baseline.

**There is no exemption for deletions.** Removing heavily-tested
code mechanically lowers the coverage percentage; that is not a
reason to lower `coverage.txt`. The required move is to add
coverage elsewhere in the same change so the new baseline is at
or above the old one. If you cannot find any untested code worth
testing, the codebase doesn't need this deletion. Do not edit
`coverage.txt` by hand to a smaller number — `cover-check`
refuses, and the policy is the same whether the tooling is
strict enough to enforce it or not.

### Go doesn't enforce this for you

The Go language is permissive about silently-discarded errors,
zero-value-as-default for missing fields, nil-receiver method calls,
and integer overflow. This project compensates by:

- Treating every error return as load-bearing. `_ = f()` for an
  error-returning call is a code smell, allowed only with a
  one-line comment explaining why the error is genuinely
  uninteresting (e.g. `defer f.Close()` after a successful read).
- Asserting invariants at the package boundary and in public
  constructors. A struct with required fields validates them on
  construction, not on first use.
- Running `golangci-lint` with `errcheck`, `gosec`, `nilerr`,
  `bodyclose`, and friends enabled — see `.golangci.yml`. The
  static-analysis surface is treated the same as the test
  surface: monotonically tightening.

### How to disagree with the rules

You can. State your reasoning in the commit message, in the PR, or
in `PLAN.md`'s open questions section. Edit the rules in the same
commit that changes the behaviour. What you cannot do is bypass the
rules quietly — `--no-verify`, `//nolint` without a justifying
comment, etc. The coverage baseline is non-negotiable: it only ever
rises (see [Verification monotonically increases](#verification-monotonically-increases)).

## Workflow

### Commit early and often

Small, frequent commits are cheap to review, easy to revert, and force you to
keep work in shippable states. If you're hesitating because "it isn't done
yet" — commit it anyway. A WIP commit is better than a 600-line wall.

A useful guideline: if you can write a one-line description of what changed,
that's a commit. If you find yourself writing "and also..." three times,
you needed to commit earlier.

### Use the git hooks

Run `make hooks` once per clone. The pre-commit hook delegates to
`make verify`, which runs every gate this project has:

- `make check` — fmt + lint + unit tests + coverage gate. The unit
  test step also replays every committed fuzz reproducer
  (`testdata/fuzz/FuzzXxx/<id>` corpora) automatically — that's
  Go's built-in behaviour for `go test`.
- `make integration` — e2e tests that boot real VMs against
  `/dev/kvm` (see "Test categories" for the unit / integration /
  e2e distinction).
- `make bench-check` — benchmark regression gate (>2× fails,
  >1.1× warns) against `bench.txt`.
- `FUZZ_TIME=1s scripts/fuzz.sh` — a tight active-fuzz sweep
  across every `FuzzXxx` target. Enough to catch deterministic
  crashes from the seed corpus; manual / nightly fuzzing uses
  `make fuzz` (default 30s/target).

**`--no-verify` is never OK.** If a gate is wrong for your change,
fix the gate and explain why in the same commit. Bypassing it
re-creates the failure modes the gate was added to prevent.

### Keep `PLAN.md` current

Project direction lives in [`PLAN.md`](PLAN.md). When scope changes, when a
milestone lands, when an open question gets answered — update `PLAN.md` in
the same change. Stale plans actively mislead future contributors (and agents).

## Cloning

Most of the time you only want the Go code; the third-party submodules
under `third_party/` (Linux kernel, Cloud Hypervisor, u-root) only matter
when you're building the microVM stack.

```sh
# Just the Go code (default):
git clone <url>

# Plus the submodules (needed for Nix builds of the kernel / VMM / userland):
git clone --recurse-submodules <url>
# or after the fact:
git submodule update --init --recursive
```

The Linux kernel submodule pulls a full stable history (~1.7 GB) by
default. Pass `--shallow-submodules` to your clone if disk space is at a
premium — the recorded ref is always a published tag, so the depth-1
clone will reach it.

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

### Test categories

This project uses three categories. The lines between them matter
because they're load-bearing in how much you can claim a test
actually proves.

- **Unit** — single function or type, every external dependency
  mocked at the package boundary. Lives in `*_test.go` alongside
  the code. No build tag. Should run in milliseconds.

- **Integration** — multiple components exercised together
  in-process, with at least one external boundary stubbed
  (in-process HTTP server instead of a real network peer, fake
  VMM runner instead of cloud-hypervisor, simulated agent writes
  instead of an in-VM container). Also lives in `*_test.go`. May
  be slower (RPC round-trips, file I/O, certificate generation).
  Names end `…Integration` so the category is obvious from `go
  test -run`.

- **End-to-end (e2e)** — the full system runs, period. For this
  project that means: a real Cloud Hypervisor boots a real
  kernel + initramfs against `/dev/kvm`, virtiofsd is a real
  subprocess, the in-VM agent is the real binary, and any
  network communication crosses a real socket. Lives behind the
  `linux,integration` build tag (run via `make integration`).

The rule that holds these apart: **if any major component is
stubbed, faked, or simulated, it is not e2e.** A test that
fakes the VM and simulates an in-VM write to the shared dir is
integration, even if it covers every other leg of the workflow.
Be honest in test names and PR descriptions — calling integration
work "e2e" hides the gap that a real-VM check would have caught.

### E2E tests on every commit

The pre-commit hook runs the e2e tests in
`internal/sandbox/integration_test.go` (build tag:
`linux,integration`) on every commit, via `make integration`. The
target name is historical; the tests it runs are e2e by the
definition above. They:

- nix-build the artefacts (`cloud-hypervisor`, `kernel`, `virtiofsd`,
  `initramfs`, `hello-bundle`),
- spawn real VMs via Cloud Hypervisor against `/dev/kvm`,
- and assert the full pull → bundle → boot → run pipeline.

**Linux + KVM is required, not optional.** Every developer who
can meaningfully contribute to this codebase has them — the whole
project is a Linux sandbox harness, so there is no useful work
without them. The pre-commit hook does not gracefully degrade
when KVM is missing; it fails. CI without KVM does not gate
commits. There are no skips: missing artefact → `t.Fatalf`,
missing `/dev/kvm` → `t.Fatalf`, broken Nix → the make target
fails. And as above, `--no-verify` is never OK — if the gate is
wrong for your change, fix the gate.

Artefact resolution is automatic: env vars (`DAFT_*`) override,
otherwise the resolver looks for the `result-*` symlinks that
`make integration-build` creates at the repo root. Silently
skipping e2e tests is exactly the kind of "green CI, broken
merge" failure mode this gate exists to prevent.

### Coverage regression check (monotonic)

The pre-commit hook (via `make cover-check` and
`scripts/cover-check.sh`) enforces three gates. All three must pass:

1. **Total coverage ≥ committed `coverage.txt` baseline.** No
   tolerance for drops. The previous "within 1.0pp" allowance was
   too forgiving — measurement noise is real, but at this codebase
   size each percentage point is dozens of statements, and "I'll
   add the test in the next commit" never lands.
2. **Per-file coverage ≥ 80%** for every non-test source file
   with more than 10 statements. Catches the case where one file
   is dark while the rest of the codebase masks it.
3. **The baseline only ever rises.** `make cover` refuses to
   write a value lower than the current `coverage.txt`. Verification
   surface is monotonic: there is no path through the tooling that
   silently weakens it.

If `cover-check` fails, the answer is **never** to lower the
baseline or weaken the per-file threshold. Real options when
coverage is hard:

- **Mock harder.** Inject the I/O surface so the function under
  test isn't asking for the network / filesystem / a privileged
  syscall. The pattern in `internal/sandbox` (the `runner` and
  `shareSidecar` interfaces) and `internal/imagestore` (the
  `puller` interface, plus go-containerregistry's
  `pkg/registry` as an in-process httptest server) covers most of
  the cases we've hit so far.
- **Provide real fixtures.** When mocking would amount to
  re-implementing the dependency, build the real thing as a test
  fixture: a dummy OCI image pushed into a localhost registry, a
  fake-binary self-exec via `TestMain`, a synthetic tar stream
  fed straight into the unpacker.
- **Remove dead branches.** Coverage is a forcing function: if a
  branch can't be exercised by any test, it probably can't be
  reached in production either.

**There is no escape hatch.** `coverage.txt` only ever rises.
Even if you delete heavily-tested code — the case where the
percentage mechanically drops because total statements drops —
the answer is to add coverage elsewhere in the same change so
the new baseline is at or above the old one. Do not edit
`coverage.txt` by hand to lower it; do not "just this once"
relax the gate. Verification monotonically increases. If your
deletion is correct, the rest of the codebase will still
contain enough untested branches to write tests against;
write them.

Coverage is the price of merge — pay it on the way in, not by
relaxing the gate.

### Fuzz where you can

> If something can be fuzzed, it should be fuzzed.

Any function that ingests input from outside the program — CLI arguments,
environment variables, files, network bytes, parsed configs, anything
bytes-shaped from an untrusted source — gets a `FuzzXxx` target alongside
its unit tests. Fuzzers find edge cases that hand-written tables miss, and
they cost almost nothing once the harness is wired up.

Mechanics:

- Use Go's built-in fuzzer (`go test -fuzz`). No third-party dependency.
- Seed the corpus generously with `f.Add(...)` — every interesting case you
  can think of by hand. The fuzzer mutates outward from there.
- Fuzz targets only accept primitive types. For structured input, take a
  single `string` or `[]byte` and parse it into the shape you want inside
  the fuzz body. See `FuzzExecute` in `internal/cli/cli_test.go` for the
  pattern (NUL-separated argv).
- Assert only invariants the fuzzer can verify mechanically: "doesn't panic",
  "round-trips", "output satisfies property X". Don't write fuzz tests that
  assert specific outputs — they'll fail constantly on every new input.
- Make the test hermetic: clear or fix any environment variables, globals,
  or filesystem state the function reads. Otherwise host noise turns the
  fuzzer non-deterministic.

Running fuzzers:

```sh
# Run only the seed corpus (acts as regular tests; this is what `make test` does):
go test ./internal/cli

# Actively fuzz for 30 seconds:
go test -run='^$' -fuzz=FuzzExecute -fuzztime=30s ./internal/cli

# Or by iteration count:
go test -run='^$' -fuzz=FuzzExecute -fuzztime=100000x ./internal/cli
```

Without `-fuzztime`, fuzzing runs until you Ctrl-C — useful when hardening
a feature, but pick a budget for routine local runs.

When the fuzzer finds a crash, Go writes the failing input to
`testdata/fuzz/<FuzzName>/<hash>` in the package directory. **Commit those
files.** From then on, every `go test` (with no flags) replays them
deterministically as regression cases — a one-time discovery becomes a
permanent test forever.

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
"the slice is sorted by this point"), assert it at runtime. Assertions are
documentation that breaks loudly when violated, and they pay for themselves
the first time they catch a bug that a static type couldn't.

Use `internal/assert` rather than bare `panic` — assertions go through the
project's vocabulary so they're greppable, formatted consistently, and
covered by the `assertpure` analyzer (see "Power of 10 (Go)" below).

```go
import "github.com/localai-org/darkaifactory/internal/assert"

// Caller guarantees nodes is non-empty and sorted ascending by ID.
func process(nodes []Node) {
    assert.True(len(nodes) > 0, "process: nodes must be non-empty")
    for i := 1; i < len(nodes); i++ {
        assert.True(nodes[i-1].ID <= nodes[i].ID,
            "process: nodes must be sorted ascending by ID")
    }
    // ... real work ...
}
```

The argument to `assert.True` / `assert.NoErr` must be side-effect free —
no function calls — so assertions could in principle be compiled out
without changing observed behaviour. Compute the value first, assert on the
named local:

```go
// Bad — analyzer flags this:
assert.True(node.Children() > 0, "...")

// Good:
n := node.Children()
assert.True(n > 0, "...")
```

For invariants in hot loops where the assert cost is itself measurable,
keep the check but factor the body so the assert is the cheap part of the
iteration. We don't have a debug-build escape hatch — the default position
is "assert it always."

#### Assert caller contracts at function entry

Many bugs in this codebase have shapes like:

- Caller passes a non-cancellable `context.Context`; the callee's
  `for { select … }` blocks forever and the bug surfaces minutes
  later as a hang in a downstream service.
- Caller passes `nil` where the type system says nothing about it,
  callee dereferences deep in a stack, panic stack-trace points
  at the wrong line.
- Caller passes a "sorted" slice that isn't, callee binary-searches
  on garbage, returns silently-wrong results.

Each of these *could* be caught by a reviewer noticing the caller.
Reviewers miss things. A one-line assertion at function entry turns
the bug from a runtime hang (or silent data corruption) into a
loud panic at the offending call site.

The canonical pattern:

```go
import "github.com/localai-org/darkaifactory/internal/assert"

func (r *Reaper) Run(ctx context.Context) {
    assert.True(ctx.Done() != nil,
        "reaper: Run requires a cancellable context")
    // ... loop body that depends on <-ctx.Done() ...
}
```

The `ctx.Done() != nil` check is worth knowing about specifically:
`context.Background()` and `context.TODO()` return a nil channel
from `Done()`, and any `WithValue` chain rooted at them does the
same. Calling `<-ctx.Done()` on a nil channel blocks forever —
exactly the bug `for { select … }` is supposed to *avoid* by
having an exit case. The runtime check pushes the failure from "we
hang at 03:00 AM" to "the call site panics immediately."

When to write a contract assertion:

- Every function whose loop or blocking I/O depends on a ctx →
  assert `ctx.Done() != nil`.
- Every function that dereferences a parameter without further
  nil-checking → assert non-nil.
- Every function that takes a "sorted" / "non-empty" / "deduped"
  slice → assert the invariant.
- Every function that takes a duration or time assumed positive
  / future / non-zero → assert it.

The assertion arguments must be side-effect free — `internal/lint/
analyzers/assertpure` enforces that. Compute any value you need
into a local first, then assert on the local.

This dovetails with the Power-of-10 (Go) rules below: many of the
analyzers enforce *structural* properties (no goto, no recursion,
bounded loops). Runtime contract assertions cover the cases the
type system and the structural analyzers can't see — caller
correctness across the function boundary. The two layers are
complementary; both are mandatory where they apply.

### Handle every error

Every error return is load-bearing. The default position is "the
caller cares about it" — return it up the stack with `%w` wrapping
or handle it locally with a recovery action.

The only sanctioned way to discard an error is the
`_, _ = f()` / `_ = f.Close()` pattern, and only when:

- the failure is genuinely uninteresting (e.g. closing a file
  after a successful read; you can't recover from a Close error
  and the data is already on disk), AND
- a comment on the same or preceding line explains why.

`golangci-lint` runs `errcheck` over our code, so unjustified
discards fail the lint gate. Do not work around `errcheck` with
silent `_, _ =` if the error genuinely matters — return it,
log it, panic on it, but don't pretend it doesn't exist.

For functions that return `(value, error)`, prefer the early
return:

```go
v, err := f()
if err != nil {
    return fmt.Errorf("f: %w", err)
}
// ... use v
```

Avoid the trap of "this can't fail in practice" — if Go's type
system thinks it can fail, treat it as if it can. The one time
it does in production, the unhandled path is the one that bites.

### Static analysis is not optional

`.golangci.yml` enables a deliberately strict set of linters
beyond the Go default. Read it before opening a PR; the gate is
not negotiable.

Adding a `//nolint:<linter>` requires a comment on the same line
or above explaining why the linter is wrong for this specific
case. Reviewers will push back on `//nolint` without
justification.

The lint surface itself is monotonic: linters get added, never
removed. If a linter produces too many false positives to be
useful, configure it tighter or scope it to specific paths in
`.golangci.yml` — but don't disable it wholesale.

### Power of 10 (Go)

We adopt a Go-shaped subset of Holzmann's "Power of Ten: Rules for
Developing Safety-Critical Code" (JPL, 2006). The rationale is the
same as theirs: when a sandbox is the security boundary between
untrusted AI agents and the user's data, "we'll harden it later" is
not survivable. The full original ten rules target C; this section
documents the subset we adopt, the static check that enforces each,
and the rules we deliberately dropped because we could not
mechanically enforce them in Go.

**Every adopted rule has a static check.** If a proposed rule has
no analyzer, it doesn't ship — review-only "rules" do not survive
contact with deadlines. The custom analyzers live under
`internal/lint/analyzers/`, are bundled into `cmd/daft-lint`, and
are run by `make lint` after `golangci-lint`.

#### Adopted rules

| # | Rule (Go form) | Enforcement |
|---|---|---|
| 1a | No `goto` | `nogoto` analyzer |
| 1b | No `panic(` outside allowlisted packages (`internal/assert`, `internal/oops`) | `forbidigo` |
| 1c | No `recover()` outside allowlisted call sites | `forbidigo` + allowlist |
| 1d | No `unsafe.*` | `forbidigo` + `gosec` G103 |
| 1e | No `//go:linkname` | `nolinkname` analyzer |
| 1f | No recursion in our packages | `norecursion` analyzer (call-graph SCCs) |
| 2 | `for {}` loops require a `//daft:unbounded reason="…"` annotation | `forloopbound` analyzer |
| 3a | Channel buffer sizes must be a typed constant or unbuffered | `chanconst` analyzer |
| 3b | HTTP request bodies must be wrapped in `http.MaxBytesReader` before being read | `maxbytes` analyzer |
| 4 | Function length cap | `funlen` (existing config; cap unchanged) |
| 5 | `internal/assert` exists; assertion arguments are side-effect free and non-trivial | `assertpure` analyzer |
| 6 | No package-level mutable `var` (sentinel errors + compile-time interface checks excepted) | `gochecknoglobals` |
| 7 | Errors are not silently discarded | `errcheck` + `internal/oops` (existing) |
| 8 | `//go:build` tags drawn from a closed catalog (`linux`, `darwin`, `integration`) | `tagcatalog` analyzer |
| 9 | No `**T` types | `nodoubleptr` analyzer |
| 10 | Max compiler/lint warnings; static analysis surface monotonically tightens | existing policy |

#### Rules we deliberately dropped

- Rule 3 in its broad C form ("no dynamic memory after init"). Go's
  GC makes the literal rule meaningless; the *spirit* (bounded
  inputs) is preserved by the two narrow checks 3a/3b above. Wider
  "no unbounded `append` from untrusted input" stays a review
  concern — there is no honest static check for it.
- Rule 5's "two assertions per function" density target. We adopt
  the *vocabulary* (`internal/assert`) and the *purity check*
  (`assertpure`); a per-function count would be density theatre.
- Rule 7's "validate every parameter" half. We have the tool
  (`internal/assert`); whether you use it in any given function is
  judgement, caught at review.
- Rule 9's "no function pointers" / one-level-deref limit. Go's
  interface dispatch and closures are the language's main
  abstraction tool; banning them is not viable. We keep the
  `**T` ban (`nodoubleptr`) as the one piece that translates.

#### Escape-hatch convention

Every analyzer that flags a pattern accepts the same suppression
directive on the offending line or the line immediately above:

```go
//daft:lint:disable=norecursion reason="protobuf decoder, depth-capped by Validate above"
func decode(b []byte) (*Msg, error) { ... }
```

- Multiple analyzers in one disable: `disable=A,B`.
- `reason="…"` is mandatory and must be non-empty — the
  `lintreasons` analyzer fails the build otherwise.
- Suppressions are reviewable artefacts: every PR with a new
  `//daft:lint:disable=` line gets the reason examined.

`//nolint:…` (the golangci-lint form) is still allowed for the
linters it controls, with the same one-line-comment justification
rule from "Static analysis is not optional." The two prefixes
coexist: `daft:lint:` for our custom analyzers, `nolint:` for
upstream linters.

#### Where it lives

- `internal/assert/` — the assertion vocabulary.
- `internal/lint/analyzers/<name>/` — custom analyzers, one
  package each.
- `cmd/daft-lint/` — the multichecker binary that bundles them.
- `make lint` runs `golangci-lint` and then `daft-lint`.

New analyzers ship with `analysistest`-driven unit tests; see
existing analyzer dirs for the pattern.

## Pull request expectations

- Describe the *why*, not just the *what* (the diff already shows the what).
- Update `PLAN.md` if the change affects scope or direction.
- Update `coverage.txt` only when intentional.
- Run `make` (the default target) locally before pushing.
