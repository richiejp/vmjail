// Package sleepsnoop is a Tier-2 CO-RE eBPF gadget (see OBSERVABILITY.md
// "Tier 2"). It runs inside the guest, attached by vmjail-agent, and reports
// every nanosleep(2) / clock_nanosleep(2) the workload makes — the caller's
// pid + comm and the requested sleep duration — so "who is sitting in a sleep
// loop" is answerable from a guest-side log. It is the strace-style companion
// to open-snoop and exec-snoop, aimed at the poll-with-sleep retry loops that
// make a build mysteriously slow without doing any real work.
//
// The BPF object is compiled from sleepsnoop_bpf.c into a committed ELF
// (sleepsnoop_bpfel.o) and loader (sleepsnoop_bpfel.go) by `make gen-bpf`,
// which dumps the host kernel BTF to a transient vmlinux.h, runs bpf2go, then
// removes vmlinux.h. The normal build and `make check` consume the committed
// output and need no clang/BTF. The generated *_bpfel.go is excluded from the
// coverage gate (scripts/cover-check.sh SKIP_PATHS).
package sleepsnoop

// bpf2go compiles sleepsnoop_bpf.c and generates the Go loader + struct types.
// -type event mirrors the C `struct event` into Go for the drift guard; our
// own parseEvent decodes the wire bytes so the decode path is unit-testable
// without loading BPF. vmlinux.h is supplied via -I. by `make gen-bpf`.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type event sleepsnoop sleepsnoop_bpf.c -- -I.
