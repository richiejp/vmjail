#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/daft-go-nav-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

cat >"$tmp/gopls" <<'EOF'
#!/bin/sh
i=1
while [ "$i" -le 150 ]; do
	echo "result $i"
	i=$((i + 1))
done
exit "${FAKE_GOPLS_STATUS:-0}"
EOF
chmod +x "$tmp/gopls"

output=$(PATH="$tmp:$PATH" DAFT_NAV_MAX_LINES=10 "$repo/scripts/go-nav.sh" symbol Example)
lines=$(printf '%s\n' "$output" | wc -l)
[ "$lines" -eq 11 ] || {
	echo "go-nav cap: got $lines lines, want 11" >&2
	exit 1
}
printf '%s\n' "$output" | tail -n 1 | grep -q '^\[output truncated;'

if PATH="$tmp:$PATH" DAFT_NAV_MAX_LINES=0 "$repo/scripts/go-nav.sh" symbol Example >/dev/null 2>&1; then
	echo "go-nav accepted an invalid line cap" >&2
	exit 1
fi

if PATH="$tmp:$PATH" FAKE_GOPLS_STATUS=7 "$repo/scripts/go-nav.sh" check example.go >/dev/null 2>&1; then
	echo "go-nav discarded the gopls failure status" >&2
	exit 1
else
	status=$?
	[ "$status" -eq 7 ] || {
		echo "go-nav returned $status, want 7" >&2
		exit 1
	}
fi
