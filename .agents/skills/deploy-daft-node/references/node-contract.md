# daft node contract

The host must be Linux on the artifact's CPU architecture. Hardware sandboxing
requires `/dev/kvm` to exist and be readable/writable by the service account.
Do not silently replace KVM isolation with a weaker mode.

daft uses cgroup v2 for accounting and limits. A system service can normally
obtain delegation from its service manager. If `cgroup.controllers` is absent,
or neither the current cgroup nor an available manager can delegate a subtree,
report that resource enforcement is unavailable and stop before advertising a
production-ready node.

The daemon needs durable writable directories for its identity, peer store,
CAS, sandbox overlays, and runtime state. Preserve these across upgrades. The
static executable removes package-manager dependencies, but the selected VM
backend and host networking can still require kernel features and privileges.

The orchestrator URL must be reachable from the node. mTLS is mutual: the node
approves the orchestrator fingerprint and the orchestrator approves the node's
fingerprint with role `sandbox-host`. Fingerprints must come through an
independent trusted channel.
