// Package execsnoop is a Tier-2 CO-RE eBPF gadget (see OBSERVABILITY.md
// "Tier 2"). It runs inside the guest, attached by daft-agent, and reports
// every execve(2) the workload makes — the program path, its full argv, and
// the return value — so "what is the build actually running, and what
// failed to run" is answerable from a guest-side log. A failed exec shows
// up as ret=ENOENT (command not found) on a readable path; the argv shows
// the actual command line. It is the strace-style companion to open-snoop.
//
// The BPF object is compiled from execsnoop_bpf.c into a committed ELF
// (execsnoop_bpfel.o) and loader (execsnoop_bpfel.go) by `make gen-bpf`,
// which dumps the host kernel BTF to a transient vmlinux.h, runs bpf2go,
// then removes vmlinux.h. The normal build and `make check` consume the
// committed output and need no clang/BTF. The generated *_bpfel.go is
// excluded from the coverage gate (scripts/cover-check.sh SKIP_PATHS).
package execsnoop

// bpf2go compiles execsnoop_bpf.c and generates the Go loader + struct
// types. -type event mirrors the C `struct event` into Go for reference;
// our own parseEvent decodes the wire bytes so the decode path is
// unit-testable without loading BPF. vmlinux.h is supplied via -I. by
// `make gen-bpf`.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type event execsnoop execsnoop_bpf.c -- -I.
