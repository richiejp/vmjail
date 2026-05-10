#!/usr/bin/env bash
#
# cover-check.sh — verifies code coverage against three gates:
#
#   1. Total coverage >= committed baseline (coverage.txt). No drop
#      tolerance — see CONTRIBUTING.md "Verification monotonically
#      increases."
#   2. Every individual non-test source file has at least
#      $PER_FILE_MIN coverage (default 80%). Catches the case where
#      total coverage stays high while one file is dark.
#   3. (--update mode only) refuses to write a lower baseline than
#      the current one. Combined with gate 1 this makes the
#      baseline a one-way ratchet: it goes up over time, never down.
#
# Reads a `go test -coverprofile` file. Skips the "mode:" header.
# Owns the comparison logic so the Makefile target stays a one-liner.
#
# Modes:
#   cover-check.sh <profile> <baseline>            — verify
#   cover-check.sh --update <profile> <baseline>   — write new
#                                                    baseline (refuses
#                                                    to lower it)
#
# Exits 0 on pass, 1 on fail.

set -euo pipefail

mode=verify
case "${1:-}" in
	--update) mode=update; shift ;;
	-h|--help) echo "usage: $0 [--update] <profile> <baseline>"; exit 2 ;;
esac

if [ $# -ne 2 ]; then
	echo "usage: $0 [--update] <profile> <baseline>" >&2
	exit 2
fi

profile="$1"
baseline="$2"

# Per-file minimum coverage as a percentage.
PER_FILE_MIN="${PER_FILE_MIN:-80.0}"

# How many statements a file must have before it's gated. Tiny files
# (a few statements of glue) are too easy to swing across the gate
# from one missed line; we only enforce on files large enough that
# 80% is a meaningful number.
PER_FILE_MIN_STMTS="${PER_FILE_MIN_STMTS:-10}"

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
_=$total_stmts

# --update mode: write the new baseline, but ratchet upward only.
# Editing coverage.txt by hand is the documented escape hatch when a
# real reduction (e.g. deletion of heavily-tested code) is intended.
if [ "$mode" = update ]; then
	if [ -f "$baseline" ]; then
		old=$(tr -d '[:space:]' < "$baseline")
		if awk -v c="$current" -v o="$old" 'BEGIN { exit (c < o ? 0 : 1) }'; then
			echo "REFUSE: current $current% < existing baseline $old%."
			echo "If this is intentional (you deleted heavily-tested code),"
			echo "edit $baseline by hand and explain the drop in the commit message."
			exit 1
		fi
	fi
	printf "%s\n" "$current" > "$baseline"
	echo "Updated baseline $baseline -> $current%"
	exit 0
fi

if [ ! -f "$baseline" ]; then
	echo "No $baseline baseline; run 'make cover' to create one."
	exit 0
fi

baseline_pct=$(tr -d '[:space:]' < "$baseline")

echo "Total coverage:    $current%"
echo "Baseline coverage: $baseline_pct%"

total_status=0
awk -v c="$current" -v b="$baseline_pct" 'BEGIN {
	if (c + 0 < b + 0) {
		printf "FAIL: total coverage %.2f%% < baseline %.2f%%. Add tests, do not lower the baseline.\n", c, b
		exit 1
	}
	delta = c - b
	printf "OK:   total %.2f%% (baseline %.2f%%, +%.2fpp)\n", c, b, delta
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
