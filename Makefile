.PHONY: all check check-submodule-revs check-deploy-skill check-agent-tools check-architecture-docs verify fmt check-fmt lint lint-golangci lint-daft test cover cover-check bench bench-check fuzz hooks build build-daft-agent build-daft-hello-container build-daft-sandboxd build-artefacts build-initramfs build-hello-bundle dev integration integration-build proto gen-bpf clean help

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
check: check-fmt check-submodule-revs check-deploy-skill check-agent-tools check-architecture-docs lint test cover-check

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

# Agent-facing helpers are part of the development surface. Keep their shell
# syntax valid without requiring gopls to be installed in release builders.
check-agent-tools:
	@sh -n scripts/go-nav.sh
	@scripts/test-go-nav.sh

# Architecture pages are an agent-facing navigation and correctness surface.
# Keep every component paired with a bounded lifecycle view and reject link or
# hierarchy drift before it becomes the next agent's false system model.
check-architecture-docs:
	@scripts/check-architecture-docs.sh

# verify is the full gate set used by the pre-commit hook. `dev`
# builds every real artefact (Go binaries + VM toolchain + initramfs +
# bundle) so a broken full build fails the commit, not just CI. Active
# fuzzing runs with a deterministic 1000-execution/target budget. A duration
# budget can expire while Go is coordinating workers and intermittently return
# `context deadline exceeded`, despite a healthy target; an execution budget
# keeps the commit gate both fast and reproducible. Manual / nightly fuzzing uses
# `make fuzz` (default 30s/target).
#
# verify does NOT run `nix flake check` — that builds our Go binaries
# and initramfs/bundle via their Nix derivations (a path parallel to
# the `go build`/Make recipes `dev` uses), which is the slow cold
# build. That gate lives in the pre-push hook + CI instead.
verify: check dev integration bench-check
	@FUZZ_TIME=1000x scripts/fuzz.sh

help:
	@echo "Targets:"
	@echo "  all         - build the project + run the fast check gates (default)"
	@echo "  check       - fmt + submodule-revs + agent/docs checks + lint + unit tests + coverage gate"
	@echo "  verify      - check + integration + bench + deterministic fuzz sweep (pre-commit gate)"
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
NIX_XFS_TOOLS_OUT := $(BIN_DIR)/guest-xfs-tools
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
	@nix build .#guest-xfs-tools  -o $(NIX_XFS_TOOLS_OUT)

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
		-files "$(CURDIR)/$(NIX_XFS_TOOLS_OUT)/bin:bin" \
		-files "$(CURDIR)/$(NIX_XFS_TOOLS_OUT)/lib:lib" \
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

# The retained tests need only the local VM artifacts and hello bundle.
integration-build: dev

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
			echo "[e2e] cgroup delegation on — sandbox tests get per-VM accounting"; \
		else \
			echo "[e2e] cgroup delegation off — no --user scope: the sandbox in-process host needs one, so e2e will fail (enable-linger / run under a Delegate=yes unit)"; \
		fi; \
		export DAFT_INITRAMFS=$(CURDIR)/$(INITRAMFS_OUT); \
			export DAFT_BUNDLE=$(CURDIR)/$(BUNDLE_OUT); \
			export DAFT_KERNEL=$(CURDIR)/$(NIX_KERNEL_OUT)/bzImage; \
			export DAFT_CLOUD_HYPERVISOR=$(CURDIR)/$(NIX_CHV_OUT)/bin/cloud-hypervisor; \
			export DAFT_VIRTIOFSD=$(CURDIR)/$(NIX_VIRTIOFSD_OUT)/bin/virtiofsd; \
			runflag=""; [ -n "$$INTEGRATION_RUN" ] && runflag="-run $$INTEGRATION_RUN"; \
			rc=0; \
			echo "[e2e] sandbox + sealed assets — in-process host shares the delegated scope: per-VM accounting, limits off"; \
			sandbox_log=$$(mktemp /tmp/daft-integration-sandbox.XXXXXX.log); \
			if $$delegate go test -v -tags=integration -count=1 -timeout 20m $$runflag -p 1 ./internal/sandbox/ ./internal/bundled/ 9>&- >$$sandbox_log 2>&1; then \
				tail -2 $$sandbox_log; rm -f $$sandbox_log; \
			else \
				echo "[e2e] sandbox failure; full log retained at $$sandbox_log"; \
				grep -n -E -B 12 -A 80 -- '--- FAIL:|panic:|fatal error|^FAIL([[:space:]]|$$)' $$sandbox_log || tail -200 $$sandbox_log; \
				rc=1; \
			fi; \
			exit $$rc; \
	)

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

build: build-daft-agent build-daft-sandboxd

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
