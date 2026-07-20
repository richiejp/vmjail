#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then echo "usage: $0 SANDBOXD_PATH" >&2; exit 2; fi
artifact=$1
if [ ! -f "$artifact" ] || [ ! -x "$artifact" ]; then echo "artifact must be a regular executable" >&2; exit 1; fi
if command -v sha256sum >/dev/null 2>&1; then
    digest=$(sha256sum "$artifact" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    digest=$(shasum -a 256 "$artifact" | awk '{print $1}')
else
    echo "sha256sum or shasum is required" >&2; exit 1
fi
version=$("$artifact" version)
printf 'schema=daft-artifact-v1\npath=%s\nsha256=%s\nversion=%s\n' "$artifact" "$digest" "$version"
if command -v file >/dev/null 2>&1; then printf 'file=%s\n' "$(file -b "$artifact")"; fi
