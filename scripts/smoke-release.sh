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

for binary in daft daft-orchestrator daft-sandboxd; do
	test -x "$root/bin/$binary"
	"$root/bin/$binary" version >/dev/null
done

test -f "$root/systemd/darkaifactory-orchestrator.service"
test -f "$root/systemd/darkaifactory-node.service"
test -f "$root/systemd/orchestrator.env.example"
test -f "$root/systemd/node.env.example"

"$root/bin/daft" auth show --config-dir "$work/client" >/dev/null
"$root/bin/daft-orchestrator" auth show --config-dir "$work/orchestrator" >/dev/null
"$root/bin/daft-sandboxd" auth show --config-dir "$work/node" >/dev/null

echo "portable release smoke passed"
