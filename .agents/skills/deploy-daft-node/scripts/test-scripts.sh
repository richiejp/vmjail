#!/bin/sh
set -eu

scripts=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
tmp=${TMPDIR:-/tmp}/daft-deploy-skill-test.$$
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir -p "$tmp"

fail() {
    echo "deploy-daft-node test: $*" >&2
    exit 1
}

expect_failure() {
    description=$1
    shift
    if "$@" >"$tmp/failure.out" 2>&1; then
        fail "$description unexpectedly succeeded"
    fi
}

digest_file() {
    sha256sum "$1" | awk '{print $1}'
}

make_artifact() {
    path=$1 version=$2
    printf '#!/bin/sh\n[ "$1" = version ] || exit 2\nprintf "%%s\\n" %s\n' "$version" > "$path"
    chmod 0755 "$path"
}

# Fresh install, local inspection, and verification.
make_artifact "$tmp/daft.upload" v1
digest_v1=$(digest_file "$tmp/daft.upload")
inspection=$("$scripts/inspect-artifact.sh" "$tmp/daft.upload")
printf '%s\n' "$inspection" | grep -F 'version=v1' >/dev/null
printf '%s\n' "$inspection" | grep -E '^runtime_architecture=.+$' >/dev/null
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v1" v1 >/dev/null
"$scripts/verify-node.sh" "$tmp/daft" https://orchestrator.invalid v1 | grep -F 'local_binary=ok' >/dev/null

# Reapplying the exact release is a no-op and consumes the upload.
make_artifact "$tmp/daft.upload" v1
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v1" v1 | grep -F 'status=noop' >/dev/null
[ ! -e "$tmp/daft.upload" ] || fail 'no-op left the upload behind'

# Upgrade retains one rollback generation.
make_artifact "$tmp/daft.upload" v2
digest_v2=$(digest_file "$tmp/daft.upload")
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v2" v2 >/dev/null
[ "$("$tmp/daft" version)" = v2 ]
[ "$("$tmp/daft.previous" version)" = v1 ]
"$scripts/stage-release.sh" --rollback "$tmp/daft"
[ "$("$tmp/daft" version)" = v1 ]

# An incompatible digest and an interrupted/missing upload cannot disturb the
# installed release.
make_artifact "$tmp/daft.upload" incompatible
expect_failure 'digest mismatch' "$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v2" incompatible
[ "$("$tmp/daft" version)" = v1 ]
rm -f "$tmp/daft.upload"
expect_failure 'interrupted upload' "$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v2" v2
[ "$("$tmp/daft" version)" = v1 ]

# A failed service start after a valid upgrade is explicitly recoverable using
# the documented rollback command.
make_artifact "$tmp/daft.upload" v2
"$scripts/stage-release.sh" "$tmp/daft.upload" "$tmp/daft" "$digest_v2" v2 >/dev/null
if false; then fail 'failed-start fixture unexpectedly started'; else "$scripts/stage-release.sh" --rollback "$tmp/daft"; fi
[ "$("$tmp/daft" version)" = v1 ]

# Post-rename validation is fail-closed even on a fresh installation. This
# fixture returns the expected version once before changing its answer.
counter="$tmp/version-count"
stateful="$tmp/stateful.upload"
printf '#!/bin/sh\n[ "$1" = version ] || exit 2\nn=0; [ ! -f %s ] || n=$(cat %s); n=$((n + 1)); printf "%%s" "$n" > %s; if [ "$n" -eq 1 ]; then echo v3; else echo corrupt; fi\n' "$counter" "$counter" "$counter" > "$stateful"
chmod 0755 "$stateful"
digest_v3=$(digest_file "$stateful")
expect_failure 'post-rename validation' "$scripts/stage-release.sh" "$stateful" "$tmp/fresh" "$digest_v3" v3
[ ! -e "$tmp/fresh" ] || fail 'invalid fresh install was retained'

