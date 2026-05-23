.PHONY: all check check-submodule-revs verify fmt check-fmt lint lint-golangci lint-daft test cover cover-check bench bench-check fuzz hooks build build-daft build-daft-agent build-daft-hello-container build-daft-sandboxd build-daft-orchestrator build-initramfs build-hello-bundle integration integration-build proto clean help

# Single source of truth for the version
VERSION := $(shell cat version.txt)
LDFLAGS := -X github.com/localai-org/darkaifactory/internal/version.Version=$(VERSION)

# Our own Go code lives under cmd/ and internal/. third_party/ holds
# vendored upstream sources (Linux kernel, Cloud Hypervisor, u-root) that
# we build from but do not own — exclude them from formatting, linting,
# testing, and coverage. OUR_GO_PKGS is what every Go-tool target operates
# on.
OUR_GO_PKGS := ./internal/... ./cmd/...
OUR_GO_DIRS := internal cmd

# Output directory for every locally-built artefact (Go binaries
# plus the VM-side initramfs/bundle). Gitignored. Keep at the top so
# every Makefile section can reference BIN_DIR without ordering
# constraints.
BIN_DIR := bin

# Default target follows the GNU convention: `make all` builds the
# project and runs the fast checks. Pre-commit and CI use `make
# verify`, which adds integration + benchmarks + a 1s fuzz sweep.
all: build check

# check is the fast pre-flight: fmt, lint, unit tests, coverage gate.
# Reproducer corpora under testdata/fuzz/ are replayed automatically
# by `go test`, so any committed crash surfaces here too.
check: check-fmt check-submodule-revs lint test cover-check

# Keep third_party/submodule-revs.json in sync with `git submodule
# status` + .gitmodules. The flake reads that file via
# builtins.fetchGit; if it drifts we'd rebuild against the wrong rev
# or URL. Fails the gate and regenerates the file on mismatch.
check-submodule-revs:
	@scripts/check-submodule-revs.sh

# verify is the full gate set used by the pre-commit hook. Active
# fuzzing runs with a tight 1s/target budget — enough to catch a
# crash that's deterministic from the seed corpus, not enough to
# delay commits significantly. Manual / nightly fuzzing uses
# `make fuzz` (default 30s/target).
verify: check integration bench-check
	@FUZZ_TIME=1s scripts/fuzz.sh

help:
	@echo "Targets:"
	@echo "  all         - build the project + run the fast check gates (default)"
	@echo "  check       - fmt + submodule-revs + lint + unit tests + coverage gate (fast pre-flight)"
	@echo "  verify      - check + integration + bench + 1s fuzz sweep (pre-commit gate)"
	@echo "  fmt         - format Go code with gofumpt"
	@echo "  check-fmt   - fail if anything is unformatted"
	@echo "  lint        - run golangci-lint"
	@echo "  test        - go test ./..."
	@echo "  cover       - update committed coverage.txt baseline"
	@echo "  cover-check - fail if total coverage drops vs coverage.txt OR any file is <80%"
	@echo "  bench       - update committed bench.txt baseline"
	@echo "  bench-check - fail on >2x regressions, warn on >1.1x, vs bench.txt"
	@echo "  fuzz        - run every FuzzXxx target (FUZZ_TIME, default 30s, per target)"
	@echo "  integration - build initramfs + bundle locally, run e2e tests (real VM boot)"
	@echo "  proto       - regenerate Go code from proto/*.proto"
	@echo "  hooks       - install git pre-commit hook into .git/hooks"
	@echo "  build       - build daft, daft-agent, daft-sandboxd, daft-orchestrator into ./bin/ (dev only)"

## Formatting ----------------------------------------------------------------

fmt:
	gofumpt -w $(OUR_GO_DIRS)

check-fmt:
	@unformatted=$$(gofumpt -l $(OUR_GO_DIRS)); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "All files formatted correctly"

