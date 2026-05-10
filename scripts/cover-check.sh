#!/usr/bin/env bash
#
# cover-check.sh — verifies code coverage against two gates:
#
#   1. Total coverage is within 1.0pp of the committed baseline
#      (coverage.txt at repo root).
#   2. Every individual non-test source file has at least
#      $PER_FILE_MIN coverage (default 80%). Catches the case where
#      total coverage stays high while one file is dark, which can
#      happen as the codebase grows.
#
# Reads a `go test -coverprofile` file. Skips the "mode:" header.
# Owns the comparison logic so the Makefile target stays a one-liner.
#
# Usage:
#   scripts/cover-check.sh <coverprofile> <baseline-file>
#
# Exits 0 on pass, 1 on fail.

set -euo pipefail

if [ $# -ne 2 ]; then
	echo "usage: $0 <coverprofile> <baseline-file>" >&2
	exit 2
fi

profile="$1"
baseline="$2"

# Per-file minimum coverage as a percentage. Override via env when
# tightening (or temporarily relaxing) the gate during refactors.
PER_FILE_MIN="${PER_FILE_MIN:-80.0}"

# How many statements a file must have before it's gated. Tiny files
# (a few statements of glue) are too easy to swing across the gate
# from one missed line; we only enforce on files large enough that
# 80% is a meaningful number.
PER_FILE_MIN_STMTS="${PER_FILE_MIN_STMTS:-10}"

# Total-coverage drop tolerance, in percentage points. Mirrors the
# old in-Makefile gate.
TOTAL_DROP_LIMIT="${TOTAL_DROP_LIMIT:-1.0}"

if [ ! -f "$baseline" ]; then
	echo "No $baseline baseline; run 'make cover' to create one."
	exit 0
fi

# Compute total coverage: sum statements weighted by hit, divide by
# total statements. Skips the "mode:" header line.
read -r current total_stmts < <(awk '
	NR == 1 { next }
	{
		s = $2; c = $3
		total += s
		if (c > 0) covered += s
	}
	END { printf "%.2f %d\n", (total > 0 ? 100 * covered / total : 0), total }
' "$profile")

baseline_pct=$(tr -d '[:space:]' < "$baseline")

echo "Total coverage:    $current%"
echo "Baseline coverage: $baseline_pct%"

total_status=0
awk -v c="$current" -v b="$baseline_pct" -v limit="$TOTAL_DROP_LIMIT" 'BEGIN {
	drop = b - c
	if (drop > limit) {
		printf "FAIL: total coverage dropped %.2fpp (limit %.2fpp)\n", drop, limit
		exit 1
	}
	printf "OK:   total delta %.2fpp (baseline %.2f -> current %.2f)\n", -drop, b, c
}' || total_status=1

# Per-file gate. Aggregate statements per file, then compute pct.
# Output sorted ascending so the worst offenders show first.
fail=$total_status
while IFS= read -r line; do
	echo "$line"
	[ "${line#FAIL}" != "$line" ] && fail=1
done < <(awk -v min="$PER_FILE_MIN" -v minstmts="$PER_FILE_MIN_STMTS" '
	NR == 1 { next }
	{
		# field 1: file:start.col,end.col
		split($1, a, ":")
		file = a[1]
		s = $2; c = $3
		total[file] += s
		if (c > 0) covered[file] += s
	}
	END {
		# Build a list keyed on pct so we can sort.
		for (f in total) {
			t = total[f]
			cv = covered[f]
			pct = (t > 0 ? 100.0 * cv / t : 100.0)
			if (t < minstmts) {
				printf "SKIP  %5.1f%% (%3d stmts)  %s\n", pct, t, f
				continue
			}
			if (pct < min) {
				printf "FAIL  %5.1f%% (%3d stmts)  %s\n", pct, t, f
			} else {
				printf "ok    %5.1f%% (%3d stmts)  %s\n", pct, t, f
			}
		}
	}
' "$profile" | sort)

if [ "$fail" -eq 1 ]; then
	echo "FAIL: one or more files below per-file minimum ($PER_FILE_MIN%)"
	exit 1
fi
echo "OK:   every gated file at or above ${PER_FILE_MIN}%"
