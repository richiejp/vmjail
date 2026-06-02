//go:build ignore

// open-snoop BPF program — the first Tier-2 CO-RE gadget.
//
// It traces every openat(2) the guest makes and pushes one fixed-size
// record onto a BPF ring buffer: the calling pid/uid, the comm, the dirfd
// + flags, the path, AND the syscall's return value (a fd, or a negative
// errno). The Go side (opensnoop.go) drains the ring buffer and renders one
// line per open. See OBSERVABILITY.md "Tier 2" — the motivating bug was a
// lint cache miss that an openat() returning ENOENT would have made obvious
// from one guest-side log line, which is exactly what the return value
// surfaces.
//
// Enter/exit correlation: sys_enter_openat carries the path + flags but no
// result; sys_exit_openat carries the result but no path. So the enter hook
// stashes the partial record in a per-thread hash map keyed by pid_tgid,
// and the exit hook completes it with the return value, emits it, and drops
// the map entry. The record is therefore emitted once, at completion, with
// the outcome known. The container-workload filter runs at enter, so a
// filtered open is never stashed and its exit lookup simply misses.
//
// CO-RE: trace_event_raw_sys_enter/exit and task_struct come from the
// kernel's own BTF via the generated vmlinux.h, so the field reads relocate
// against the *guest* kernel's layout at load time. Compiled to a committed
// ELF by `make gen-bpf` (clang -target bpf); the normal build consumes the
// committed object and needs no toolchain.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16
#define NAME_MAX 255

// self_pidns is the daft-agent's own PID-namespace inode number, written
// into .rodata by the Go loader (opensnoop.go) before load. When non-zero,
// every openat from a task sharing this namespace — the agent, the guest
// init, kernel threads, and crun itself — is dropped, so the trace and the
// build-progress aggregate carry only the *container workload*, which crun
// runs in a child PID namespace (the OCI bundle always requests a `pid`
// namespace; see internal/imagestore/runtimespec.go). Zero disables the
// filter and emits every openat — the pre-filter behaviour, kept as a safe
// fallback for when the agent can't determine its own namespace.
const volatile __u32 self_pidns = 0;

// Wire layout shared with opensnoop.go's parseEvent. Keep the field order
// and widths in lockstep with that decoder — the Go side reads this by
// fixed offset, little-endian. ret is the openat(2) return value: a
// non-negative fd on success, a negative errno (e.g. -2 = -ENOENT) on
// failure.
struct event {
	__u32 pid;
	__u32 uid;
	__s32 dfd;
	__s32 flags;
	__s32 ret;
	__u8 comm[TASK_COMM_LEN];
	__u8 filename[NAME_MAX];
};

// Force bpf2go (-type event) to emit the struct into the generated BTF.
const struct event *unused_event __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

// inflight holds the partial record between enter and exit, keyed by
// pid_tgid. Sized for many concurrent threads in a build; an entry that
// never sees its exit (a task killed mid-syscall) is leaked until the key
// is reused, which is bounded and harmless for a diagnostic gadget.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct event);
} inflight SEC(".maps");

SEC("tracepoint/syscalls/sys_enter_openat")
int handle_openat_enter(struct trace_event_raw_sys_enter *ctx)
{
	// Container-workload filter: drop opens from tasks in the agent's own
	// PID namespace (agent, guest init, kernel threads, crun). The
	// workload runs in a child namespace, so anything matching self_pidns
	// is harness noise the summary was being dominated by. Filtered opens
	// are never stashed, so their exit lookup misses and nothing is
	// emitted.
	if (self_pidns) {
		struct task_struct *task = (struct task_struct *)bpf_get_current_task();
		__u32 pidns = BPF_CORE_READ(task, nsproxy, pid_ns_for_children, ns.inum);
		if (pidns == self_pidns)
			return 0;
	}

	__u64 id = bpf_get_current_pid_tgid();
	struct event e = {};
	e.pid = id >> 32;
	e.uid = (__u32)bpf_get_current_uid_gid();
	e.dfd = (__s32)ctx->args[0];
	e.flags = (__s32)ctx->args[2];
	bpf_get_current_comm(&e.comm, sizeof(e.comm));
	bpf_probe_read_user_str(&e.filename, sizeof(e.filename),
				(const char *)ctx->args[1]);
	bpf_map_update_elem(&inflight, &id, &e, BPF_ANY);
	return 0;
}

SEC("tracepoint/syscalls/sys_exit_openat")
int handle_openat_exit(struct trace_event_raw_sys_exit *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct event *start = bpf_map_lookup_elem(&inflight, &id);
	if (!start)
		return 0; // filtered at enter, or no matching enter — nothing to emit.

	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (e) {
		__builtin_memcpy(e, start, sizeof(*e));
		e->ret = (__s32)ctx->ret;
		bpf_ringbuf_submit(e, 0);
	}
	bpf_map_delete_elem(&inflight, &id);
	return 0;
}
