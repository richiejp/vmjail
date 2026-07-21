.PHONY: all check check-submodule-revs check-deploy-skill verify fmt check-fmt lint lint-golangci lint-daft test cover cover-check bench bench-check fuzz hooks build build-daft build-daft-agent build-daft-hello-container build-daft-sandboxd build-daft-orchestrator build-artefacts build-initramfs build-hello-bundle dev integration integration-build update-egress-cache proto gen-bpf clean help

# Single source of truth for the version
VERSION := $(shell cat version.txt)
LDFLAGS := -X github.com/richiejp/darkaifactory/internal/version.Version=$(VERSION)

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
check: check-fmt check-submodule-revs check-deploy-skill lint test cover-check

# Keep third_party/submodule-revs.json in sync with `git submodule
# status` + .gitmodules. The flake reads that file via
# builtins.fetchGit; if it drifts we'd rebuild against the wrong rev
# or URL. Fails the gate and regenerates the file on mismatch.
check-submodule-revs:
	@scripts/check-submodule-revs.sh

# The deployment skill is executable product surface even though it is not Go.
# Keep its POSIX-shell install/upgrade/rollback contract in the fast gate.
check-deploy-skill:
	@.agents/skills/deploy-daft-node/scripts/test-scripts.sh

# verify is the full gate set used by the pre-commit hook. `dev`
# builds every real artefact (Go binaries + VM toolchain + initramfs +
# bundle) so a broken full build fails the commit, not just CI. Active
# fuzzing runs with a tight 1s/target budget — enough to catch a
# crash that's deterministic from the seed corpus, not enough to
# delay commits significantly. Manual / nightly fuzzing uses
# `make fuzz` (default 30s/target).
#
# verify does NOT run `nix flake check` — that builds our Go binaries
# and initramfs/bundle via their Nix derivations (a path parallel to
# the `go build`/Make recipes `dev` uses), which is the slow cold
# build. That gate lives in the pre-push hook + CI instead.
verify: check dev integration bench-check
	@FUZZ_TIME=1s scripts/fuzz.sh

help:
	@echo "Targets:"
	@echo "  all         - build the project + run the fast check gates (default)"
	@echo "  check       - fmt + submodule-revs + deployment skill + lint + unit tests + coverage gate"
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
	@echo "  gen-bpf     - regenerate Tier-2 eBPF gadget loaders (bpf2go)"
	@echo "  hooks       - install git pre-commit (make verify) + pre-push (nix flake check) hooks into .git/hooks"
	@echo "  build       - build daft, daft-agent, daft-sandboxd, daft-orchestrator into ./bin/ (dev only)"
	@echo "  dev         - build everything in the dev shell: Go binaries + VM toolchain (kernel/CH/virtiofsd/crun/u-root) + initramfs + bundle"

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

# Each coverage invocation owns a unique profile. This keeps `make cover` and
# `make cover-check` correct when an editor, agent, or parallel make invokes
# them concurrently; sharing coverage.out allowed one invocation to remove the
# other's profile between `go test` and the checker.
COVER_OUT_PREFIX := coverage.out
COVER_BASELINE := coverage.txt

cover:
	@set -e; profile=$$(mktemp ./$(COVER_OUT_PREFIX).XXXXXX); log=$$(mktemp); \
	trap 'rm -f "$$profile" "$$log"' EXIT; \
	if ! go test -covermode=atomic -coverprofile="$$profile" $(OUR_GO_PKGS) > "$$log"; then cat "$$log"; exit 1; fi; \
	scripts/cover-check.sh --update "$$profile" $(COVER_BASELINE)

