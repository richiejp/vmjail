#!/usr/bin/env bash
# Narrow fault-injection control supplied only to disposable blackbox campaigns.
set -euo pipefail

pid=${DAFT_BLACKBOX_NODE_PID:-}
[[ "$pid" =~ ^[1-9][0-9]*$ ]] || {
	echo "campaign-control: disposable Node PID is unavailable" >&2
	exit 2
}

case "${1:-}" in
node-pause)
	kill -STOP "$pid"
	;;
node-resume)
	kill -CONT "$pid"
	;;
node-status)
	if kill -0 "$pid" 2>/dev/null; then
		echo "node process present"
	else
		echo "node process absent"
		exit 1
	fi
	;;
*)
	echo "usage: campaign-control node-pause|node-resume|node-status" >&2
	exit 2
	;;
esac
