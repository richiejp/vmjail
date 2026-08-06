# Host storage and runtime paths

darkaifactory separates files by lifetime. Managed services should use their
service manager's state, cache, and runtime directories; per-user development
uses the XDG base directories. Explicit command-line and environment overrides
take precedence over defaults.

| Owner | Content | Class | Default |
| --- | --- | --- | --- |
| `daft` client | identity, approved peers, client preferences | configuration | `$XDG_CONFIG_HOME/daft/client` |
| orchestrator | identity, approved peers, catalogues, database | persistent state | explicit `--config-dir`; per-user fallback `$XDG_CONFIG_HOME/daft/orchestrator` |
| sandbox node | identity and approved peers | persistent state | explicit `--config-dir`; per-user fallback `$XDG_CONFIG_HOME/daft/sandboxd` |
| sandbox node | observer traces and workspace disks | persistent state | `DAFT_STATE_DIR`; fallback `$XDG_STATE_HOME/daft/sandboxes` |
| sandbox node | OCI images and assembled bundles | rebuildable cache | `DAFT_STORE_DIR`; fallback `$XDG_CACHE_HOME/daft-sandboxd-images` |
| sandbox node | embedded VMM, kernel, initramfs, and virtiofsd | rebuildable cache | `$XDG_CACHE_HOME/daft-sandboxd` |
| sandbox node | egress recording/replay objects | rebuildable cache | optional absolute `DAFT_EGRESS_PROXY_CACHE_DIR` |
| sandbox node | persistent egress interception CA | persistent identity | optional absolute `DAFT_EGRESS_PROXY_CA_DIR`; otherwise ephemeral per daemon run |
| sandbox node | hybrid-vsock Unix sockets | runtime | `--vsock-dir`; then `$RUNTIME_DIRECTORY/vsock`, `$XDG_RUNTIME_DIR/daft-vsock`, or a private temporary directory |
| local development cluster | identities, logs, and process metadata | persistent development state | `--state`; fallback `$XDG_STATE_HOME/daft/cluster` |
| guest | agent credentials, overlays, observation data, and workspace-disk mounts | VM runtime | fixed paths below `/run/daft`; destroyed with the VM |
| guest build container | build scratch data | temporary | RAM-backed `/tmp`; destroyed with the VM |

Daemon `--config-dir` values, `DAFT_STATE_DIR`, `DAFT_STORE_DIR`,
`DAFT_EGRESS_PROXY_CACHE_DIR`, `DAFT_EGRESS_PROXY_CA_DIR`, and `--vsock-dir`
must be absolute. Relative `XDG_*` values are ignored as required by the XDG
base directory specification. These explicit directory settings historically
accepted relative paths in some code paths; configure absolute paths before
upgrading if a deployment relied on that behavior.

The node removes daemon-owned temporary socket directories on clean shutdown.
Managed runtime directories remain service-manager owned, while each listener
removes its exact stale socket before binding. Startup rejects a runtime path
that cannot fit the longest hybrid-vsock name in Linux's Unix-socket path
limit.

Host-specific state locations belong in deployment configuration, not this
repository. The reusable NixOS orchestrator module therefore requires an
explicit persistent `configDir` and grants write access only to that path.
