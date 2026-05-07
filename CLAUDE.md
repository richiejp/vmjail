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
make fmt      # format Go code
make lint     # golangci-lint
make test     # go test ./...
make cover    # update coverage.txt baseline
make hooks    # install pre-commit hook into .git/hooks
```

## Things that bite

- The pre-commit hook fails the commit if total Go test coverage drops by more
  than 1.0 percentage point vs `coverage.txt`. If you legitimately need a
  bigger drop (e.g. you deleted a heavily-tested package), update `coverage.txt`
  in the same commit and explain it in the message.
- Tag pushes are validated against `version.txt` by CI — bump `version.txt`
  before tagging.
