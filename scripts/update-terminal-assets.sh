#!/bin/sh
set -eu

if [ "${DAFT_SANDBOX_BUILD:-}" != "1" ]; then
	echo "update-terminal-assets: run through 'daft workspace exec' with DAFT_SANDBOX_BUILD=1" >&2
	exit 1
fi

repo=${1:-$(pwd)}
frontend="$repo/web/terminal"
vendor="$repo/internal/orchestrator/web/thirdparty/xterm"

cd "$frontend"
bun install --ignore-scripts --frozen-lockfile

check_file() {
	expected=$1
	file=$2
	actual=$(sha512sum "$file" | cut -d ' ' -f 1)
	if [ "$actual" != "$expected" ]; then
		echo "update-terminal-assets: unexpected SHA-512 for $file: $actual" >&2
		exit 1
	fi
}

check_file ba2ccd21cae87d6f6dbcfc420cddb3cb744a19eb053e2f27c15d12104e3cd06faa529c83696108f626bd967c138ad894d436d6e302d0700fef16b5738e056477 node_modules/@xterm/xterm/lib/xterm.js
check_file cf53598cf599479e4a6fc37fb67507d595328654c02e61ba568f775897738e8dff1ba36c5367ef7f50f21ca5c27d6b850c5a990c6f6ef407b231c2888e818447 node_modules/@xterm/xterm/css/xterm.css
check_file c7622323eb5eb75589e131cc1156d839f953c7e2b478614b4f3520da92279d6c15d0a43df6ece63a15ec5d8b5ccebffa637820ae58cb0d01bd0d0732621fa206 node_modules/@xterm/xterm/LICENSE

install -m 0644 node_modules/@xterm/xterm/lib/xterm.js "$vendor/xterm.js"
install -m 0644 node_modules/@xterm/xterm/css/xterm.css "$vendor/xterm.css"
install -m 0644 node_modules/@xterm/xterm/LICENSE "$vendor/LICENSE"
