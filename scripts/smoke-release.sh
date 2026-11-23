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

for binary in vmjail vmjail-sandboxd; do
	test -x "$root/bin/$binary"
	"$root/bin/$binary" version >/dev/null
	"$root/bin/$binary" --help >/dev/null
done

echo "portable release smoke passed"