# cover-check: three gates (see CONTRIBUTING.md and scripts/cover-check.sh).
#   1. Total coverage >= committed baseline (no drop tolerance).
#   2. Per-file coverage >= 80% for every non-test source file with
#      more than 10 statements.
#   3. (--update mode) baseline only ever rises.
cover-check:
	@set -e; profile=$$(mktemp ./$(COVER_OUT_PREFIX).XXXXXX); log=$$(mktemp); \
	trap 'rm -f "$$profile" "$$log"' EXIT; \
	if ! go test -covermode=atomic -coverprofile="$$profile" $(OUR_GO_PKGS) > "$$log"; then cat "$$log"; exit 1; fi; \
	scripts/cover-check.sh "$$profile" $(COVER_BASELINE)

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
# Test-fixture OCI images / flake fixture, built fresh from the flake by
# `integration-build` (nix build -o) so they track source instead of a
# frozen dev-shell path.
NIX_IMAGE_OUT         := $(BIN_DIR)/nix-shell-image
NIX_TOOLING_IMAGE_OUT := $(BIN_DIR)/nix-tooling-image
NIX_FLAKE_FIXTURE_OUT := $(BIN_DIR)/nix-flake-fixture
# Nix-less make-check image (D6c.4 slice 1): toolchain only, runs
# `make check` offline. The Go deps are delivered separately as the
# vendor tree (daft.goModules) the test copies into its source mount —
# baking go:embed files through dockerTools corrupts them. See
# flake.nix `daft-check-image`.
NIX_CHECK_IMAGE_OUT   := $(BIN_DIR)/daft-check-image
NIX_CHECK_VENDOR_OUT  := $(BIN_DIR)/daft-check-vendor
# In-sandbox image builder (buildah) for #101 slice 3 — the build-based
# devcontainer path. Consumed by TestClusterWorkspaceBuildahSpike.
NIX_BUILD_IMAGE_OUT   := $(BIN_DIR)/daft-build-image
# Sandbox-VM toolchain artefacts, built fresh from the flake by
# `build-artefacts` (nix build -o) instead of exported by the dev shell.
# Nobody runs these by hand — they're dependencies of the sandbox build
# (initramfs assembly) and runtime (VM boot). See PLAN.md "Dev-shell
# purification".
NIX_KERNEL_OUT    := $(BIN_DIR)/kernel
NIX_CHV_OUT       := $(BIN_DIR)/cloud-hypervisor
NIX_VIRTIOFSD_OUT := $(BIN_DIR)/virtiofsd
NIX_CRUN_OUT      := $(BIN_DIR)/crun
NIX_UROOT_OUT     := $(BIN_DIR)/u-root
NIX_UROOT_SRC_OUT := $(BIN_DIR)/u-root-src
# Real (//go:embed-bundled) daft-sandboxd, for the embedded-boot test.
NIX_SANDBOXD_OUT  := $(BIN_DIR)/daft-sandboxd-bundled
# Real (//go:embed-bundled build image) daft-orchestrator, for the
# auto-seeded-builder devcontainer-build test.
NIX_ORCH_OUT      := $(BIN_DIR)/daft-orchestrator-bundled
# iso-nested-qemu diversion fixtures (branch iso-nested-qemu): a tiny
# bootable-with-sshd NixOS ISO + a headless-qemu launcher image. Consumed
# by TestClusterNestedISOBoot, which boots the ISO as a nested L2 VM under
# qemu inside a daft sandbox and forwards its sshd out. See PLAN.md.
NIX_ISO_FIXTURE_OUT    := $(BIN_DIR)/iso-ssh-fixture
NIX_ISO_QEMU_IMAGE_OUT := $(BIN_DIR)/iso-qemu-runner-image

# Pinned egress-replay cache for the bun e2e test (PLAN.md "Pinned
# egress-replay cache"). The recorded cache is published as a one-layer
# OCI image to ghcr and pulled here; the tag is the first 16 hex of the
# fixture flake.lock's sha256, so bumping the lock forces a re-record via
# `make update-egress-cache`. integration-build pulls it (best-effort);
# `make integration` then replays it offline when present.
EGRESS_CACHE_OUT  := $(BIN_DIR)/egress-cache
EGRESS_CACHE_LOCK := internal/testdaemons/bun-flake.lock
EGRESS_CACHE_REPO := ghcr.io/richiejp/daft-egress-cache
EGRESS_CACHE_TAG  := $(shell sha256sum $(EGRESS_CACHE_LOCK) 2>/dev/null | cut -c1-16)
EGRESS_CACHE_REF  := $(EGRESS_CACHE_REPO):$(EGRESS_CACHE_TAG)

