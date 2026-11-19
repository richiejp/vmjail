#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <darkaifactory release archive>" >&2
	exit 2
fi

archive=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
tar -xzf "$archive" -C "$work"
root=$(find "$work" -mindepth 1 -maxdepth 1 -type d -name 'darkaifactory-*' -print)
test -n "$root"
test "$(printf '%s\n' "$root" | wc -l)" -eq 1

test -x "$root/bin/daft-sandboxd"
"$root/bin/daft-sandboxd" version >/dev/null
"$root/bin/daft-sandboxd" --help >/dev/null

echo "portable release smoke passed"
