# CLAUDE.md

## FIRST: read [PLAN.md](PLAN.md)

[PLAN.md](PLAN.md) is the living plan for this project. Read it before doing
anything else, and **keep it up to date at all times** — when scope changes,
when a milestone lands, when a new open question appears, edit `PLAN.md` in
the same change.

## Project guidelines

- See [CONTRIBUTING.md](CONTRIBUTING.md) for the contributor rules. They apply
  to agents too.
- See [README.md](README.md) for the user-facing overview.

## Repository layout

- `cmd/daft/` — the `daft` CLI entry point.
- `internal/` — all reusable Go code. **No `.go` files at the repo root.**
- `version.txt` — single source of truth for the version number. Bumping it
  bumps the Nix package version and the `daft version` output.
- `flake.nix` / `.envrc` — Nix dev shell + direnv. `direnv allow` to enter.
- `Makefile` — common dev targets. `make help` lists them all.
- `scripts/hooks/` — git hooks installed by `make hooks`.

## Quick commands

```sh
make all          # default: build the project + run check
make check        # fmt + lint + unit tests + coverage gate (fast)
make verify       # check + integration + bench-check + 1s fuzz sweep
                  # (= what the pre-commit hook runs)
make fuzz         # active fuzz every FuzzXxx target (FUZZ_TIME=30s default)
make integration  # KVM-gated end-to-end VM boot tests
make proto        # regenerate Go code from proto/*.proto
make cover        # update coverage.txt baseline (refuses to lower)
make bench        # update bench.txt baseline
make hooks        # install pre-commit hook into .git/hooks
```

## Things that bite

- The pre-commit hook fails the commit if total Go test coverage drops below
  the committed `coverage.txt` baseline (no tolerance) or if any single
  non-test file falls under 80%. **Coverage only ever rises** — even when
  deleting heavily-tested code. If your deletion lowers the percentage,
  add tests elsewhere in the same change so the baseline rises (or at
  worst stays put). Do not lower `coverage.txt` by hand. See
  CONTRIBUTING.md "Verification monotonically increases."
- The pre-commit hook also fails if any benchmark in `bench.txt` regresses by
  more than 2.00× (>100% slower); a >1.10× regression only warns. The point is
  to catch order-of-magnitude bugs, not micro-optimisations. Update `bench.txt`
  with `make bench` if a real perf change is intentional.
- Tag pushes are validated against `version.txt` by CI — bump `version.txt`
  before tagging.
