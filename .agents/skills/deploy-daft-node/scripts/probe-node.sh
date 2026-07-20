#!/bin/sh
set -eu

command_path() {
    if command -v "$1" >/dev/null 2>&1; then command -v "$1"; else printf '%s' absent; fi
}

readable_file() {
    if [ -r "$1" ]; then tr '\n' ' ' < "$1"; else printf '%s' unavailable; fi
}

printf 'schema=daft-node-probe-v1\n'
printf 'kernel=%s\n' "$(uname -s 2>/dev/null || printf unknown)"
printf 'architecture=%s\n' "$(uname -m 2>/dev/null || printf unknown)"
printf 'user_id=%s\n' "$(id -u 2>/dev/null || printf unknown)"
printf 'group_id=%s\n' "$(id -g 2>/dev/null || printf unknown)"
printf 'pid1=%s\n' "$(readable_file /proc/1/comm)"
printf 'systemctl=%s\n' "$(command_path systemctl)"
printf 'openrc=%s\n' "$(command_path rc-service)"
printf 'runit=%s\n' "$(command_path sv)"
printf 's6=%s\n' "$(command_path s6-svscanctl)"
printf 'sha256sum=%s\n' "$(command_path sha256sum)"
printf 'shasum=%s\n' "$(command_path shasum)"
if [ -e /dev/kvm ]; then
    printf 'kvm_exists=true\n'
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then printf 'kvm_access=rw\n'; else printf 'kvm_access=denied\n'; fi
else
    printf 'kvm_exists=false\n'
    printf 'kvm_access=absent\n'
fi
printf 'cgroup_controllers=%s\n' "$(readable_file /sys/fs/cgroup/cgroup.controllers)"
printf 'current_cgroup=%s\n' "$(readable_file /proc/self/cgroup)"
if [ -w /sys/fs/cgroup ]; then printf 'cgroup_root_writable=true\n'; else printf 'cgroup_root_writable=false\n'; fi
printf 'home=%s\n' "${HOME:-unknown}"
printf 'tmpdir=%s\n' "${TMPDIR:-/tmp}"
