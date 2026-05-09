#!/usr/bin/env bash
#
# Compare current Go benchmarks against bench.txt baseline.
#
# Reads `go test -bench` output on stdin, extracts ns/op per benchmark
# (stripping the GOMAXPROCS -N suffix so results are host-portable), and
# compares against the committed baseline.
#
# Per benchmark in baseline:
#   ratio > 2.00x -> FAIL  (caught: an order-of-magnitude regression)
#   ratio > 1.10x -> WARN  (printed; does not fail the build — perf is noisy)
#   else          -> OK
# Benchmarks not in the baseline are reported as NEW. Benchmarks in the
# baseline but absent from this run (e.g. e2e bench skipped without
# /dev/kvm) are ignored.
#
# Usage:
#   go test -bench=. -run='^$' -benchtime=3x ./... | scripts/bench-check.sh bench.txt

set -euo pipefail

if [ $# -ne 1 ]; then
	echo "usage: $0 <baseline-file>" >&2
	exit 2
fi

baseline="$1"

if [ ! -f "$baseline" ]; then
	echo "No $baseline baseline yet; run 'make bench' to create one."
	# Drain stdin so the producer doesn't get SIGPIPE.
	cat > /dev/null
	exit 0
fi

current="$(mktemp)"
trap 'rm -f "$current"' EXIT

# Parse stdin: lines like "BenchmarkName-20  3  326935767 ns/op  ..."
# yield "BenchmarkName 326935767".
awk '/^Benchmark/ {
	name = $1
	sub(/-[0-9]+$/, "", name)
	print name, $3
}' | sort > "$current"

awk -v fail_thresh=2.0 -v warn_thresh=1.1 '
	FNR == NR {
		if ($0 ~ /^[#[:space:]]*$/ || $0 ~ /^#/) next
		base[$1] = $2
		next
	}
	$0 ~ /^[#[:space:]]*$/ { next }
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
' "$baseline" "$current"
