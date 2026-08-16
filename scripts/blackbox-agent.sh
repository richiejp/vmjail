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
output_root=${DAFT_BLACKBOX_OUTPUT:-}
bin_dir=${DAFT_BLACKBOX_BIN_DIR:-"$repo_root/bin"}
live_cluster=${DAFT_BLACKBOX_LIVE:-0}

[[ "$campaign_seconds" =~ ^[1-9][0-9]*$ ]] || { echo "blackbox-agent: timeout must be positive seconds" >&2; exit 2; }
[[ "$max_issues" =~ ^[1-9][0-9]*$ ]] || { echo "blackbox-agent: max issues must be positive" >&2; exit 2; }
command -v "$codex_bin" >/dev/null || { echo "blackbox-agent: Codex executable not found: $codex_bin" >&2; exit 2; }
[[ "$live_cluster" == "0" || "$live_cluster" == "1" ]] || { echo "blackbox-agent: DAFT_BLACKBOX_LIVE must be 0 or 1" >&2; exit 2; }

if [[ "${DAFT_BLACKBOX_SKIP_BUILD:-}" != "1" ]]; then
	make -C "$repo_root" dev
fi
for binary in daft daft-orchestrator daft-sandboxd; do
	[[ -x "$bin_dir/$binary" ]] || { echo "blackbox-agent: missing executable: $bin_dir/$binary" >&2; exit 2; }
done
export DAFT_CLOUD_HYPERVISOR=${DAFT_CLOUD_HYPERVISOR:-"$bin_dir/cloud-hypervisor/bin/cloud-hypervisor"}
export DAFT_KERNEL=${DAFT_KERNEL:-"$bin_dir/kernel/bzImage"}
export DAFT_INITRAMFS=${DAFT_INITRAMFS:-"$bin_dir/initramfs.cpio.gz"}
export DAFT_VIRTIOFSD=${DAFT_VIRTIOFSD:-"$bin_dir/virtiofsd/bin/virtiofsd"}
export DAFT_BUNDLE=${DAFT_BUNDLE:-"$bin_dir/hello-bundle"}

if [[ -z "$output_root" ]]; then
	output_root=$(mktemp -d "${TMPDIR:-/tmp}/daft-blackbox-terra.XXXXXX")
else
	mkdir -p "$output_root"
fi
public_root="$output_root/public"
mkdir -p "$public_root/bin" "$public_root/docs" "$output_root/results/runs" "$output_root/results/scenarios"
cp "$bin_dir/daft" "$public_root/bin/daft"
cp "$repo_root/scripts/blackbox-control.sh" "$public_root/bin/campaign-control"
cp -R "$repo_root/blackbox" "$public_root/blackbox"
cp "$repo_root/README" "$repo_root/OBSERVABILITY.md" "$public_root/docs/"

