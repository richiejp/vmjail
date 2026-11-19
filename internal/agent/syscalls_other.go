//go:build !linux

package agent

// defaultSyscallDumpDeps is disabled off Linux: the syscall-events tracefs
// subtree is a Linux-only kernel surface. vmjail-agent only ever runs in the
// Linux guest; this stub exists so the package still compiles cross-platform.
func defaultSyscallDumpDeps() syscallDumpDeps { return syscallDumpDeps{} }
