#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/config" "$tmp/fakebin" "$tmp/clusterbin"
cat >"$tmp/fakebin/codex" <<'EOF'
#!/bin/sh
printf '%s\n' "$*"
cat >/dev/null
EOF
cat >"$tmp/clusterbin/daft" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$DAFT_FAKE_LOG"
if [ "$1 $2" = "cluster up" ]; then
	while [ "$#" -gt 0 ]; do
		if [ "$1" = "--state" ]; then shift; state=$1; fi
		shift
	done
	mkdir -p "$state/client" "$state/logs"
	printf '%s\n%s\n' "$$" "$$" >"$state/cluster.pid"
fi
if [ "$1 $2 $3" = "dashboard password-hash" ]; then
	cat >/dev/null
	printf '%s\n' '$2a$10$rEWm4r7F8qv0I5J8dQE7OeVfDQp1u0wEH1fZUwNNQMIfIQK0jZB2G'
fi
if [ "$1 $2" = "image push" ]; then
	printf '%s\n' '127.0.0.1:24443/blackbox-toolbox:v1'
fi
if [ "$1 $2" = "cluster down" ]; then
	while [ "$#" -gt 0 ]; do
		if [ "$1" = "--state" ]; then shift; state=$1; fi
		if [ "$1" = "--purge" ]; then purge=1; fi
		shift
	done
	[ "${purge:-}" = 1 ] && rm -rf "$state"
fi
EOF
cp "$tmp/clusterbin/daft" "$tmp/clusterbin/daft-orchestrator"
cp "$tmp/clusterbin/daft" "$tmp/clusterbin/daft-sandboxd"
chmod +x "$tmp/fakebin/codex" "$tmp/clusterbin/"*
common_env=(
	CODEX_BIN="$tmp/fakebin/codex"
	DAFT_BLACKBOX_BIN_DIR="$tmp/clusterbin"
	DAFT_BLACKBOX_SKIP_BUILD=1
	DAFT_FAKE_LOG="$tmp/daft.log"
)
if env "${common_env[@]}" "$repo_root/scripts/blackbox-agent.sh" >/dev/null 2>&1; then
	echo "blackbox-agent accepted missing opt-in" >&2; exit 1
fi
if env "${common_env[@]}" DAFT_BLACKBOX_UNRESTRICTED=1 DAFT_BLACKBOX_LIVE=1 \
	"$repo_root/scripts/blackbox-agent.sh" >/dev/null 2>&1; then
	echo "blackbox-agent accepted live mode without explicit client config" >&2; exit 1
fi
if env "${common_env[@]}" DAFT_BLACKBOX_UNRESTRICTED=1 DAFT_BLACKBOX_LIVE=1 \
	DAFT_CONFIG_DIR="$tmp/config" "$repo_root/scripts/blackbox-agent.sh" >/dev/null 2>&1; then
	echo "blackbox-agent accepted live mode without explicit orchestrator" >&2; exit 1
fi
out="$tmp/out"
touch "$tmp/toolbox.tar"
env "${common_env[@]}" DAFT_BLACKBOX_UNRESTRICTED=1 DAFT_BLACKBOX_OUTPUT="$out" \
	DAFT_BLACKBOX_TOOLBOX_IMAGE="$tmp/toolbox.tar" \
	DAFT_BLACKBOX_TIMEOUT_SECONDS=5 "$repo_root/scripts/blackbox-agent.sh" >/dev/null
test -x "$out/public/bin/daft"
test -d "$out/results/runs"
test -d "$out/results/scenarios"
test -d "$out/results/cluster-logs"
test ! -e "$out/cluster"
test "$(grep -c '^[0-9]\+\.' "$out/public/blackbox/GOALS.md")" -eq 20
grep -q '^cluster up .*--egress-proxy' "$tmp/daft.log"
grep -q '^cluster down .*--purge' "$tmp/daft.log"
grep -q -- '--model gpt-5.6-terra --sandbox danger-full-access' "$out/results/codex-events.log"
grep -q 'all 20 goals attempted' "$out/prompt.md"
grep -q 'headed or headless' "$out/prompt.md"
grep -q 'isolated local cluster built from the current checkout' "$out/prompt.md"
grep -q 'Never invoke cluster up or cluster' "$out/prompt.md"
grep -q 'root-global flag' "$out/prompt.md"
grep -q -- '--orchestrator https://127.0.0.1:24443' "$out/prompt.md"
grep -q 'positional address 127.0.0.1:24443' "$out/prompt.md"
echo 'blackbox-agent script tests: OK'
