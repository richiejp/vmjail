#!/usr/bin/env bash
#
# check-submodule-revs.sh — keep third_party/submodule-revs.json in sync
# with .gitmodules + `git submodule status`. The flake reads that JSON
# via builtins.fetchGit (url + rev per submodule); if either drifts we
# rebuild against the wrong source. This script is the gate.
#
# Behaviour:
#   * Regenerates the JSON from current .gitmodules urls + submodule
#     HEADs (sorted by name for stable diffs).
#   * If the regenerated content matches the existing file: exits 0.
#   * If it differs: overwrites the file and exits 1 so the caller
#     fails loudly. The dev re-stages the regenerated file and retries
#     the commit. This catches both directions of drift:
#       - bumped a submodule but didn't update the JSON
#       - hand-edited the JSON without bumping the submodule
#
# Run via `make check-submodule-revs` or `make verify`; pre-commit
# pulls verify, so a drift is caught before the commit lands.

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

revs_file="third_party/submodule-revs.json"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# git submodule status prefixes the rev with a space, '+', '-', or 'U'
# (clean / out-of-sync / not-initialized / merge-conflict). Strip the
# leading character so we always quote the raw 40-char SHA.
declare -A rev_by_path
while IFS= read -r line; do
	# Format: "[ +-U]<sha> <path>[ (<describe>)]"
	raw_rev="${line:0:41}"
	rev="${raw_rev:1}"
	rest="${line:42}"
	path="${rest%% *}"
	# A '-' prefix means "not initialized"; the SHA is the expected one
	# from the parent index, which is still what we want to pin against.
	rev_by_path["$path"]="$rev"
done < <(git submodule status)

# Build the JSON. Sort submodule names so output is deterministic.
declare -A url_by_path
while IFS=$'\n' read -r line; do
	# Format: "submodule.<name>.path <path>" or ".url <url>"
	key="${line%% *}"
	value="${line#* }"
	name="${key#submodule.}"
	field="${name##*.}"
	name="${name%."$field"}"
	case "$field" in
	path) url_by_path["__path_$name"]="$value" ;;
	url) url_by_path["__url_$name"]="$value" ;;
	esac
done < <(git config --file .gitmodules --get-regexp '^submodule\.')

# Collect submodule names (the keys that begin with __path_), sorted.
names=()
for k in "${!url_by_path[@]}"; do
	if [[ "$k" == __path_* ]]; then
		names+=("${k#__path_}")
	fi
done
IFS=$'\n' names_sorted=($(printf '%s\n' "${names[@]}" | sort))
unset IFS

{
	printf '{\n'
	first=1
	for name in "${names_sorted[@]}"; do
		path="${url_by_path[__path_$name]}"
		url="${url_by_path[__url_$name]}"
		rev="${rev_by_path[$path]:-}"
		if [ -z "$rev" ]; then
			echo "ERROR: no submodule status for $path" >&2
			exit 1
		fi
		short_name="${path##*/}"
		if [ $first -eq 1 ]; then
			first=0
		else
			printf ',\n'
		fi
		printf '  "%s": {\n    "url": "%s",\n    "rev": "%s"\n  }' \
			"$short_name" "$url" "$rev"
	done
	printf '\n}\n'
} > "$tmp"

if [ -f "$revs_file" ] && cmp -s "$revs_file" "$tmp"; then
	exit 0
fi

cp "$tmp" "$revs_file"
echo "submodule-revs drift detected; $revs_file regenerated."
echo "Stage it ('git add $revs_file') and re-run the commit."
exit 1
