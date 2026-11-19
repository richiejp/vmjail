// Package cgroupwrap guarantees the sandbox host runs inside a
// delegated cgroup v2 subtree by re-exec'ing itself under a
// `systemd-run` scope when its inherited cgroup isn't delegated.
//
// Per-VM accounting cgroups (one sub-cgroup per booted VM, holding
// cloud-hypervisor + every virtiofsd sidecar) require a writable,
// delegated parent. Rather than degrade to /proc-only sampling when
// the parent isn't delegated — which CONTRIBUTING "Probe preconditions
// at startup; no silent fallbacks" forbids for blast-radius/diagnostics
// infrastructure — vmjail *creates* the resource it needs: it wraps
// itself in a transient `systemd-run --scope -p Delegate=yes`, which
// hands back a freshly delegated subtree. If even that can't be
// obtained, startup fails loudly with a fix-it message instead of
// booting VMs it can't account for.
//
// The wrap is skipped when the process already sits in a delegated
// subtree (e.g. a parent already established a scope, or a production
// systemd unit set Delegate=yes), so it is a no-op in the common case
// and never disturbs an already-correct process tree.
package cgroupwrap

// EnvMarker is set on the re-exec'd process so a second EnsureDelegated
// call recognises it has already wrapped once and does not loop — if
// delegation is still unavailable past the marker, that is a hard
// failure, not another re-exec. Exported so tests (and tools that spawn
// sub-processes which should not re-enter the wrap) can set/inspect it.
const EnvMarker = "VMJAIL_IN_CGROUP_SCOPE"