cluster_description="an isolated local cluster built from the current checkout"
dashboard_description="the isolated dashboard URL printed below"
if [[ "$live_cluster" == "1" ]]; then
	config_dir=${DAFT_CONFIG_DIR:-}
	[[ -n "$config_dir" ]] || { echo "blackbox-agent: live mode requires an explicit DAFT_CONFIG_DIR" >&2; exit 2; }
	[[ -d "$config_dir" ]] || { echo "blackbox-agent: client config not found: $config_dir" >&2; exit 2; }
	orchestrator_url=${DAFT_BLACKBOX_ORCHESTRATOR:-}
	[[ -n "$orchestrator_url" ]] || { echo "blackbox-agent: live mode requires an explicit DAFT_BLACKBOX_ORCHESTRATOR" >&2; exit 2; }
	ping_endpoint=${orchestrator_url#https://}
	cluster_description="the explicitly requested live cluster"
	dashboard_description=${DAFT_BLACKBOX_DASHBOARD:-"not supplied; record dashboard goals as blocked"}
	ln -s "$config_dir" "$public_root/client-config"
else
	cluster_state="$output_root/cluster"
	cluster_addr=${DAFT_BLACKBOX_CLUSTER_ADDR:-127.0.0.1:24443}
	cluster_web_addr=${DAFT_BLACKBOX_WEB_ADDR:-127.0.0.1:24080}
	dashboard_password=${DAFT_BLACKBOX_DASHBOARD_PASSWORD:-blackbox-password}
	password_file="$cluster_state/dashboard-password"
	mkdir -p "$cluster_state"
	printf '%s\n' "$dashboard_password" | "$bin_dir/daft-orchestrator" dashboard password-hash >"$password_file"
	chmod 0600 "$password_file"
	export DAFT_WORKSPACE_DISK_DEFAULT_BYTES=${DAFT_BLACKBOX_DISK_BYTES:-134217728}
	cleanup_cluster() {
		if [[ -n "${DAFT_BLACKBOX_NODE_PID:-}" ]]; then
			kill -CONT "$DAFT_BLACKBOX_NODE_PID" >/dev/null 2>&1 || true
		fi
		"$bin_dir/daft" cluster down --state "$cluster_state" >/dev/null 2>&1 || true
		if [[ -d "$cluster_state/logs" ]]; then
			cp -R "$cluster_state/logs" "$output_root/results/cluster-logs"
		fi
		"$bin_dir/daft" cluster down --state "$cluster_state" --purge >/dev/null 2>&1 || true
	}
	trap cleanup_cluster EXIT
	"$bin_dir/daft" cluster up --state "$cluster_state" --addr "$cluster_addr" \
		--web-addr "$cluster_web_addr" --web-password-file "$password_file" \
		--web-authn-rp-id localhost --web-authn-origin "http://localhost:${cluster_web_addr##*:}" \
		--egress-proxy \
		--orchestrator-bin "$bin_dir/daft-orchestrator" --sandboxd-bin "$bin_dir/daft-sandboxd"
	ln -s "$cluster_state/client" "$public_root/client-config"
	export DAFT_BLACKBOX_NODE_PID
	DAFT_BLACKBOX_NODE_PID=$(sed -n '2p' "$cluster_state/cluster.pid")
	[[ "$DAFT_BLACKBOX_NODE_PID" =~ ^[1-9][0-9]*$ ]] || { echo "blackbox-agent: missing disposable Node PID" >&2; exit 2; }
	orchestrator_url="https://$cluster_addr"
	ping_endpoint="$cluster_addr"
	dashboard_description="http://localhost:${cluster_web_addr##*:}"
	toolbox_link=${DAFT_BLACKBOX_TOOLBOX_IMAGE:-}
	if [[ -z "$toolbox_link" ]]; then
		toolbox_link="$output_root/blackbox-toolbox-image"
		nix build "$repo_root#blackbox-toolbox-image" --out-link "$toolbox_link"
	fi
	toolbox_ref=$("$bin_dir/daft" image push "$toolbox_link" --repo blackbox-toolbox --tag v1 \
		--orchestrator "$orchestrator_url" --config-dir "$cluster_state/client")
	cat >"$public_root/CAMPAIGN.md" <<EOF
# Disposable campaign resources

- Toolbox image: \`$toolbox_ref\`. It contains \`sh\`, \`bash\`, \`go\`,
  \`bun\`, \`curl\`, and core utilities. Its configured command prints
  \`daft-blackbox-short-command\` and exits; ordinary Workspaces override it
  with their persistent keepalive.
- New Workspace disks default to $DAFT_WORKSPACE_DISK_DEFAULT_BYTES bytes so
  storage freeze and live-growth tests remain bounded.
- Dashboard: $dashboard_description. Password: \`$dashboard_password\`.
  WebAuthn is configured for RP \`localhost\` and can be exercised with a
  browser virtual authenticator after password login and enrollment.
- \`./bin/campaign-control node-pause|node-resume|node-status\` is the only
  permitted host fault-injection interface. Always resume the Node after the
  Node-loss scenario.
- Use a synthetic one-operation value such as \`blackbox-one-shot-secret\`
  for the secret lifecycle test; it grants no external authority.
EOF
fi

prompt_file="$output_root/prompt.md"
cat >"$prompt_file" <<EOF
You are a source-blind product tester for darkaifactory. Test all 20 numbered
goals in blackbox/GOALS.md, in order, using only the installed public CLI,
public docs, fixtures, CAMPAIGN.md when present, and ${cluster_description}. Do not inspect the darkaifactory
source checkout, Git history, implementation logs, Codex session files, or the
contents of the client identity directory. Invoke the client as:

  ./bin/daft <command> <subcommand> ... --config-dir ./client-config

The cluster is already running and ready. Never invoke cluster up or cluster
down, and do not inspect its host processes. In an isolated campaign, the
bounded campaign-control command documented in CAMPAIGN.md is permitted only
for the Node-loss goal. --config-dir is a flag on remote
client subcommands such as workspace, sandbox, node, image, and ping; it is not
a root-global flag and commands such as version and blackbox do not accept it.
Consult the exact subcommand's --help before concluding that a public operation
is unavailable. For every remote workspace, sandbox, node, or image operation,
pass both --orchestrator ${orchestrator_url} and --config-dir ./client-config.
For the ping command only, use the positional address ${ping_endpoint} plus
--config-dir ./client-config. These are the only campaign endpoints: do not
infer, scan for, start, or substitute another control plane.

You have unrestricted host networking and shell access because Codex itself is
not the security boundary. Run untrusted project commands only inside
darkaifactory sandboxes. Do not weaken TLS, expose credentials, stop host
services, or mutate unrelated host files. Browser-only goals must drive an
actual browser, either headed or headless with DevTools/screenshots; do not
substitute curl. The dashboard is ${dashboard_description}. Do not inspect the
client-config symlink or its contents.

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
