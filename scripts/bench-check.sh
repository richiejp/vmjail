#!/usr/bin/env bash
#
# Compare current Go benchmarks against the bench.txt baseline, or
# refresh the baseline.
#
# Reads `go test -bench` output on stdin and extracts ns/op per
# benchmark, stripping the GOMAXPROCS -N suffix so results are
# host-portable.
#
# Modes:
#   bench-check.sh <baseline>            — compare stdin to baseline
#   bench-check.sh --update <baseline>   — overwrite baseline from stdin
#
# Compare-mode verdicts (per benchmark):
#   ratio > 2.00x -> FAIL  (caught: an order-of-magnitude regression)
#   ratio > 1.10x -> WARN  (printed; does not fail the build — perf is noisy)
#   else          -> OK
# Benchmarks not in the baseline are reported as NEW. Benchmarks in the
# baseline but absent from this run (e.g. e2e bench skipped without
# /dev/kvm) are ignored.
#
# Baseline format: one benchmark per line, "<name> <ns/op>". Lines
# starting with `#` are comments and ignored.

set -euo pipefail

usage() {
	echo "usage: $0 [--update] <baseline-file>" >&2
	exit 2
}

mode=compare
case "${1:-}" in
	--update) mode=update; shift ;;
	-h|--help) usage ;;
	"") usage ;;
esac

[ $# -eq 1 ] || usage
baseline="$1"

# Extract `Benchmark<Name>-<GOMAXPROCS>  <iters>  <ns/op>  ...` into
# `<Name> <ns/op>`. The single source of truth for `go test -bench`
# output shape — both update and compare modes feed stdin through this.
extract_awk='/^Benchmark/ {
	name = $1
	sub(/-[0-9]+$/, "", name)
	print name, $3
}'

if [ "$mode" = update ]; then
	# Merge semantics: entries seen in this run overwrite the baseline;
	# entries in the baseline but absent from this run are preserved.
	# This matters for the e2e BenchmarkHelloBoot — running `make bench`
	# on a machine without /dev/kvm must not silently drop it from the
	# committed baseline.
	current="$(mktemp)"
	trap 'rm -f "$current"' EXIT
	awk "$extract_awk" > "$current"

	{
		echo "# bench.txt v1 — ns/op baseline; updated by 'make bench'."
		awk '
			NR == FNR { merged[$1] = $2; next }
			/^#/ || NF == 0 { next }
			!($1 in merged) { merged[$1] = $2 }
			END { for (name in merged) print name, merged[name] }
		' "$current" "$baseline" 2>/dev/null | sort
	} > "$baseline.tmp"
	mv "$baseline.tmp" "$baseline"
	echo "Updated baseline $baseline"
	exit 0
fi

if [ ! -f "$baseline" ]; then
	echo "No $baseline baseline yet; run 'make bench' to create one."
	cat > /dev/null  # drain stdin so producer doesn't see SIGPIPE
	exit 0
fi

# Compare in a single awk pass: baseline as the first file (NR == FNR),
# the extractor's output as the second via process substitution.
awk -v fail_thresh=2.0 -v warn_thresh=1.1 '
	NR == FNR {
		if ($0 ~ /^#/ || $0 ~ /^[[:space:]]*$/) next
		base[$1] = $2
		next
	}
	{
		name = $1; cur = $2 + 0
		if (!(name in base)) {
			printf "[NEW]   %-40s %s ns/op (no baseline)\n", name, cur
			next
		}
		b = base[name] + 0
		if (b == 0) next
		ratio = cur / b
		if (ratio > fail_thresh) {
			printf "[FAIL]  %-40s %s -> %s ns/op (%.2fx, > %.2fx)\n", name, base[name], cur, ratio, fail_thresh
			failed = 1
		} else if (ratio > warn_thresh) {
			printf "[WARN]  %-40s %s -> %s ns/op (%.2fx, > %.2fx)\n", name, base[name], cur, ratio, warn_thresh
		} else {
			printf "[OK]    %-40s %s -> %s ns/op (%.2fx)\n", name, base[name], cur, ratio
		}
	}
	END { exit failed ? 1 : 0 }
' "$baseline" <(awk "$extract_awk")
