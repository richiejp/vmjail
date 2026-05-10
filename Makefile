.PHONY: all fmt check-fmt lint test cover cover-check bench bench-check hooks build build-daft build-daft-agent build-daft-sandboxd clean help

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

all: check-fmt lint test cover-check

help:
	@echo "Targets:"
	@echo "  fmt         - format Go code with gofumpt"
	@echo "  check-fmt   - fail if anything is unformatted"
	@echo "  lint        - run golangci-lint"
	@echo "  test        - go test ./..."
	@echo "  cover       - update committed coverage.txt baseline"
	@echo "  cover-check - fail if total coverage drops >1pp vs coverage.txt OR any file is <80%"
	@echo "  bench       - update committed bench.txt baseline"
	@echo "  bench-check - fail on >2x regressions, warn on >1.1x, vs bench.txt"
	@echo "  hooks       - install git pre-commit hook into .git/hooks"
	@echo "  build       - build daft, daft-agent, daft-sandboxd into ./bin/ (dev only)"

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

lint:
	golangci-lint run $(OUR_GO_PKGS)

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
	@total=$$(go tool cover -func=$(COVER_OUT) | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "Total coverage: $$total%"; \
	echo "$$total" > $(COVER_BASELINE); \
	echo "Updated baseline $(COVER_BASELINE)"

# cover-check: two gates, both must pass.
#   1. Total coverage stays within 1.0pp of the committed baseline
#      (coverage.txt). Catches "we drifted lower" overall.
#   2. Every individual non-test source file (with > 10 statements)
#      is at or above 80% coverage. Catches "one file is dark while
#      the rest of the codebase masks it." Threshold can be tuned via
#      $$PER_FILE_MIN.
# scripts/cover-check.sh owns the comparison logic.
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
BIN_DIR := bin

build: build-daft build-daft-agent build-daft-sandboxd

build-daft:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft ./cmd/daft

build-daft-agent:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-agent ./cmd/daft-agent

build-daft-sandboxd:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o $(BIN_DIR)/daft-sandboxd ./cmd/daft-sandboxd

clean:
	rm -rf $(BIN_DIR) $(COVER_OUT)
	@# Legacy: earlier builds put binaries at the repo root. Clean
	@# those too in case someone is upgrading from an older Makefile.
	rm -f daft daft-agent daft-sandboxd