## Linting -------------------------------------------------------------------
#
# Two-stage: upstream golangci-lint first (broad coverage, well-known
# rules), then our own daft-lint binary (custom analyzers under
# internal/lint/analyzers/, bundled by cmd/daft-lint). The order is
# load-bearing — golangci-lint's diagnostics overlap with parts of
# what our custom analyzers check, so running it first surfaces the
# generic-named issues with the more recognisable tool. See
# CONTRIBUTING.md "Power of 10 (Go)" for the per-analyzer policy.

lint: lint-golangci lint-daft

lint-golangci:
	golangci-lint run $(OUR_GO_PKGS)

# daft-lint is `go run`-invoked rather than pre-built; the cost is
# trivial (under a second) and the alternative would require a
# build-then-run dance that desyncs from go.mod whenever the
# analyzer package changes.
lint-daft:
	@go run ./cmd/daft-lint $(OUR_GO_PKGS)

## Testing ------------------------------------------------------------------

test:
	go test $(OUR_GO_PKGS)

## Coverage ------------------------------------------------------------------

# Compute total coverage as a percentage with one decimal point.
# Writes coverage.out (transient) and prints the number to stdout.
COVER_OUT := coverage.out
COVER_BASELINE := coverage.txt

cover:
	@go test -covermode=atomic -coverprofile=$(COVER_OUT) $(OUR_GO_PKGS) > /dev/null
	@scripts/cover-check.sh --update $(COVER_OUT) $(COVER_BASELINE)
	@rm -f $(COVER_OUT)

# cover-check: three gates (see CONTRIBUTING.md and scripts/cover-check.sh).
#   1. Total coverage >= committed baseline (no drop tolerance).
#   2. Per-file coverage >= 80% for every non-test source file with
#      more than 10 statements.
#   3. (--update mode) baseline only ever rises.
cover-check:
	@go test -covermode=atomic -coverprofile=$(COVER_OUT) $(OUR_GO_PKGS) > /dev/null
	@scripts/cover-check.sh $(COVER_OUT) $(COVER_BASELINE)
	@rm -f $(COVER_OUT)

## Benchmarks ----------------------------------------------------------------
#
# We track ns/op per benchmark in bench.txt to catch order-of-magnitude
# regressions — performance noise is too high for a tight gate, but a
# 2x slowdown almost always means a bug.
#
# `make bench` updates the baseline; `make bench-check` runs the
# benchmarks and compares them to the baseline (>2x fails, >1.1x warns).
# The integration boot benchmark (BenchmarkHelloBoot) is gated on the
# DAFT_* artifact env vars and /dev/kvm; without them it self-skips and
# we just compare whichever benches did run.

BENCH_BASELINE := bench.txt
# Use the default 1s benchtime: tiny benches run millions of iterations
# and stabilise; the e2e BenchmarkHelloBoot at ~300 ms/op gets 3-4
# iterations naturally. Lower benchtimes (e.g. -benchtime=3x) flap badly
# on the microbenchmarks because cobra setup is dominated by GC noise.
BENCH_FLAGS := -bench=. -run=^$$ -benchmem

bench:
	@go test $(BENCH_FLAGS) $(OUR_GO_PKGS) | scripts/bench-check.sh --update $(BENCH_BASELINE)

bench-check:
	@go test $(BENCH_FLAGS) $(OUR_GO_PKGS) | scripts/bench-check.sh $(BENCH_BASELINE)

## Fuzzing -------------------------------------------------------------------
#
# fuzz: run every FuzzXxx target across our packages for FUZZ_TIME
# each (default 30s; pre-commit overrides to 1s via the verify
# recipe). Reproducers Go writes under testdata/fuzz/FuzzXxx/<id>
# are automatically replayed by `go test` thereafter, so a one-off
# crash becomes a permanent regression test once the file is
# committed.
fuzz:
	@scripts/fuzz.sh

