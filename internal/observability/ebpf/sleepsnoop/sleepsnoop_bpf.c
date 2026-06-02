//go:build ignore

// sleep-snoop BPF program — Tier-2 CO-RE gadget for voluntary sleeps.
//
// It traces every nanosleep(2) and clock_nanosleep(2) the guest makes and
// pushes one record onto a ring buffer: the calling pid (tgid), the caller's
// comm, and the requested sleep duration in nanoseconds. This is the
// strace-style "who is sitting in a sleep loop" signal — the motivating
// hypothesis (OBSERVABILITY.md "Tier 2") is that a slow build is often a
// process polling-with-sleep instead of waiting on a real event ("is it up
// yet? no → sleep 100ms → ask again"), which a per-comm tally of sleep calls
// and cumulative requested sleep makes obvious.
//
// Enter-only: unlike open-snoop / exec-snoop there is no result worth
// correlating — the requested duration is known at entry, and whether the
// sleep was interrupted (EINTR) does not change "this process asked to
// sleep". So each enter tracepoint emits one record directly; no inflight
// map, no exit hook.
//
// clock_nanosleep(clockid, flags, req, rem) with TIMER_ABSTIME asks to sleep
// *until* an absolute time, so its `req` is a wall-clock instant, not a
// duration — recording it as a duration would be garbage. Such calls are
// still counted (they're still a sleep) but with req_ns = 0 (unknown), so the
// cumulative-duration aggregate stays meaningful.
//
// CO-RE: trace_event_raw_sys_enter and task_struct come from the guest
// kernel's BTF via vmlinux.h, relocated at load. Compiled to a committed ELF
// by `make gen-bpf`.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16
// TIMER_ABSTIME (uapi/linux/time.h): clock_nanosleep flag meaning `req` is an
// absolute deadline, not a relative duration.
#define TIMER_ABSTIME 1
#define NSEC_PER_SEC 1000000000ULL

// self_pidns is the daft-agent's own PID-namespace inode, written into
// .rodata by the Go loader before load. Non-zero drops every sleep from a
// task in that namespace (agent, init, kernel threads, crun), leaving only
// the container workload — symmetric with open-snoop / exec-snoop. Zero
// disables the filter.
const volatile __u32 self_pidns = 0;

// Wire layout shared with sleepsnoop.go's parseEvent. req_ns leads so the
// __u64 is naturally aligned; the Go side reads each field by fixed offset,
// little-endian. The C struct is 32 bytes (28 used + 4 trailing pad to the
// 8-byte alignment), which the Go decoder's recordSize matches.
struct event {
	__u64 req_ns;             // requested sleep in ns; 0 = absolute/unknown
	__u32 pid;                // tgid of the caller
	__u8 comm[TASK_COMM_LEN]; // caller's comm
};

// Force bpf2go (-type event) to emit the struct into the generated BTF.
const struct event *unused_event __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

// record_sleep emits one sleep observation. req_ptr is the user-space
// `struct timespec *` the syscall was passed; absolute is non-zero for a
// clock_nanosleep with TIMER_ABSTIME, in which case the duration is left 0.
static __always_inline int record_sleep(const void *req_ptr, int absolute)
{
	if (self_pidns) {
		struct task_struct *task = (struct task_struct *)bpf_get_current_task();
		__u32 pidns = BPF_CORE_READ(task, nsproxy, pid_ns_for_children, ns.inum);
		if (pidns == self_pidns)
			return 0;
	}

	struct event e = {};
	e.pid = bpf_get_current_pid_tgid() >> 32;
	bpf_get_current_comm(&e.comm, sizeof(e.comm));

	// timespec is { tv_sec, tv_nsec }, two longwords on the 64-bit guest.
	// Read it from user space; a failed read or a nonsensical (negative)
	// value leaves req_ns 0 rather than poisoning the duration aggregate.
	if (!absolute && req_ptr) {
		__s64 ts[2] = {};
		if (bpf_probe_read_user(&ts, sizeof(ts), req_ptr) == 0 &&
		    ts[0] >= 0 && ts[1] >= 0)
			e.req_ns = (__u64)ts[0] * NSEC_PER_SEC + (__u64)ts[1];
	}

	bpf_ringbuf_output(&events, &e, sizeof(e), 0);
	return 0;
}

// nanosleep(req, rem): req is the relative duration at args[0].
SEC("tracepoint/syscalls/sys_enter_nanosleep")
int handle_nanosleep_enter(struct trace_event_raw_sys_enter *ctx)
{
	return record_sleep((const void *)ctx->args[0], 0);
}

// clock_nanosleep(clockid, flags, req, rem): req is at args[2]; flags at
// args[1] carries TIMER_ABSTIME, which makes req an absolute deadline.
SEC("tracepoint/syscalls/sys_enter_clock_nanosleep")
int handle_clock_nanosleep_enter(struct trace_event_raw_sys_enter *ctx)
{
	int flags = (int)ctx->args[1];
	return record_sleep((const void *)ctx->args[2], flags & TIMER_ABSTIME);
}
