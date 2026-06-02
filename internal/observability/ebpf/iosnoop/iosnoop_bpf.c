//go:build ignore

// io-snoop BPF program — Tier-2 CO-RE gadget for fd I/O.
//
// It traces the syscalls that move bytes through a file descriptor and
// answers "which files/sockets/pipes is the workload reading and writing,
// and how much" — the strace-style companion to open-snoop / exec-snoop /
// sleep-snoop, aimed at the build that is slow because it is shovelling data
// (a huge artifact write, a chatty package socket) rather than computing.
//
// Why in-kernel aggregation, not a record per syscall: read(2)/write(2) are
// the highest-frequency syscalls in a build — a ring-buffer record each would
// add exactly the per-syscall overhead we are trying to *measure*, and an
// earlier per-event io-snoop draft destabilised the other gadgets that way
// ("slowness is a bug"). So each call instead bumps a per-(pid,fd) LRU hash
// and a monotonic per-CPU global; the Go side iterates the map ~1 Hz, turns
// each (pid,fd) into a target via /proc/<pid>/fd/<fd>, and builds the summary.
// No ring buffer, no userspace wakeup on the hot path.
//
// Enter/exit correlation: sys_enter_<name> carries the fd(s) + direction but
// not the byte count; sys_exit_<name> carries the return (bytes moved, or a
// negative errno) but not the args. So the enter hook stashes a small request
// record keyed by pid_tgid and a single shared exit hook (attached to every
// traced syscall's sys_exit) folds the result in and drops the entry. The
// container-workload filter runs at enter, so a filtered call is never
// stashed and its exit lookup simply misses.
//
// Direction-aware byte attribution: the return value lands as *read* bytes on
// the fd being read from and *write* bytes on the fd being written to. For the
// single-fd families that is one fd; for the zero-copy fd→fd family
// (sendfile/splice/tee/copy_file_range) the same return count is attributed as
// a read on the source fd and a write on the destination fd, so both fds are
// tracked with meaningful counts. vmsplice / recvmmsg / sendmmsg / connect /
// accept carry no clean byte count, so they are recorded calls-only on their
// fd (accept's fd is the *returned* connection fd, known only at exit).
//
// CO-RE: trace_event_raw_sys_enter/exit and task_struct come from the guest
// kernel's BTF via the generated vmlinux.h, relocated at load. Compiled to a
// committed ELF by `make gen-bpf`; the normal build consumes the committed
// object and needs no clang/BTF.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

// Role bits OR-ed into a per-fd entry so a fd touched only with zero-byte
// reads/writes (EOF, empty socket) is still classified r / w / rw. Kept in
// lockstep with iosnoop.go's role rendering.
#define ROLE_READ 1
#define ROLE_WRITE 2

// self_pidns is the daft-agent's own PID-namespace inode, written into
// .rodata by the Go loader before load. Non-zero drops every call from a task
// in that namespace (agent, init, kernel threads, crun), leaving only the
// container workload — symmetric with the other gadgets. Zero disables it.
const volatile __u32 self_pidns = 0;

// fdstat is the per-(pid,fd) accumulator (the ranking source). The key is
// (pid << 32) | fd. Held in an LRU map so a long build's churn of fds can't
// overflow it — cold fds evict; the monotonic totals below are unaffected.
struct fdstat {
	__u64 rd_bytes;
	__u64 wr_bytes;
	__u64 calls;
	__u32 errors;
	__u32 role;
};

// totals is the monotonic global counter (one element, per-CPU so the hot
// path is lockless; the Go side sums across CPUs). It is never evicted, so it
// is the trustworthy source for the summary's Total / rate / error count even
// as fdstat entries come and go.
struct iototals {
	__u64 calls;
	__u64 rd_bytes;
	__u64 wr_bytes;
	__u64 errors;
};

// ioreq is the in-flight record stashed at enter and consumed at exit. It
// names the fd to charge read bytes to (fd_rd) and the fd to charge write
// bytes to (fd_wr); -1 means "none". rd_from_ret marks accept(2), whose read
// fd is the syscall's return value (the new connection), not an arg.
struct ioreq {
	__s32 fd_rd;
	__s32 fd_wr;
	__u32 pid;
	__u32 role_rd;
	__u32 role_wr;
	__u8 bytes_from_ret; // 1: ret is a byte count to attribute; 0: calls-only
	__u8 rd_from_ret;    // 1: fd_rd = ret at exit (accept)
};

// Force bpf2go (-type fdstat) to emit the ranking struct into the generated
// BTF so the Go side can name its layout for the drift guard.
const struct fdstat *unused_fdstat __attribute__((unused));
const struct iototals *unused_iototals __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct fdstat);
} fdstats SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct iototals);
} totals SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 10240);
	__type(key, __u64);
	__type(value, struct ioreq);
} inflight SEC(".maps");

// in_self_pidns reports whether the current task shares the agent's PID
// namespace (the container-workload filter). Cheap no-op when self_pidns is 0.
static __always_inline int in_self_pidns(void)
{
	if (!self_pidns)
		return 0;
	struct task_struct *task = (struct task_struct *)bpf_get_current_task();
	__u32 pidns = BPF_CORE_READ(task, nsproxy, pid_ns_for_children, ns.inum);
	return pidns == self_pidns;
}

