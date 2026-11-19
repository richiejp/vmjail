// Package iosnoop is a Tier-2 CO-RE eBPF gadget (see OBSERVABILITY.md
// "Tier 2"). It runs inside the guest, attached by vmjail-agent, and reports the
// files, sockets, and pipes the workload reads and writes — per-fd byte and
// call counts resolved to a target via /proc/<pid>/fd — so "what is this build
// shovelling data through" is answerable from a guest-side log. It is the
// strace-style I/O companion to open-snoop, exec-snoop, and sleep-snoop.
//
// Unlike those three (a ring-buffer record per event), io-snoop aggregates in
// the kernel: read(2)/write(2) are the busiest syscalls in a build, so a
// record each would add the very per-syscall overhead the gadget exists to
// measure. Instead each call folds into a per-(pid,fd) LRU hash plus a
// monotonic per-CPU global; the Go drain iterates the map ~1 Hz, turns each
// (pid,fd) into a target name, and builds the summary. See iosnoop_bpf.c.
//
// The BPF object is compiled from iosnoop_bpf.c into a committed ELF
// (iosnoop_bpfel.o) and loader (iosnoop_bpfel.go) by `make gen-bpf`, which
// dumps the host kernel BTF to a transient vmlinux.h, runs bpf2go, then removes
// vmlinux.h. The normal build and `make check` consume the committed output and
// need no clang/BTF. The generated *_bpfel.go is excluded from the coverage
// gate (scripts/cover-check.sh SKIP_PATHS).
package iosnoop

// bpf2go compiles iosnoop_bpf.c and generates the Go loader + struct types.
// -type fdstat / -type iototals mirror the C ranking + totals structs into Go
// for the drift guard; our own decoders read the map values by fixed offset so
// the decode path is unit-testable without loading BPF. vmlinux.h is supplied
// via -I. by `make gen-bpf`.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type fdstat -type iototals iosnoop iosnoop_bpf.c -- -I.
