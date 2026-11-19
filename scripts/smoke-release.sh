#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <vmjail release archive>" >&2
	exit 2
fi

archive=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
tar -xzf "$archive" -C "$work"
root=$(find "$work" -mindepth 1 -maxdepth 1 -type d -name 'vmjail-*' -print)
test -n "$root"
test "$(printf '%s\n' "$root" | wc -l)" -eq 1

test -x "$root/bin/vmjail-sandboxd"
"$root/bin/vmjail-sandboxd" version >/dev/null
"$root/bin/vmjail-sandboxd" --help >/dev/null

echo "portable release smoke passed"
