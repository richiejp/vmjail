# darkaifactory

> _Project description TBD — see [PLAN.md](PLAN.md) for current direction
> and [ARCHITECTURE.md](ARCHITECTURE.md) for the resource model and
> conventions._

`darkaifactory` is the source repository for the `daft` command-line tool.

## Getting started

If you have [Nix](https://nixos.org) and [direnv](https://direnv.net) installed,
just `cd` into the repo and run:

```sh
direnv allow
```

That drops you into a dev shell with Go and the linting tools pinned. Without
direnv, run `nix develop` instead.

Once in the shell:

```sh
make hooks   # install git hooks (pre-commit fmt+lint+test+coverage)
make test    # run all Go tests
go run ./cmd/daft --help
```

## Building

```sh
nix build              # builds the daft binary using version.txt
./result/bin/daft version
```

## Repository layout

- `cmd/daft/` — `daft` CLI entry point.
- `internal/` — all reusable Go code (nothing at the repo root).
- `version.txt` — single source of truth for the released version.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

TBD.
