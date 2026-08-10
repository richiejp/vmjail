#!/usr/bin/env bash
#
# cover-check.sh — verifies code coverage against three gates:
#
#   1. Total coverage >= committed baseline (coverage.txt), unless
#      TOTAL_COVERAGE_MIN supplies an explicit verification floor.
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

profile_in="$1"
baseline="$2"

# Filter generated code out of the profile. Auto-generated *.pb.go /
# *.connect.go (via the proto/buf pipeline) and bpf2go's *_bpfel.go /
# *_bpfeb.go eBPF loaders (Tier-2 gadgets, see OBSERVABILITY.md) are not
# authored by us and would otherwise drag both the total and per-file
# gates with 0%-covered code we don't intend to test. Match by path
# component / filename suffix so the rule keeps applying as each gen tree
# grows. (The hand-written gadget wiring — opensnoop.go etc. — is NOT
# excluded; its load path is unit-tested via an injected-deps seam.)
#
# The default exclusion list covers generated code under internal/genpb/
# and the bpf2go loaders. Override via SKIP_PATHS="pat1|pat2|..." (POSIX
# ERE; matched against the full file path with `grep -E`).
SKIP_PATHS="${SKIP_PATHS:-internal/genpb/|_bpf(el|eb)\.go}"
profile=$(mktemp)
trap 'rm -f "$profile"' EXIT
grep -Ev "$SKIP_PATHS" "$profile_in" > "$profile" || true

# Per-file minimum coverage as a percentage. Every file in the
# profile is gated — there is no statements-count exemption. Even
# a 1-statement file in package main has to be covered (e.g. via a
# main_test.go that drives main() through a swappable os.Exit).
PER_FILE_MIN="${PER_FILE_MIN:-80.0}"

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
# There is no escape hatch — even when a deletion lowers the
# percentage, the answer is to add coverage elsewhere in the same
# change so the baseline holds or rises. See CONTRIBUTING.md
# "Verification monotonically increases."
if [ "$mode" = update ]; then
	if [ -f "$baseline" ]; then
		old=$(tr -d '[:space:]' < "$baseline")
		if awk -v c="$current" -v o="$old" 'BEGIN { exit (c < o ? 0 : 1) }'; then
			echo "REFUSE: current $current% < existing baseline $old%."
			echo "Coverage only ever rises in this project. Add tests in"
			echo "the same change to bring the total back to or above $old%."
			echo "Do NOT lower $baseline by hand."
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
required_pct="${TOTAL_COVERAGE_MIN:-$baseline_pct}"
required_label=baseline
[ -z "${TOTAL_COVERAGE_MIN:-}" ] || required_label="explicit floor"

echo "Total coverage:    $current%"
echo "Baseline coverage: $baseline_pct%"
[ -z "${TOTAL_COVERAGE_MIN:-}" ] || echo "Required coverage: $required_pct% (explicit floor)"

total_status=0
awk -v c="$current" -v b="$required_pct" -v label="$required_label" 'BEGIN {
	if (c + 0 < b + 0) {
		printf "FAIL: total coverage %.2f%% < %s %.2f%%. Add tests.\n", c, label, b
		exit 1
	}
	delta = c - b
	printf "OK:   total %.2f%% (%s %.2f%%, +%.2fpp)\n", c, label, b, delta
}' || total_status=1

# Per-file gate. Aggregate statements per file, then compute pct.
# Output sorted ascending so the worst offenders show first.
fail=$total_status
while IFS= read -r line; do
	echo "$line"
	[ "${line#FAIL}" != "$line" ] && fail=1
done < <(awk -v min="$PER_FILE_MIN" '
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
			if (pct < min) {
				printf "FAIL  %5.1f%% (%3d stmts)  %s\n", pct, t, f
			} else {
				printf "ok    %5.1f%% (%3d stmts)  %s\n", pct, t, f
			}
		}
	}
' "$profile" | sort)

if [ "$fail" -eq 1 ]; then
	[ "$total_status" -eq 0 ] || echo "FAIL: total coverage is below the required minimum"
	[ "$total_status" -ne 0 ] || echo "FAIL: one or more files below per-file minimum ($PER_FILE_MIN%)"
	exit 1
fi
echo "OK:   every gated file at or above ${PER_FILE_MIN}%"