// stash records one in-flight I/O syscall keyed by the calling thread, after
// the container-workload filter. The exit hook completes it.
static __always_inline void stash(__u64 id, __s32 fd_rd, __u32 role_rd,
				  __s32 fd_wr, __u32 role_wr,
				  __u8 bytes_from_ret, __u8 rd_from_ret)
{
	if (in_self_pidns())
		return;
	struct ioreq r = {};
	r.pid = id >> 32;
	r.fd_rd = fd_rd;
	r.fd_wr = fd_wr;
	r.role_rd = role_rd;
	r.role_wr = role_wr;
	r.bytes_from_ret = bytes_from_ret;
	r.rd_from_ret = rd_from_ret;
	bpf_map_update_elem(&inflight, &id, &r, BPF_ANY);
}

// bump folds one fd's share of a completed syscall into its per-fd entry. A
// negative fd (the unused side of a single-fd op) is skipped.
static __always_inline void bump(__u32 pid, __s32 fd, __u64 rd, __u64 wr,
				 __u32 role, __u32 err)
{
	if (fd < 0)
		return;
	__u64 key = ((__u64)pid << 32) | (__u32)fd;
	struct fdstat *s = bpf_map_lookup_elem(&fdstats, &key);
	if (s) {
		s->rd_bytes += rd;
		s->wr_bytes += wr;
		s->calls += 1;
		s->errors += err;
		s->role |= role;
		return;
	}
	struct fdstat ns = {};
	ns.rd_bytes = rd;
	ns.wr_bytes = wr;
	ns.calls = 1;
	ns.errors = err;
	ns.role = role;
	bpf_map_update_elem(&fdstats, &key, &ns, BPF_ANY);
}

// ---- enter hooks: one per fd-arg layout, shared across same-shaped syscalls.

// read family — fd at args[0], bytes from ret on the read side.
SEC("tracepoint/syscalls/sys_enter_read")
int handle_read_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[0], ROLE_READ, -1, 0, 1, 0);
	return 0;
}

// write family — fd at args[0], bytes from ret on the write side.
SEC("tracepoint/syscalls/sys_enter_write")
int handle_write_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), -1, 0, (__s32)ctx->args[0], ROLE_WRITE, 1, 0);
	return 0;
}

// mmsg — fd at args[0]; ret is a message count, not bytes, so calls-only.
SEC("tracepoint/syscalls/sys_enter_recvmmsg")
int handle_recvmmsg_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[0], ROLE_READ, -1, 0, 0, 0);
	return 0;
}

SEC("tracepoint/syscalls/sys_enter_sendmmsg")
int handle_sendmmsg_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), -1, 0, (__s32)ctx->args[0], ROLE_WRITE, 0, 0);
	return 0;
}

// connect — existing socket fd at args[0]; calls-only (the target is the fd).
SEC("tracepoint/syscalls/sys_enter_connect")
int handle_connect_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[0], 0, -1, 0, 0, 0);
	return 0;
}

// accept — the tracked fd is the *returned* connection fd (rd_from_ret).
SEC("tracepoint/syscalls/sys_enter_accept")
int handle_accept_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), -1, 0, -1, 0, 0, 1);
	return 0;
}

// vmsplice — single fd at args[0]; direction depends on flags, so calls-only.
SEC("tracepoint/syscalls/sys_enter_vmsplice")
int handle_vmsplice_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[0], 0, -1, 0, 0, 0);
	return 0;
}

// sendfile64(out_fd, in_fd, ...) — read from in (args[1]), write to out (args[0]).
SEC("tracepoint/syscalls/sys_enter_sendfile64")
int handle_sendfile_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[1], ROLE_READ,
	      (__s32)ctx->args[0], ROLE_WRITE, 1, 0);
	return 0;
}

// splice(fd_in, *, fd_out, ...) / copy_file_range(fd_in, *, fd_out, ...) —
// read from args[0], write to args[2].
SEC("tracepoint/syscalls/sys_enter_splice")
int handle_splice_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[0], ROLE_READ,
	      (__s32)ctx->args[2], ROLE_WRITE, 1, 0);
	return 0;
}

// tee(fd_in, fd_out, ...) — read from args[0], write to args[1].
SEC("tracepoint/syscalls/sys_enter_tee")
int handle_tee_enter(struct trace_event_raw_sys_enter *ctx)
{
	stash(bpf_get_current_pid_tgid(), (__s32)ctx->args[0], ROLE_READ,
	      (__s32)ctx->args[1], ROLE_WRITE, 1, 0);
	return 0;
}

// ---- exit hook: one shared program attached to every traced syscall's
// sys_exit. It folds the stashed request in with the now-known return value.
SEC("tracepoint/syscalls/sys_exit_read")
int handle_io_exit(struct trace_event_raw_sys_exit *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct ioreq *r = bpf_map_lookup_elem(&inflight, &id);
	if (!r)
		return 0; // filtered at enter, or no matching enter.

	long ret = ctx->ret;
	__u32 err = ret < 0 ? 1 : 0;
	__u64 bytes = (!err && r->bytes_from_ret) ? (__u64)ret : 0;

	__s32 fd_rd = r->fd_rd;
	if (r->rd_from_ret)
		fd_rd = err ? -1 : (__s32)ret; // accept: new connection fd

	bump(r->pid, fd_rd, bytes, 0, r->role_rd, err);
	bump(r->pid, r->fd_wr, 0, bytes, r->role_wr, err);

	__u32 zero = 0;
	struct iototals *t = bpf_map_lookup_elem(&totals, &zero);
	if (t) {
		t->calls += 1;
		t->rd_bytes += (fd_rd >= 0) ? bytes : 0;
		t->wr_bytes += (r->fd_wr >= 0) ? bytes : 0;
		t->errors += err;
	}

	bpf_map_delete_elem(&inflight, &id);
	return 0;
}
