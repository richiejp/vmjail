#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/config" "$tmp/fakebin"
cat >"$tmp/fakebin/codex" <<'EOF'
#!/bin/sh
printf '%s\n' "$*"
cat >/dev/null
EOF
chmod +x "$tmp/fakebin/codex"
if CODEX_BIN="$tmp/fakebin/codex" DAFT_CONFIG_DIR="$tmp/config" "$repo_root/scripts/blackbox-agent.sh" >/dev/null 2>&1; then
	echo "blackbox-agent accepted missing opt-in" >&2; exit 1
fi
out="$tmp/out"
DAFT_BLACKBOX_UNRESTRICTED=1 CODEX_BIN="$tmp/fakebin/codex" DAFT_CONFIG_DIR="$tmp/config" \
	DAFT_BLACKBOX_OUTPUT="$out" DAFT_BLACKBOX_TIMEOUT_SECONDS=5 "$repo_root/scripts/blackbox-agent.sh" >/dev/null
test -x "$out/public/bin/daft"
test "$(grep -c '^[0-9]\+\.' "$out/public/blackbox/GOALS.md")" -eq 20
grep -q -- '--model gpt-5.6-terra --sandbox danger-full-access' "$out/results/codex-events.log"
grep -q 'all 20 goals attempted' "$out/prompt.md"
echo 'blackbox-agent script tests: OK'
