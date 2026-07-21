#!/bin/sh
# Bounded gopls navigation for humans and coding agents.
set -eu

usage() {
	cat <<'EOF'
usage: scripts/go-nav.sh <command> <query>

commands:
  symbols FILE             list declarations in one Go file
  symbol QUERY             search workspace declarations
  definition FILE:LINE:COL find the selected declaration
  references FILE:LINE:COL find references to the selected identifier
  call-hierarchy FILE:LINE:COL show incoming/outgoing calls
  check FILE               show diagnostics for one Go file

Set DAFT_NAV_MAX_LINES to change the output cap (default 120, maximum 500).
EOF
}

[ "$#" -eq 2 ] || { usage >&2; exit 2; }

max_lines=${DAFT_NAV_MAX_LINES:-120}
case "$max_lines" in
	''|*[!0-9]*) echo "DAFT_NAV_MAX_LINES must be an integer" >&2; exit 2 ;;
esac
[ "$max_lines" -ge 1 ] && [ "$max_lines" -le 500 ] || {
	echo "DAFT_NAV_MAX_LINES must be between 1 and 500" >&2
	exit 2
}

cache_root=${XDG_CACHE_HOME:-${TMPDIR:-/tmp}/daft-gopls-cache-$(id -u)}
export XDG_CACHE_HOME=$cache_root

# A contributor may be updating go.mod and vendor/ in separate commits. LSP
# navigation must remain read-only and must not fail merely because Go would
# otherwise select the incomplete vendor tree automatically.
case " ${GOFLAGS:-} " in
	*" -mod="*) ;;
	*) GOFLAGS="${GOFLAGS:+$GOFLAGS }-mod=readonly"; export GOFLAGS ;;
esac

tmp=$(mktemp "${TMPDIR:-/tmp}/daft-go-nav.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM

case "$1" in
	symbols) command="symbols" ;;
	symbol) command="workspace_symbol" ;;
	definition) command="definition" ;;
	references) command="references" ;;
	call-hierarchy) command="call_hierarchy" ;;
	check) command="check" ;;
	*) usage >&2; exit 2 ;;
esac

status=0
gopls "$command" "$2" >"$tmp" 2>&1 || status=$?
awk -v max="$max_lines" '
	NR <= max { print }
	NR == max + 1 { truncated = 1 }
	END { if (truncated) print "[output truncated; refine the query or raise DAFT_NAV_MAX_LINES]" }
' "$tmp"
exit "$status"
