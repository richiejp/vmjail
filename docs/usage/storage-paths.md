# Local sandbox storage paths

The retained runtime separates persistent diagnostic state, rebuildable image
caches and temporary sockets. Names below use the `vmjail` prefix.

| Content | Current location or configuration |
| --- | --- |
| Host observer logs | `VMJAIL_STATE_DIR`; otherwise `$XDG_STATE_HOME/vmjail/sandboxes` or `~/.local/state/vmjail/sandboxes`, with a per-launch subdirectory. |
| OCI images and bundles | `VMJAIL_STORE_DIR`; otherwise `$XDG_CACHE_HOME/vmjail-sandboxd-images` using the platform user-cache fallback. |
| Embedded VMM, kernel, initramfs and virtiofsd | Sealed Linux memfds, inherited by child processes; no on-disk asset cache. |
| Workspace files | The host directory explicitly supplied as a virtiofs share. Writable direct shares persist changes there. |
| Guest observation output | A host directory exported to the guest, with guest log/summary paths set inside that share. Local-launcher wiring still needs extraction. |
| Standalone vsock sockets | A private temporary directory created by `sandbox.NewStandaloneVsock` and removed through its cleanup callback. |

Use absolute state/cache overrides. The current state resolver ignores relative
XDG state paths and falls back to the user's home directory. Direct mounts do
not create a separate workspace copy or a CAS snapshot. Optional guest overlays
change write persistence and must be selected explicitly.

The old deployment identity/catalog directories and remote storage flows are
no longer part of the target local interface. The launcher is still being
separated from those paths; see [the current plan](../../PLAN.md).
