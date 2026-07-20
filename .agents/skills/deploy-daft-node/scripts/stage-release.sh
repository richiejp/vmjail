#!/bin/sh
set -eu

digest_file() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else echo "sha256sum or shasum is required" >&2; return 1; fi
}

if [ "${1:-}" = --rollback ]; then
    [ "$#" -eq 2 ] || { echo "usage: $0 --rollback DEST" >&2; exit 2; }
    dest=$2
    [ -f "$dest.previous" ] || { echo "no rollback artifact: $dest.previous" >&2; exit 1; }
    if [ -e "$dest" ]; then mv -f "$dest" "$dest.failed"; fi
    if ! mv -f "$dest.previous" "$dest"; then mv -f "$dest.failed" "$dest"; exit 1; fi
    rm -f "$dest.failed"
    exit 0
fi

[ "$#" -eq 4 ] || { echo "usage: $0 TEMP DEST SHA256 VERSION" >&2; exit 2; }
source_file=$1 dest=$2 expected_digest=$3 expected_version=$4
[ -f "$source_file" ] || { echo "staged artifact is not a regular file" >&2; exit 1; }
[ "$(dirname "$source_file")" = "$(dirname "$dest")" ] || { echo "TEMP and DEST must share a directory for atomic rename" >&2; exit 1; }
actual_digest=$(digest_file "$source_file")
[ "$actual_digest" = "$expected_digest" ] || { echo "digest mismatch" >&2; exit 1; }
chmod 0755 "$source_file"
actual_version=$("$source_file" version)
[ "$actual_version" = "$expected_version" ] || { echo "version mismatch: $actual_version" >&2; exit 1; }
if [ -x "$dest" ] && [ "$(digest_file "$dest")" = "$expected_digest" ] && [ "$("$dest" version)" = "$expected_version" ]; then
    rm -f "$source_file"
    printf 'installed=%s\nsha256=%s\nversion=%s\nstatus=noop\n' "$dest" "$actual_digest" "$actual_version"
    exit 0
fi
rm -f "$dest.previous"
if [ -f "$dest" ]; then mv -f "$dest" "$dest.previous"; fi
if ! mv -f "$source_file" "$dest"; then
    if [ -f "$dest.previous" ]; then mv -f "$dest.previous" "$dest"; fi
    exit 1
fi
if [ "$("$dest" version)" != "$expected_version" ]; then
    if [ -f "$dest.previous" ]; then mv -f "$dest" "$dest.failed"; mv -f "$dest.previous" "$dest"; fi
    echo "installed version validation failed" >&2
    exit 1
fi
printf 'installed=%s\nsha256=%s\nversion=%s\n' "$dest" "$actual_digest" "$actual_version"
