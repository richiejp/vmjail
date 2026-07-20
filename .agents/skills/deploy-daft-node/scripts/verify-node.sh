#!/bin/sh
set -eu

[ "$#" -eq 3 ] || { echo "usage: $0 SANDBOXD_PATH ORCHESTRATOR_URL VERSION" >&2; exit 2; }
sandboxd=$1 orchestrator=$2 expected_version=$3
[ -x "$sandboxd" ] || { echo "daft-sandboxd executable missing: $sandboxd" >&2; exit 1; }
actual_version=$("$sandboxd" version)
[ "$actual_version" = "$expected_version" ] || { echo "version mismatch: $actual_version" >&2; exit 1; }
case "$orchestrator" in https://*) ;; *) echo "orchestrator URL must use https" >&2; exit 1;; esac
printf 'local_binary=ok\nversion=%s\norchestrator=%s\n' "$actual_version" "$orchestrator"
echo 'registration=not-checked'
