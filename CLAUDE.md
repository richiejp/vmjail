# CLAUDE.md

## FIRST: read [PLAN.md](PLAN.md)

[PLAN.md](PLAN.md) is the living plan for this project. Read it before doing
anything else, and **keep it up to date at all times** — when scope changes,
when a milestone lands, when a new open question appears, edit `PLAN.md` in
the same change.

## Project guidelines

- See [CONTRIBUTING.md](CONTRIBUTING.md) for the contributor rules. They apply
  to agents too.
- See [README](README) for the user-facing overview.
- See [OBSERVABILITY.md](OBSERVABILITY.md) for the host-side `/proc`/cgroup
  sampler, guest-side eBPF gadgets and build-progress summaries, and where to
  read or stream a Sandbox's resource trace when a test fails.

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
make verify       # check + e2e + bench-check + 1s fuzz sweep
                  # (= what the pre-commit hook runs)
make fuzz         # active fuzz every FuzzXxx target (FUZZ_TIME=30s default)
make integration  # e2e tests: real VM boot against /dev/kvm
                  # (target name is historical; see CONTRIBUTING.md
                  # "Test categories" for the unit / integration /
                  # e2e distinction — these are e2e by that taxonomy)
make proto        # regenerate Go code from proto/*.proto
make cover        # update coverage.txt baseline (refuses to lower)
make bench        # update bench.txt baseline
make hooks        # install pre-commit hook into .git/hooks
```

## Context-efficient investigation

Keep source and diagnostic output bounded. For Go, navigate semantically before
reading files linearly:

```sh
scripts/go-nav.sh symbol WorkspaceApply
scripts/go-nav.sh definition internal/mcpserver/server.go:120:8
scripts/go-nav.sh references internal/mcpserver/server.go:120:8
scripts/go-nav.sh check internal/mcpserver/server.go
```

The wrapper caps output at 120 lines. An agent runtime may instead register
`gopls mcp` and use the same symbol, definition, reference, call-hierarchy, and
diagnostic operations directly.

Search first, then read the smallest relevant source window—normally at most
200 lines. Do not dump complete large files, generated protobufs, full test
logs, or unbounded traces into the conversation. Save verbose command output to
a temporary file and report a one-line success or a bounded failure excerpt.
Give subagents compact task briefs with relevant paths rather than the full
parent transcript, and use a fresh session with a concise handoff after a major
milestone.

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
- `flake.nix` reads submodule revs + upstream URLs from
  `third_party/submodule-revs.json` (one fetchGit per submodule) rather than
  scanning `./third_party/<sub>` on every eval. After `git submodule update`,
  run `make check-submodule-revs` — it regenerates the JSON from `.gitmodules`
  + `git submodule status` and fails (with the new file already on disk) if
  anything drifted, so you can stage it and retry. The gate also runs as part
  of `make check`.
