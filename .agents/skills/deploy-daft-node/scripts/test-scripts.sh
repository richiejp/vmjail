#!/bin/sh
set -eu

scripts=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
tmp=${TMPDIR:-/tmp}/daft-deploy-skill-test.$$
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir -p "$tmp"

make_artifact() {
    path=$1 version=$2
    printf '#!/bin/sh\n[ "$1" = version ] || exit 2\nprintf "%%s\\n" %s\n' "$version" > "$path"
    chmod 0755 "$path"
}

make_artifact "$tmp/daft.upload" v1
digest=$(sha256sum "$tmp/daft.upload" | awk '{print $1}')
"$scripts/inspect-artifact.sh" "$tmp/daft.upload" | grep -F 'version=v1' >/dev/null
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest" v1 >/dev/null
"$scripts/verify-node.sh" "$tmp/daft" https://orchestrator.invalid v1 | grep -F 'local_binary=ok' >/dev/null

make_artifact "$tmp/daft.upload" v1
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest" v1 | grep -F 'status=noop' >/dev/null

make_artifact "$tmp/daft.upload" v2
digest_v2=$(sha256sum "$tmp/daft.upload" | awk '{print $1}')
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v2" v2 >/dev/null
[ "$("$tmp/daft" version)" = v2 ]
"$scripts/stage-release.sh" --rollback "$tmp/daft"
[ "$("$tmp/daft" version)" = v1 ]

make_artifact "$tmp/daft.upload" bad
if "$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v2" bad >/dev/null 2>&1; then
    echo 'digest mismatch unexpectedly succeeded' >&2
    exit 1
fi
"$scripts/probe-node.sh" | grep -F 'schema=daft-node-probe-v1' >/dev/null
echo 'deploy-daft-node script tests: OK'
