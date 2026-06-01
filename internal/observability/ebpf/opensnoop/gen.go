// Package opensnoop is the first Tier-2 CO-RE eBPF gadget (see
// OBSERVABILITY.md "Tier 2"). It runs inside the guest, attached by
// daft-agent, and reports every openat(2) the workload makes — the
// diagnostic that would have turned a silent lint cache miss (an
// openat returning ENOENT on a path the test cared about) into one
// guest-side log line.
//
// The BPF object is compiled from opensnoop_bpf.c into a committed ELF
// (opensnoop_bpfel.o) and a committed loader (opensnoop_bpfel.go) by
// `make gen-bpf`, which dumps the host kernel BTF to a transient
// vmlinux.h, runs bpf2go, then removes vmlinux.h. The normal Go build
// and `make check` consume the committed output and need no clang/BTF.
// The generated *_bpfel.go is excluded from the coverage gate
// (scripts/cover-check.sh SKIP_PATHS), like internal/genpb.
package opensnoop

// bpf2go compiles opensnoop_bpf.c and generates the Go loader + struct
// types. -type event mirrors the C `struct event` into Go for reference;
// our own parseEvent decodes the wire bytes so the decode path is
// unit-testable without loading BPF. vmlinux.h is supplied via -I. by
// `make gen-bpf`.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type event opensnoop opensnoop_bpf.c -- -I.
