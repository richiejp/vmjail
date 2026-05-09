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
- `Makefile` — common dev targets: `make fmt lint test cover hooks`.
- `scripts/hooks/` — git hooks installed by `make hooks`.

## Quick commands

```sh
make fmt          # format Go code
make lint         # golangci-lint
make test         # go test ./...
make cover        # update coverage.txt baseline
make bench        # update bench.txt baseline (run with DAFT_* env vars
                  # set to also include BenchmarkHelloBoot)
make bench-check  # fail >2x regressions, warn >1.1x, vs bench.txt
make hooks        # install pre-commit hook into .git/hooks
```

## Things that bite

- The pre-commit hook fails the commit if total Go test coverage drops by more
  than 1.0 percentage point vs `coverage.txt`. If you legitimately need a
  bigger drop (e.g. you deleted a heavily-tested package), update `coverage.txt`
  in the same commit and explain it in the message.
- The pre-commit hook also fails if any benchmark in `bench.txt` regresses by
  more than 2.00× (>100% slower); a >1.10× regression only warns. The point is
  to catch order-of-magnitude bugs, not micro-optimisations. Update `bench.txt`
  with `make bench` if a real perf change is intentional.
- Tag pushes are validated against `version.txt` by CI — bump `version.txt`
  before tagging.