## E2E tests -----------------------------------------------------------------
#
# `make integration` runs the e2e tests in
# internal/sandbox/integration_test.go (build tag: linux,integration).
# The target name is historical; by the unit/integration/e2e
# taxonomy in CONTRIBUTING.md these are e2e because they boot a
# real cloud-hypervisor against the kernel / initramfs / virtiofsd /
# OCI bundle artefacts.
#
# Slow-moving artefacts (cloud-hypervisor, kernel, virtiofsd, crun,
# u-root) come from the Nix dev shell via DAFT_* env vars — pinned
# in flake.nix, only realised when their sources change. Per-commit
# artefacts (the initramfs and the hello-bundle, both of which embed
# our own Go binaries) assemble locally under bin/ via `go build` +
# u-root, so iterating on agent / hello-container code does not
# flood /nix/store with a new store path each commit.
#
# Linux + KVM is required, not optional. The pre-commit hook runs
# this on every commit — there is no graceful degradation.

# Local artefact outputs. Both live under bin/ (gitignored) so they
# don't escape into the repo or into /nix/store.
INITRAMFS_OUT := $(BIN_DIR)/initramfs.cpio.gz
BUNDLE_OUT    := $(BIN_DIR)/hello-bundle

# build-initramfs assembles the sandbox-VM initramfs locally with
# u-root, using daft-agent + crun from the dev shell. Mirrors the
# layout produced by the flake's `initramfs` derivation (which the
# `daft-sandboxd` build still uses).
#
# Requires the dev shell's DAFT_CRUN / DAFT_UROOT / DAFT_UROOT_SRC
# env vars to be set; the recipe asserts that up front so a failed
# build doesn't dump a confusing stack trace.
build-initramfs: build-daft-agent
	@: "$${DAFT_UROOT:?DAFT_UROOT not set — enter the Nix dev shell}"
	@: "$${DAFT_UROOT_SRC:?DAFT_UROOT_SRC not set — enter the Nix dev shell}"
	@: "$${DAFT_CRUN:?DAFT_CRUN not set — enter the Nix dev shell}"
	@echo "[initramfs] assembling $(INITRAMFS_OUT)"
	@rm -rf $(BIN_DIR)/initramfs-work && mkdir -p $(BIN_DIR)/initramfs-work
	@cp -r --no-preserve=mode $$DAFT_UROOT_SRC/. $(BIN_DIR)/initramfs-work/
	@# u-root spawns `go` internally to compile cmds/core/init; it
	@# locates the toolchain via $GOROOT/bin/go, so spell GOROOT out
	@# explicitly (the dev shell's `go env GOROOT` resolves it).
	@cd $(BIN_DIR)/initramfs-work && GOROOT="$$(go env GOROOT)" $$DAFT_UROOT \
		-o ../initramfs.cpio \
		-defaultsh="" \
		-base=/dev/null \
		-files "$(CURDIR)/$(BIN_DIR)/daft-agent:bin/daft-agent" \
		-files "$$DAFT_CRUN:bin/crun" \
		-uinitcmd=/bin/daft-agent \
		./cmds/core/init
	@gzip -f -n $(BIN_DIR)/initramfs.cpio
	@rm -rf $(BIN_DIR)/initramfs-work

# build-hello-bundle assembles the OCI bundle the integration test
# feeds to crun. Mirrors the flake's `hello-bundle` derivation
# layout — one binary in rootfs/ plus the checked-in config.json.
build-hello-bundle: build-daft-hello-container
	@echo "[hello-bundle] assembling $(BUNDLE_OUT)"
	@rm -rf $(BUNDLE_OUT)
	@mkdir -p $(BUNDLE_OUT)/rootfs
	@cp $(BIN_DIR)/daft-hello-container $(BUNDLE_OUT)/rootfs/hello
	@chmod +x $(BUNDLE_OUT)/rootfs/hello
	@cp contrib/hello-bundle/config.json $(BUNDLE_OUT)/config.json

