# darkaifactory — PLAN

This is the living plan for the project. It MUST be kept up to date as work
progresses. Whenever scope, direction, milestones, or open questions change,
update this file in the same change that introduces the change.

## Project goal

_TBD — fill in once the product direction is articulated._ The working name is
`darkaifactory`, and the primary deliverable is a Go command-line tool called
`daft` (see `cmd/daft`).

## Status

- [x] Repository skeleton (Go layout, Nix flake, direnv, git hooks)
- [x] CONTRIBUTING.md
- [x] `daft` CLI scaffolding (cobra-based, with `completion` and `version` subcommands)
- [x] GitHub CI (`validate-build`, `validate-tag`)
- [ ] Define project domain and scope (what does `daft` actually do?)
- [ ] First feature slice

## Architecture

- `cmd/<name>/main.go` — entry points for binaries. Currently only `daft`.
- `internal/<pkg>` — non-exported library code; everything that isn't a `main`
  package or a top-level config file lives here.
- `version.txt` — single source of truth for the released version. Read by
  `flake.nix` (Nix package version) and embedded into the `daft` binary at
  build time.

## Conventions

See [CONTRIBUTING.md](CONTRIBUTING.md). Highlights:

- Commit early and often.
- No Go files at the repository root — they go in `cmd/` or `internal/`.
- Every feature ships with tests.
- Use runtime assertions liberally: any invariant that can be checked, should be.
- Write doc comments on exported identifiers (so `go doc` picks them up).
- Any function that ingests outside input gets a `FuzzXxx` target — see
  `CONTRIBUTING.md` for the pattern.

## Open questions

- What is the actual product? `daft` is currently a shell with no real
  subcommands beyond `completion` and `version`.
- Code coverage baseline: the pre-commit hook compares against `coverage.txt`
  in the repo root. Initial baseline is whatever the first run produces; we
  refuse new commits that drop coverage by more than 1.0 percentage points.

## How to update this plan

When you (human or agent) change direction, finish a milestone, or surface a
new open question, edit this file in the same commit. The first line of
CLAUDE.md points future agents here, so stale plans actively mislead.