# An execution failure during the post-rename check must take the rollback
# path too; `set -e` must not bypass restoration.
make_artifact "$tmp/daft" v1
failing="$tmp/failing.upload"
printf '#!/bin/sh\n[ "$1" = version ] || exit 2\nn=0; [ ! -f %s ] || n=$(cat %s); n=$((n + 1)); printf "%%s" "$n" > %s; [ "$n" -eq 1 ] || exit 1; echo v4\n' "$counter.fail" "$counter.fail" "$counter.fail" > "$failing"
chmod 0755 "$failing"
digest_v4=$(digest_file "$failing")
expect_failure 'post-rename execution' "$scripts/stage-release.sh" "$failing" "$tmp/daft" "$digest_v4" v4
[ "$("$tmp/daft" version)" = v1 ] || fail 'post-rename execution failure did not restore v1'

# Partial fleet upgrades preserve a deterministic rollback boundary. Two Nodes
# accept v2 while the third fails post-stage and restores v1; rolling back only
# the changed subset returns the whole fleet to v1.
for node in a b c; do
    mkdir -p "$tmp/fleet-$node"
    make_artifact "$tmp/fleet-$node/upload" v1
    "$scripts/stage-release.sh" "$tmp/fleet-$node/upload" "$tmp/fleet-$node/daft" "$digest_v1" v1 >/dev/null
done
for node in a b; do
    make_artifact "$tmp/fleet-$node/upload" v2
    "$scripts/stage-release.sh" "$tmp/fleet-$node/upload" "$tmp/fleet-$node/daft" "$digest_v2" v2 >/dev/null
done
fleet_counter="$tmp/fleet-c/version-count"
fleet_failing="$tmp/fleet-c/upload"
printf '#!/bin/sh\n[ "$1" = version ] || exit 2\nn=0; [ ! -f %s ] || n=$(cat %s); n=$((n + 1)); printf "%%s" "$n" > %s; if [ "$n" -eq 1 ]; then echo v2; else echo corrupt; fi\n' "$fleet_counter" "$fleet_counter" "$fleet_counter" > "$fleet_failing"
chmod 0755 "$fleet_failing"
fleet_bad_digest=$(digest_file "$fleet_failing")
expect_failure 'partial fleet node c' "$scripts/stage-release.sh" "$fleet_failing" "$tmp/fleet-c/daft" "$fleet_bad_digest" v2
[ "$("$tmp/fleet-a/daft" version)" = v2 ]
[ "$("$tmp/fleet-b/daft" version)" = v2 ]
[ "$("$tmp/fleet-c/daft" version)" = v1 ]
for node in a b; do
    "$scripts/stage-release.sh" --rollback "$tmp/fleet-$node/daft"
    [ "$("$tmp/fleet-$node/daft" version)" = v1 ]
done

# Every documented supervisor strategy must keep the daemon in the foreground
# with the same argument contract. The overseer still owns service-file syntax.
service="$tmp/service"
printf '#!/bin/sh\nprintf "%%s\\n" "$@" > %s\n' "$tmp/service.argv" > "$service"
chmod 0755 "$service"
for supervisor in systemd openrc runit s6; do
    "$service" serve --orchestrator https://orchestrator.invalid --config-dir "$tmp/$supervisor/config"
    expected=$(printf 'serve\n--orchestrator\nhttps://orchestrator.invalid\n--config-dir\n%s\n' "$tmp/$supervisor/config")
    actual=$(cat "$tmp/service.argv")
    [ "$actual" = "$expected" ] || fail "$supervisor changed the foreground service contract"
done

"$scripts/probe-node.sh" | grep -F 'schema=daft-node-probe-v1' >/dev/null
expect_failure 'non-HTTPS orchestrator' "$scripts/verify-node.sh" "$tmp/daft" http://orchestrator.invalid v1
expect_failure 'rollback without previous release' "$scripts/stage-release.sh" --rollback "$tmp/absent"

echo 'deploy-daft-node script tests: OK'