# integration-build prepares everything `make integration` needs to
# run. The slow-moving binaries are already on disk from the dev
# shell; only the per-commit artefacts have to be built.
integration-build: build-initramfs build-hello-bundle
	@: "$${DAFT_CLOUD_HYPERVISOR:?DAFT_CLOUD_HYPERVISOR not set — enter the Nix dev shell}"
	@: "$${DAFT_KERNEL:?DAFT_KERNEL not set — enter the Nix dev shell}"
	@: "$${DAFT_VIRTIOFSD:?DAFT_VIRTIOFSD not set — enter the Nix dev shell}"
	@: "$${DAFT_NIX_IMAGE:?DAFT_NIX_IMAGE not set — enter the Nix dev shell}"
	@: "$${DAFT_NIX_TOOLING_IMAGE:?DAFT_NIX_TOOLING_IMAGE not set — enter the Nix dev shell}"
	@: "$${DAFT_NIX_FLAKE_FIXTURE:?DAFT_NIX_FLAKE_FIXTURE not set — enter the Nix dev shell}"

integration: integration-build
	@echo "[e2e] booting real VMs against /dev/kvm..."
	@DAFT_INITRAMFS=$(CURDIR)/$(INITRAMFS_OUT) \
		DAFT_BUNDLE=$(CURDIR)/$(BUNDLE_OUT) \
		go test -v -tags=integration -count=1 -timeout 20m ./internal/sandbox/ ./internal/testdaemons/

## Git hooks -----------------------------------------------------------------

hooks:
	@mkdir -p .git/hooks
	@cp scripts/hooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed .git/hooks/pre-commit"

## Building ------------------------------------------------------------------

# build: produce all shipped binaries, statically linked (CGO=0).
# Output lands under bin/ — never the repo root — so there's no
# confusion between a dev `make build` and a release artefact:
#   - daft:           host-side client CLI
#   - daft-agent:     in-VM PID-1 agent
#   - daft-sandboxd:  sandbox-host single executable. Note: a `make
#     build` produces a daft-sandboxd with STUB embedded artifacts —
#     the real cloud-hypervisor / kernel / initramfs are pulled in
#     only by the Nix build (`nix build .#daft-sandboxd`). Use Nix
#     for any release artefact.

build: build-daft build-daft-agent build-daft-sandboxd build-daft-orchestrator

build-daft:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft ./cmd/daft

build-daft-agent:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-agent ./cmd/daft-agent

# daft-hello-container is the workload binary that runs *inside the
# container*. The integration test's hello-bundle rootfs/hello is a
# copy of this. Statically linked: it runs against the VM's minimal
# initramfs, no host libc.
build-daft-hello-container:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-hello-container ./cmd/daft-hello-container

build-daft-sandboxd:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-sandboxd ./cmd/daft-sandboxd

build-daft-orchestrator:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-orchestrator ./cmd/daft-orchestrator

## Codegen -------------------------------------------------------------------

# proto: regenerate Go code from .proto files. Run this whenever a
# .proto under proto/ changes; the generated code under internal/genpb
# is committed so a normal build never needs buf or the codegen
# plugins. Plugins are pinned via tools/tools.go and invoked through
# `go run` so versions track go.mod. `buf` itself comes from the Nix
# dev shell.
proto:
	@command -v buf >/dev/null || { echo "buf not on PATH; enter the Nix dev shell"; exit 1; }
	buf generate

clean:
	rm -rf $(BIN_DIR) $(COVER_OUT)
	@# Legacy: earlier builds put binaries at the repo root. Clean
	@# those too in case someone is upgrading from an older Makefile.
	rm -f daft daft-agent daft-sandboxd
	@# Legacy: pre-2026-05-13 `make integration-build` planted
	@# result-* symlinks at the repo root. We no longer create them
	@# (the dev shell pins the same store paths), but keep the rm so
	@# upgrading developers get a clean tree without an extra step.
	rm -f result result-ch result-kernel result-virtiofsd result-initramfs result-bundle
