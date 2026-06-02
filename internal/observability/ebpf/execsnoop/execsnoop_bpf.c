//go:build ignore

// exec-snoop BPF program — Tier-2 CO-RE gadget for process launches.
//
// It traces every execve(2) the guest makes and pushes one record onto a
// ring buffer: the calling pid/ppid/uid, the caller's comm, the full argv
// (path + arguments), and the syscall's return value. execve only returns
// to the caller on failure (success replaces the process image), but the
// `sys_exit_execve` tracepoint fires in *both* cases — ret=0 in the new
// program's context on success, ret=-errno in the old context on failure —
// so the same enter→exit correlation open-snoop uses works here: the enter
// hook stashes the partial record (incl. argv) keyed by tgid, and the exit
// hook completes it with ret, emits it, and drops the entry.
//
// This is the "what is the build actually running, and what failed to run"
// signal: a failed exec shows up as ret=ENOENT (command not found) on a
// path you can read, and the argv reveals the actual command line. See
// OBSERVABILITY.md "Tier 2".
//
// Keyed on tgid (not pid_tgid): a non-main thread that calls execve becomes
// the new process's main thread (tid := tgid), so the exit's pid_tgid would
// differ from the enter's; the tgid is stable across the call.
//
// CO-RE: trace_event_raw_sys_enter/exit and task_struct come from the
// guest kernel's BTF via vmlinux.h, relocated at load. Compiled to a
// committed ELF by `make gen-bpf`.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16
// argv capture caps. MAX_ARGS argv entries, each up to ARG_SIZE bytes
// (incl. NUL). A build's longest commands (gcc, nix) can exceed these; the
// record then carries args_count == MAX_ARGS and a clipped tail, which the
// Go side flags as truncated. ARGS_BUF is the packed NUL-separated buffer;
// LAST_ARG bounds the write offset so the verifier accepts the variable
// index.
#define MAX_ARGS 20
#define ARG_SIZE 128
#define ARGS_BUF (MAX_ARGS * ARG_SIZE)
#define LAST_ARG (ARGS_BUF - ARG_SIZE)

// self_pidns is the daft-agent's own PID-namespace inode, written into
// .rodata by the Go loader before load. Non-zero drops every exec from a
// task in that namespace (agent, init, kernel threads, crun), leaving only
// the container workload — symmetric with open-snoop. Zero disables it.
const volatile __u32 self_pidns = 0;

// Wire layout shared with execsnoop.go's parseEvent — fixed-size prefix
// then the packed args buffer. The Go side reads the prefix by fixed
// offset, little-endian, then splits args[:args_size] on NUL.
struct event {
	__u32 pid;        // tgid of the caller
	__u32 ppid;       // tgid of the parent
	__u32 uid;
	__s32 ret;        // 0 on success, -errno on a failed exec
	__s32 args_count; // number of argv entries captured (capped at MAX_ARGS)
	__u32 args_size;  // bytes used in args[]
	__u8 comm[TASK_COMM_LEN]; // caller's comm at enter
	__u8 args[ARGS_BUF];      // NUL-separated argv; args[0] is the path
};

// Force bpf2go (-type event) to emit the struct into the generated BTF.
const struct event *unused_event __attribute__((unused));

// Zero template to initialise a fresh map slot — the struct is far too big
// (~2.6 KB) for the 512-byte BPF stack, so we never make a stack copy; we
// seed the map slot from this global and fill it in place.
const struct event empty_event = {};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u32); // tgid
	__type(value, struct event);
} inflight SEC(".maps");

SEC("tracepoint/syscalls/sys_enter_execve")
int handle_execve_enter(struct trace_event_raw_sys_enter *ctx)
{
	if (self_pidns) {
		struct task_struct *task = (struct task_struct *)bpf_get_current_task();
		__u32 pidns = BPF_CORE_READ(task, nsproxy, pid_ns_for_children, ns.inum);
		if (pidns == self_pidns)
			return 0;
	}

	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;

	// Seed a fresh slot; BPF_NOEXIST skips if an exec is already in flight
	// for this tgid (a re-entrant or racing exec — rare, and we keep the
	// first).
	if (bpf_map_update_elem(&inflight, &tgid, &empty_event, BPF_NOEXIST))
		return 0;
	struct event *e = bpf_map_lookup_elem(&inflight, &tgid);
	if (!e)
		return 0;

	e->pid = tgid;
	e->uid = (__u32)bpf_get_current_uid_gid();
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	e->ppid = (__u32)BPF_CORE_READ(task, real_parent, tgid);
	bpf_get_current_comm(&e->comm, sizeof(e->comm));

	// execve(path, argv, envp): argv is ctx->args[1], a NULL-terminated
	// array of user-space char pointers. Read each pointer, then the
	// string it points at, packing them NUL-separated into args[].
	const char *const *argv = (const char *const *)ctx->args[1];
	e->args_count = 0;
	e->args_size = 0;

#pragma unroll
	for (int i = 0; i < MAX_ARGS; i++) {
		const char *argp = NULL;
		bpf_probe_read_user(&argp, sizeof(argp), &argv[i]);
		if (!argp)
			break;
		if (e->args_size > LAST_ARG)
			break;
		int len = bpf_probe_read_user_str(&e->args[e->args_size], ARG_SIZE, argp);
		if (len <= 0)
			break;
		e->args_size += len;
		e->args_count++;
	}
	return 0;
}

SEC("tracepoint/syscalls/sys_exit_execve")
int handle_execve_exit(struct trace_event_raw_sys_exit *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;
	struct event *e = bpf_map_lookup_elem(&inflight, &tgid);
	if (!e)
		return 0; // filtered at enter, or no matching enter.

	e->ret = (__s32)ctx->ret;
	bpf_ringbuf_output(&events, e, sizeof(*e), 0);
	bpf_map_delete_elem(&inflight, &tgid);
	return 0;
}
