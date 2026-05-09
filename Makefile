.PHONY: all fmt check-fmt lint test cover cover-check hooks build build-daft build-daft-agent build-daft-sandboxd clean help

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
	@echo "  cover-check - fail if coverage drops > 1pp vs coverage.txt"
	@echo "  hooks       - install git pre-commit hook into .git/hooks"
	@echo "  build       - build daft, daft-agent, daft-sandboxd (statically)"

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

# cover-check: pass if coverage hasn't dropped > 1.0 percentage point vs baseline.
# - If coverage.txt is missing, this is a no-op (first run).
# - If the drop is > 1.0pp, exit non-zero.
cover-check:
	@go test -covermode=atomic -coverprofile=$(COVER_OUT) $(OUR_GO_PKGS) > /dev/null
	@current=$$(go tool cover -func=$(COVER_OUT) | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "Current total coverage: $$current%"; \
	if [ ! -f $(COVER_BASELINE) ]; then \
		echo "No $(COVER_BASELINE) baseline yet; run 'make cover' to create one."; \
		exit 0; \
	fi; \
	baseline=$$(cat $(COVER_BASELINE) | tr -d '[:space:]'); \
	echo "Baseline coverage:        $$baseline%"; \
	awk -v c="$$current" -v b="$$baseline" 'BEGIN { \
		drop = b - c; \
		if (drop > 1.0) { \
			printf "FAIL: coverage dropped by %.2fpp (limit 1.00pp)\n", drop; \
			exit 1; \
		} \
		printf "OK: coverage delta %.2fpp (baseline %.2f -> current %.2f)\n", -drop, b, c; \
	}'

## Git hooks -----------------------------------------------------------------

hooks:
	@mkdir -p .git/hooks
	@cp scripts/hooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed .git/hooks/pre-commit"

## Building ------------------------------------------------------------------

# build: produce all shipped binaries, statically linked (CGO=0).
#   - daft:           host-side client CLI
#   - daft-agent:     in-VM PID-1 agent
#   - daft-sandboxd:  sandbox-host single executable. Note: a `make
#     build` produces a daft-sandboxd with STUB embedded artifacts —
#     the real cloud-hypervisor / kernel / initramfs are pulled in
#     only by the Nix build (`nix build .#daft-sandboxd`). Use Nix
#     for any release artefact.
build: build-daft build-daft-agent build-daft-sandboxd

build-daft:
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o daft ./cmd/daft

build-daft-agent:
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o daft-agent ./cmd/daft-agent

build-daft-sandboxd:
	CGO_ENABLED=0 go build -ldflags '-s -w $(LDFLAGS)' -o daft-sandboxd ./cmd/daft-sandboxd

clean:
	rm -f daft daft-agent daft-sandboxd $(COVER_OUT)
