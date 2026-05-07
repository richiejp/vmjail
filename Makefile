.PHONY: all fmt check-fmt lint test cover cover-check hooks build clean help

# Single source of truth for the version
VERSION := $(shell cat version.txt)
LDFLAGS := -X github.com/richiejp/darkaifactory/internal/version.Version=$(VERSION)

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
	@echo "  build       - build daft into ./daft"

## Formatting ----------------------------------------------------------------

fmt:
	gofumpt -w .

check-fmt:
	@unformatted=$$(gofumpt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "All files formatted correctly"

## Linting -------------------------------------------------------------------

lint:
	golangci-lint run

## Testing ------------------------------------------------------------------

test:
	go test ./...

## Coverage ------------------------------------------------------------------

# Compute total coverage as a percentage with one decimal point.
# Writes coverage.out (transient) and prints the number to stdout.
COVER_OUT := coverage.out
COVER_BASELINE := coverage.txt

cover:
	@go test -covermode=atomic -coverprofile=$(COVER_OUT) ./... > /dev/null
	@total=$$(go tool cover -func=$(COVER_OUT) | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "Total coverage: $$total%"; \
	echo "$$total" > $(COVER_BASELINE); \
	echo "Updated baseline $(COVER_BASELINE)"

# cover-check: pass if coverage hasn't dropped > 1.0 percentage point vs baseline.
# - If coverage.txt is missing, this is a no-op (first run).
# - If the drop is > 1.0pp, exit non-zero.
cover-check:
	@go test -covermode=atomic -coverprofile=$(COVER_OUT) ./... > /dev/null
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

build:
	go build -ldflags '$(LDFLAGS)' -o daft ./cmd/daft

clean:
	rm -f daft $(COVER_OUT)
