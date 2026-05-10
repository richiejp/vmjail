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
linting, tests, coverage regression, the benchmark gate, and the
KVM-gated integration suite. **`--no-verify` is never OK.** If a
gate is wrong for your change, fix the gate and explain why in the
same commit. Bypassing it re-creates the failure modes the gate was
added to prevent.

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

### Integration tests on every commit

The pre-commit hook runs the KVM-gated integration tests in
`internal/sandbox/integration_test.go` (build tag:
`linux,integration`) on every commit, via `make integration`. They:

- nix-build the artefacts (`cloud-hypervisor`, `kernel`, `virtiofsd`,
  `initramfs`, `hello-bundle`),
- spawn real VMs via Cloud Hypervisor against `/dev/kvm`,
- and assert the full pull → bundle → boot → run pipeline.

**Linux + KVM is a hard prerequisite for committing.** There is no
useful work in this codebase that doesn't need it, so any developer
who can write sandbox code already has the prerequisites. CI
environments without KVM are useless for this project and don't
gate commits. There are no skips: missing artefact → `t.Fatalf`,
missing `/dev/kvm` → `t.Fatalf`, broken Nix → the make target
fails. And as above, `--no-verify` is never OK — if the gate is
wrong for your change, fix the gate.

Artefact resolution is automatic: env vars (`DAFT_*`) override,
otherwise the resolver looks for the `result-*` symlinks that
`make integration-build` creates at the repo root. Silently
skipping integration tests is exactly the kind of "green CI,
broken merge" failure mode this gate exists to prevent.

### Coverage regression check

The pre-commit hook (via `make cover-check` and
`scripts/cover-check.sh`) enforces two gates. Both must pass:

1. **Total coverage** stays within 1.0pp of the committed
   `coverage.txt` baseline. Catches "we drifted lower" overall.
2. **Per-file coverage** is at least 80% for every non-test source
   file with more than 10 statements. Catches the case where one
   file is dark while the rest of the codebase masks it — high
   total coverage isn't a substitute for "every meaningful file is
   exercised."

If `cover-check` fails, the answer is **never** to lower the
baseline or weaken the per-file threshold to make the bar fit. Add
the missing tests. Real options when coverage is hard:

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

Only update `coverage.txt` when you've genuinely deleted
heavily-tested code (so the new total *can* be higher than the
old one would predict). Explain the bump in the commit message.

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