# build-artefacts realises the sandbox-VM toolchain from the flake into
# bin/ (a cache hit when nothing changed). These used to be exported by
# the dev shell at entry; building them here on demand keeps the shell
# to developer tooling and stops their store paths going stale. See
# PLAN.md "Dev-shell purification".
build-artefacts:
	@echo "[artefacts] nix build sandbox-VM toolchain"
	@nix build .#kernel           -o $(NIX_KERNEL_OUT)
	@nix build .#cloud-hypervisor -o $(NIX_CHV_OUT)
	@nix build .#virtiofsd        -o $(NIX_VIRTIOFSD_OUT)
	@nix build .#crun             -o $(NIX_CRUN_OUT)
	@nix build .#u-root           -o $(NIX_UROOT_OUT)
	@nix build .#u-root-src       -o $(NIX_UROOT_SRC_OUT)

# build-initramfs assembles the sandbox-VM initramfs locally with
# u-root, using daft-agent + crun. Mirrors the layout produced by the
# flake's `initramfs` derivation (which the `daft-sandboxd` build still
# uses). u-root, its source, and crun come from build-artefacts.
build-initramfs: build-daft-agent build-artefacts
	@echo "[initramfs] assembling $(INITRAMFS_OUT)"
	@rm -rf $(BIN_DIR)/initramfs-work && mkdir -p $(BIN_DIR)/initramfs-work
	@cp -r --no-preserve=mode $(CURDIR)/$(NIX_UROOT_SRC_OUT)/. $(BIN_DIR)/initramfs-work/
	@# u-root spawns `go` internally to compile cmds/core/init; it
	@# locates the toolchain via $GOROOT/bin/go, so spell GOROOT out
	@# explicitly (the dev shell's `go env GOROOT` resolves it).
	@cd $(BIN_DIR)/initramfs-work && GOROOT="$$(go env GOROOT)" \
		$(CURDIR)/$(NIX_UROOT_OUT)/bin/u-root \
		-o ../initramfs.cpio \
		-defaultsh="" \
		-base=/dev/null \
		-files "$(CURDIR)/$(BIN_DIR)/daft-agent:bin/daft-agent" \
		-files "$(CURDIR)/$(NIX_CRUN_OUT)/bin/crun:bin/crun" \
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

# integration-build = `dev` (every real artefact) plus the test-only
# fixtures the e2e suite mounts: the OCI fixture images, the flake
# fixture, and the //go:embed-bundled daft-sandboxd the embedded-boot
# test exercises. Building on `dev` keeps it as the single real-artefact
# build path so `make dev` and the e2e suite produce the same things,
# rather than maintaining a parallel artefact list here. Fixtures come
# fresh from the flake (cache hits when unchanged). See PLAN.md
# "Dev-shell purification".
integration-build: dev
	@echo "[images] nix build test-fixture images + bundled daft-sandboxd"
	@nix build .#nix-shell-image    -o $(NIX_IMAGE_OUT)
	@nix build .#nix-tooling-image  -o $(NIX_TOOLING_IMAGE_OUT)
	@nix build .#daft-check-image   -o $(NIX_CHECK_IMAGE_OUT)
	@nix build .#daft-build-image   -o $(NIX_BUILD_IMAGE_OUT)
	@nix build .#daft.goModules     -o $(NIX_CHECK_VENDOR_OUT)
	@nix build .#d6c1-flake-fixture -o $(NIX_FLAKE_FIXTURE_OUT)
	@nix build .#daft-sandboxd      -o $(NIX_SANDBOXD_OUT)
	@nix build .#daft-orchestrator  -o $(NIX_ORCH_OUT)
	@nix build .#iso-ssh-fixture       -o $(NIX_ISO_FIXTURE_OUT)
	@nix build .#iso-qemu-runner-image -o $(NIX_ISO_QEMU_IMAGE_OUT)
	@# Pull the pinned egress-replay cache so the bun test runs offline.
	@# Best-effort: skipped while recording, and a miss (not yet published,
	@# or no network) just leaves the bun test to record live this run.
	@if [ -z "$$DAFT_EGRESS_RECORD" ]; then \
		echo "[egress-cache] pull $(EGRESS_CACHE_REF)"; \
		go run ./cmd/daft-egress-cache pull-unpack $(EGRESS_CACHE_REF) $(EGRESS_CACHE_OUT) \
			|| echo "[egress-cache] pull failed (unpublished?) — bun test will record live"; \
	fi

