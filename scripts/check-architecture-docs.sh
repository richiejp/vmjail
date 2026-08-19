#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
architecture_dir="$repo_root/docs/architecture"
max_words=750
components=(cluster orchestrator client node workspace sandbox storage)

fail() {
	printf 'architecture-docs: %s\n' "$*" >&2
	exit 1
}

for component in "${components[@]}"; do
	page="$architecture_dir/$component.md"
	lifecycle="$architecture_dir/$component-lifecycle.md"
	diagram="$architecture_dir/diagrams/$component-lifecycle.dot"
	rendered="$architecture_dir/diagrams/$component-lifecycle.svg"
	[[ -f "$page" ]] || fail "missing component page: $page"
	[[ -f "$lifecycle" ]] || fail "missing lifecycle page: $lifecycle"
	[[ -f "$diagram" ]] || fail "missing lifecycle diagram: $diagram"
	[[ -s "$rendered" ]] || fail "missing rendered lifecycle diagram: $rendered"
	rg -Fq "Lifecycle:" "$page" || fail "$page does not link its lifecycle"
	rg -Fq "Initial invariants to formalise" "$lifecycle" || fail "$lifecycle has no invariant section"
	rg -Fq "Implementation map" "$lifecycle" || fail "$lifecycle has no implementation map"
	rg -Fq "diagrams/$component-lifecycle.dot" "$lifecycle" || fail "$lifecycle does not link its diagram"
	rg -Fq "diagrams/$component-lifecycle.svg" "$lifecycle" || fail "$lifecycle does not embed its rendered diagram"
done

while IFS= read -r page; do
	words=$(wc -w <"$page")
	((words <= max_words)) || fail "$page has $words words; limit is $max_words"
done < <(printf '%s\n' "$repo_root/ARCHITECTURE.md"; find "$architecture_dir" -maxdepth 1 -name '*.md' ! -name 'resource-model-reference.md' -print | sort)

while IFS=: read -r page target; do
	case "$target" in
	'' | '#'* | http://* | https://*) continue ;;
	esac
	target=${target%%#*}
	resolved=$(realpath -m "$(dirname "$page")/$target")
	[[ -e "$resolved" ]] || fail "broken link: $page -> $target"
done < <(rg -o --no-heading --replace '$1' '\]\(([^)]+)\)' "$repo_root/ARCHITECTURE.md" "$architecture_dir"/*.md)

for page in "$architecture_dir"/*.md; do
	base=$(basename "$page")
	case "$base" in
	resource-model-reference.md) continue ;;
	esac
	rg -Fq "($base)" "$repo_root/ARCHITECTURE.md" "$architecture_dir"/*.md || fail "orphan page: $page"
done

printf 'architecture docs: OK (%d component/lifecycle pairs, %d-word limit)\n' "${#components[@]}" "$max_words"
