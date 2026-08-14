#!/usr/bin/env bash
# Run the exploratory product tester without Codex filesystem/network sandboxing.
# darkaifactory remains the isolation boundary for untrusted workloads.
set -euo pipefail

if [[ "${DAFT_BLACKBOX_UNRESTRICTED:-}" != "1" ]]; then
	echo "blackbox-agent: set DAFT_BLACKBOX_UNRESTRICTED=1 to acknowledge unrestricted host CLI/network access" >&2
	exit 2
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
codex_bin=${CODEX_BIN:-codex}
model=${DAFT_BLACKBOX_MODEL:-gpt-5.6-terra}
campaign_seconds=${DAFT_BLACKBOX_TIMEOUT_SECONDS:-7200}
max_issues=${DAFT_BLACKBOX_MAX_ISSUES:-10}
config_dir=${DAFT_CONFIG_DIR:-"${HOME}/.config/daft/client"}
output_root=${DAFT_BLACKBOX_OUTPUT:-}

[[ "$campaign_seconds" =~ ^[1-9][0-9]*$ ]] || { echo "blackbox-agent: timeout must be positive seconds" >&2; exit 2; }
[[ "$max_issues" =~ ^[1-9][0-9]*$ ]] || { echo "blackbox-agent: max issues must be positive" >&2; exit 2; }
command -v "$codex_bin" >/dev/null || { echo "blackbox-agent: Codex executable not found: $codex_bin" >&2; exit 2; }
[[ -x "$repo_root/bin/daft" ]] || { echo "blackbox-agent: build bin/daft first" >&2; exit 2; }
[[ -d "$config_dir" ]] || { echo "blackbox-agent: client config not found: $config_dir" >&2; exit 2; }

if [[ -z "$output_root" ]]; then
	output_root=$(mktemp -d "${TMPDIR:-/tmp}/daft-blackbox-terra.XXXXXX")
else
	mkdir -p "$output_root"
fi
public_root="$output_root/public"
mkdir -p "$public_root/bin" "$public_root/docs" "$output_root/results"
cp "$repo_root/bin/daft" "$public_root/bin/daft"
cp -R "$repo_root/blackbox" "$public_root/blackbox"
cp "$repo_root/README" "$repo_root/OBSERVABILITY.md" "$public_root/docs/"

prompt_file="$output_root/prompt.md"
cat >"$prompt_file" <<EOF
You are a source-blind product tester for darkaifactory. Test all 20 numbered
goals in blackbox/GOALS.md, in order, using only the installed public CLI,
public docs, fixtures, and the live cluster. Do not inspect the darkaifactory
source checkout, Git history, implementation logs, Codex session files, or the
contents of the client identity directory. Invoke the client as:

  ./bin/daft ... --config-dir "$config_dir"

You have unrestricted host networking and shell access because Codex itself is
not the security boundary. Run untrusted project commands only inside
darkaifactory sandboxes. Do not weaken TLS, expose credentials, stop host
services, or mutate unrelated host files. Browser-only goals must use an actual
available browser interface; if no browser exists, record that prerequisite as
blocked rather than substituting curl.

Campaign bounds: ${campaign_seconds}s total, all 20 goals attempted, at most
${max_issues} confirmed issues. A confirmed issue requires two clean
reproductions and category correctness, UX, or performance. Keep command output
bounded. Write results only below ../results: goal-results.jsonl (exactly one
record per goal with goal, status, actions, evidence, and summary), scenarios,
runs, issues.jsonl, and final-report.md. Preserve failed evidence. Validate any
scenario and issue files with the public daft blackbox commands. Clean up only
workspaces you created, and do not discard unadopted changes unless the goal
explicitly tests rejection/deletion.
EOF

echo "blackbox-agent: output=$output_root model=$model timeout=${campaign_seconds}s" >&2
cd "$public_root"
timeout --signal=TERM --kill-after=30s "${campaign_seconds}s" \
	"$codex_bin" exec --ephemeral --skip-git-repo-check --ignore-user-config \
	--model "$model" --sandbox danger-full-access -c 'approval_policy="never"' \
	--output-last-message "$output_root/results/codex-final.txt" \
	- <"$prompt_file" 2>&1 | tee "$output_root/results/codex-events.log"

echo "$output_root"