# Lock file for `make integration`. Two concurrent runs race on
# /dev/kvm, host ports, vsock CIDs, and tap interface names, and
# corrupt each other in ways the test framework does not detect
# (silent hangs, EADDRINUSE, ghost taps). flock --nonblock makes a
# second invocation fail fast with a clear message naming the
# holder instead. See CONTRIBUTING.md "Concurrent integration runs
# are gated".
INTEGRATION_LOCK := $(CURDIR)/.git/daft-integration.lock

integration: integration-build
	@command -v flock >/dev/null || { echo "flock(1) not on PATH — install util-linux"; exit 1; }
	@( \
		exec 9>$(INTEGRATION_LOCK); \
		flock -n 9 || { \
			echo "make integration: another run already holds $(INTEGRATION_LOCK)"; \
			echo "  holder: $$(fuser $(INTEGRATION_LOCK) 2>&1 | tr -s ' ')"; \
			echo "  wait for it to finish, or kill it, then retry."; \
			exit 1; \
		}; \
		echo "[e2e] booting real VMs against /dev/kvm..."; \
		delegate=""; \
		if systemd-run --user --scope -p Delegate=yes --quiet -- true >/dev/null 2>&1; then \
			delegate="systemd-run --user --scope -p Delegate=yes --quiet --"; \
			echo "[e2e] cgroup delegation on — sandbox pkg gets accounting; testdaemons sandboxd self-wraps for limits"; \
		else \
			echo "[e2e] cgroup delegation off — no --user scope: the sandbox in-process host and sandboxd's self-wrap both need one, so e2e will fail (enable-linger / run under a Delegate=yes unit)"; \
		fi; \
		export DAFT_INITRAMFS=$(CURDIR)/$(INITRAMFS_OUT); \
			export DAFT_BUNDLE=$(CURDIR)/$(BUNDLE_OUT); \
			export DAFT_KERNEL=$(CURDIR)/$(NIX_KERNEL_OUT)/bzImage; \
			export DAFT_CLOUD_HYPERVISOR=$(CURDIR)/$(NIX_CHV_OUT)/bin/cloud-hypervisor; \
			export DAFT_VIRTIOFSD=$(CURDIR)/$(NIX_VIRTIOFSD_OUT)/bin/virtiofsd; \
			export DAFT_SANDBOXD_BUNDLED_BIN=$(CURDIR)/$(NIX_SANDBOXD_OUT)/bin/daft-sandboxd; \
			export DAFT_ORCH_BUNDLED_BIN=$(CURDIR)/$(NIX_ORCH_OUT)/bin/daft-orchestrator; \
			export DAFT_NIX_IMAGE=$$(realpath $(NIX_IMAGE_OUT)); \
			export DAFT_BUILD_IMAGE=$$(realpath $(NIX_BUILD_IMAGE_OUT)); \
			export DAFT_NIX_TOOLING_IMAGE=$$(realpath $(NIX_TOOLING_IMAGE_OUT)); \
			export DAFT_CHECK_IMAGE=$$(realpath $(NIX_CHECK_IMAGE_OUT)); \
			export DAFT_CHECK_VENDOR=$$(realpath $(NIX_CHECK_VENDOR_OUT)); \
			export DAFT_NIX_FLAKE_FIXTURE=$$(realpath $(NIX_FLAKE_FIXTURE_OUT)); \
			export DAFT_ISO_SSH_FIXTURE=$$(realpath $(NIX_ISO_FIXTURE_OUT)/iso/*.iso); \
			export DAFT_ISO_QEMU_IMAGE=$$(realpath $(NIX_ISO_QEMU_IMAGE_OUT)); \
			runflag=""; [ -n "$$INTEGRATION_RUN" ] && runflag="-run $$INTEGRATION_RUN"; \
			if [ -n "$$DAFT_EGRESS_RECORD" ]; then \
				echo "[egress-cache] RECORD: bun test records into $(EGRESS_CACHE_OUT)"; \
				rm -rf $(EGRESS_CACHE_OUT); mkdir -p $(EGRESS_CACHE_OUT); \
				export DAFT_EGRESS_PROXY_CACHE_RECORD=$$(realpath $(EGRESS_CACHE_OUT)); \
			elif [ -f $(EGRESS_CACHE_OUT)/index.json ]; then \
				echo "[egress-cache] REPLAY: bun test replays $(EGRESS_CACHE_OUT) offline"; \
				export DAFT_EGRESS_PROXY_CACHE=$$(realpath $(EGRESS_CACHE_OUT)); \
			fi; \
			rc=0; \
			echo "[e2e] sandbox pkg — in-process host shares the delegated scope: per-VM accounting, limits off"; \
			sandbox_log=$$(mktemp /tmp/daft-integration-sandbox.XXXXXX.log); \
			if $$delegate go test -v -tags=integration -count=1 -timeout 20m $$runflag ./internal/sandbox/ >$$sandbox_log 2>&1; then \
				tail -2 $$sandbox_log; rm -f $$sandbox_log; \
			else \
				echo "[e2e] sandbox failure; full log retained at $$sandbox_log"; \
				grep -n -E -B 12 -A 80 -- '--- FAIL:|panic:|fatal error|^FAIL([[:space:]]|$$)' $$sandbox_log || tail -200 $$sandbox_log; \
				rc=1; \
			fi; \
			echo "[e2e] testdaemons pkg — daft-sandboxd self-wraps into a private scope: per-VM limits enforced (fail-closed)"; \
			daemon_log=$$(mktemp /tmp/daft-integration-testdaemons.XXXXXX.log); \
			if go test -v -tags=integration -count=1 -timeout 20m $$runflag ./internal/testdaemons/ >$$daemon_log 2>&1; then \
				tail -2 $$daemon_log; rm -f $$daemon_log; \
			else \
				echo "[e2e] testdaemons failure; full log retained at $$daemon_log"; \
				grep -n -E -B 12 -A 80 -- '--- FAIL:|panic:|fatal error|^FAIL([[:space:]]|$$)' $$daemon_log || tail -200 $$daemon_log; \
				rc=1; \
			fi; \
			exit $$rc; \
	)

## update-egress-cache: re-record the bun dev-shell egress cache and
## publish it to ghcr. Run after bumping internal/testdaemons/bun-flake.lock
## (the tag tracks the lock's hash). Records online once (~13 min cold) via
## the bun test in RECORD mode, then packs + pushes the OCI artifact. Needs
## gh authenticated with write:packages.
update-egress-cache:
	@command -v gh >/dev/null 2>&1 || { echo "gh not on PATH (needed for ghcr auth)"; exit 1; }
	@gh auth token >/dev/null 2>&1 || { echo "gh not authenticated — run: gh auth login"; exit 1; }
	@echo "[egress-cache] recording $(EGRESS_CACHE_REF) (online; ~13 min cold)…"
	@DAFT_EGRESS_RECORD=1 INTEGRATION_RUN='^TestClusterWorkspaceBunDevServer$$' $(MAKE) integration
	@echo "[egress-cache] publishing $(EGRESS_CACHE_REF)…"
	@DAFT_GHCR_TOKEN=$$(gh auth token) DAFT_GHCR_USER=$$(gh api user -q .login) \
		go run ./cmd/daft-egress-cache pack-push $(EGRESS_CACHE_OUT) $(EGRESS_CACHE_REF)
	@echo "[egress-cache] published $(EGRESS_CACHE_REF) — make this public in the ghcr UI the first time."

## Git hooks -----------------------------------------------------------------

hooks:
	@mkdir -p .git/hooks
	@cp scripts/hooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@cp scripts/hooks/pre-push .git/hooks/pre-push
	@chmod +x .git/hooks/pre-push
	@echo "Installed .git/hooks/pre-commit + .git/hooks/pre-push"

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

build: build-daft build-daft-agent build-daft-sandboxd build-daft-orchestrator build-daft-mcp

# dev builds every artefact daft produces, entirely within the Nix
# dev shell: the host-side Go binaries (build), the sandbox-VM
# toolchain (build-artefacts: kernel, cloud-hypervisor, virtiofsd,
# crun, u-root), and the assembled VM initramfs + hello-bundle. The
# point is a single target that reproduces the full build inside a
# sandbox — which carries the same dev shell — so `nix develop` then
# `make dev` builds the lot.
#
# Note: `build` produces a daft-sandboxd with STUB embedded artefacts
# (see the `build` target). For a runnable, asset-embedded sandboxd,
# use `nix build .#daft-sandboxd` (what `make integration-build` does).
dev: build build-artefacts build-initramfs build-hello-bundle
	@echo "[dev] full build complete — artefacts under $(BIN_DIR)/"

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

build-daft-mcp:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-mcp ./cmd/daft-mcp

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

# gen-bpf: regenerate the Tier-2 eBPF gadget loaders (OBSERVABILITY.md
# "Tier 2"). For each gadget under internal/observability/ebpf/, dump the
# host kernel BTF to a transient vmlinux.h, run bpf2go (clang -> a
# committed *_bpfel.o + *_bpfel.go loader), then drop vmlinux.h. The
# committed objects mean a normal build / `make check` needs none of
# clang, bpftool, or BTF. NIX_HARDENING_ENABLE is cleared for the compile
# because the Nix cc-wrapper otherwise injects -fzero-call-used-regs /
# -fstack-protector, which clang rejects for the bpf target.
EBPF_DIR := internal/observability/ebpf
gen-bpf:
	@command -v bpftool >/dev/null || { echo "bpftool not on PATH; enter the Nix dev shell"; exit 1; }
	@command -v clang   >/dev/null || { echo "clang not on PATH; enter the Nix dev shell"; exit 1; }
	@test -r /sys/kernel/btf/vmlinux || { echo "/sys/kernel/btf/vmlinux absent; need a CONFIG_DEBUG_INFO_BTF host kernel"; exit 1; }
	for d in $(EBPF_DIR)/*/; do \
		[ -f "$$d"*_bpf.c ] || continue; \
		bpftool btf dump file /sys/kernel/btf/vmlinux format c > "$$d/vmlinux.h"; \
		NIX_HARDENING_ENABLE="" go generate ./"$$d"... ; \
		rm -f "$$d/vmlinux.h"; \
	done

clean:
	rm -rf $(BIN_DIR) $(COVER_OUT_PREFIX) $(COVER_OUT_PREFIX).*
	@# Legacy: earlier builds put binaries at the repo root. Clean
	@# those too in case someone is upgrading from an older Makefile.
	rm -f daft daft-agent daft-sandboxd
	@# Legacy: pre-2026-05-13 `make integration-build` planted
	@# result-* symlinks at the repo root. We no longer create them
	@# (the dev shell pins the same store paths), but keep the rm so
	@# upgrading developers get a clean tree without an extra step.
	rm -f result result-ch result-kernel result-virtiofsd result-initramfs result-bundle
