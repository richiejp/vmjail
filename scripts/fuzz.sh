#!/usr/bin/env bash
#
# fuzz.sh — run every FuzzXxx target in our tree for FUZZ_TIME each.
#
# Discovers fuzz functions per package via `go test -list='^Fuzz'`
# (Go's tooling refuses to fuzz multiple matching targets in a single
# invocation, so we iterate one at a time).
#
# Usage:
#   FUZZ_TIME=30s scripts/fuzz.sh                 # default duration
#   FUZZ_TIME=1s  scripts/fuzz.sh                 # tiny per-target
#                                                  budget for the
#                                                  pre-commit hook
#
# A failing fuzz run drops a reproducer under
# <pkg>/testdata/fuzz/FuzzFoo/<id> — those files are then run
# automatically by `go test` from then on, so a one-off bug becomes
# a permanent regression test once the file is committed.

set -euo pipefail

FUZZ_TIME="${FUZZ_TIME:-30s}"

# We only fuzz our own code. third_party and generated genpb code
# don't author fuzz targets, but list them defensively.
GO_PKGS=("./internal/..." "./cmd/...")

failed=0
total=0
while IFS= read -r pkg; do
	# go test -list returns one line per matched test; strip the
	# trailing "ok    pkg ..." summary line.
	fuzzes=$(go test -list='^Fuzz' "$pkg" 2>/dev/null \
		| grep -E '^Fuzz' || true)
	for fz in $fuzzes; do
		total=$((total + 1))
		echo "[fuzz] $pkg :: $fz (fuzztime=$FUZZ_TIME)"
		if ! go test "$pkg" -run='^$' -fuzz="^${fz}\$" \
				-fuzztime="$FUZZ_TIME"; then
			failed=$((failed + 1))
		fi
	done
done < <(go list "${GO_PKGS[@]}")

if [ "$total" -eq 0 ]; then
	echo "[fuzz] no fuzz targets found"
	exit 0
fi
if [ "$failed" -gt 0 ]; then
	echo "[fuzz] $failed/$total fuzz runs failed"
	exit 1
fi
echo "[fuzz] $total fuzz runs OK (fuzztime=$FUZZ_TIME each)"
