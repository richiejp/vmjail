//go:build ignore

// open-snoop BPF program — the first Tier-2 CO-RE gadget.
//
// It attaches to the raw syscall tracepoint sys_enter_openat and, for
// every openat(2) the guest makes, pushes one fixed-size record onto a
// BPF ring buffer: the calling pid/uid, the comm, the dirfd + flags, and
// the path the caller passed. The Go side (opensnoop.go) drains the ring
// buffer and renders one line per open. See OBSERVABILITY.md "Tier 2" —
// the motivating bug was a lint cache miss that an openat() returning
// ENOENT would have made obvious from one guest-side log line.
//
// CO-RE: trace_event_raw_sys_enter comes from the kernel's own BTF via
// the generated vmlinux.h, so reading ctx->args[] relocates against the
// *guest* kernel's layout at load time. Compiled to a committed ELF by
// `make gen-bpf` (clang -target bpf); the normal build consumes the
// committed object and needs no toolchain.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16
#define NAME_MAX 255

// Wire layout shared with opensnoop.go's parseEvent. Keep the field
// order and widths in lockstep with that decoder — the Go side reads
// this by fixed offset, little-endian.
struct event {
	__u32 pid;
	__u32 uid;
	__s32 dfd;
	__s32 flags;
	__u8 comm[TASK_COMM_LEN];
	__u8 filename[NAME_MAX];
};

// Force bpf2go (-type event) to emit the struct into the generated BTF.
const struct event *unused_event __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

SEC("tracepoint/syscalls/sys_enter_openat")
int handle_openat(struct trace_event_raw_sys_enter *ctx)
{
	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;

	__u64 id = bpf_get_current_pid_tgid();
	e->pid = id >> 32;
	e->uid = (__u32)bpf_get_current_uid_gid();
	e->dfd = (__s32)ctx->args[0];
	e->flags = (__s32)ctx->args[2];
	bpf_get_current_comm(&e->comm, sizeof(e->comm));
	bpf_probe_read_user_str(&e->filename, sizeof(e->filename),
				(const char *)ctx->args[1]);

	bpf_ringbuf_submit(e, 0);
	return 0;
}
