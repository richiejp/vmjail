# vmjail living plan

The original protected project goal below was mistakenly removed in commit
`cf1f895` and is restored verbatim. It records the inherited darkaifactory
goal; the separately labelled current vmjail scope reflects the agreed fork
direction.

## Project goal

**AGENTS DO NOT EDIT THIS SECTION** (Yes that includes you GPT Astra)

Primary Goal: Make running a single process inside a VM sandbox as easy as 
              running it inside a namespace sandox.
Secondary Goal: Expose advanced observability features of the guest kernel and 
                allow configuration of the guest kernel per the application 
                requirements.



## Current vmjail scope

Reduce the inherited project to a local Linux microVM sandbox containing an OCI
container. Keep direct host-directory mounts, resource limits, reliable process
cleanup, host resource accounting and guest IO/process observations. Remove the
distributed control plane, web UI and workspace CAS/replication.

[Architecture](ARCHITECTURE.md) describes the retained runtime.
[Observability](OBSERVABILITY.md) describes the diagnostic surfaces to preserve.
Git history retains the removed project plans and distributed architecture.

## Completed work

The distributed control plane, web/client interfaces, CAS, authentication and
private workspace disks have been removed. Host-directory mounts, container
rootfs cloning, VM/guest observability and sealed embedded assets remain.

Historical scope reports, function/test inventories, deletion manifests and
validation records are in [docs/archive](docs/archive/README.md). The archive
includes the rationale and removed capabilities for QCOW2/XFS workspace storage.
Active architecture, observability and storage-path guidance stays outside it.
The standalone reliability checklist has been folded into lifecycle guidance;
accounting, instrumentation readiness and diagnostic retention were already
covered in OBSERVABILITY.md.

The obsolete remote-node deployment and AgentRun skills, their supporting
files, the MCP agent configuration and the deployment-skill Make check have
been removed. A local-run skill is deferred until the CLI workflow is defined.

## Current validation

Commit `243623e` passed full `make verify`, including real VM tests, benchmarks
and all five fuzz targets. Coverage was 94.80% against the unchanged 94.43%
baseline; every gated file met 80%. The documentation archive reorganization
is checked separately for links and architecture documentation consistency.

## Next implementation work

1. Extract a local launcher from retained sandboxd/boot code. Keep namespace and
   cgroup preparation, modern bundle cloning and non-root mount translation.
2. Wire direct mounts into the foreground launcher and choose the correct
   workload lifetime runner; the retained debug command still uses sandbox.Hello.
3. Move guest observation-share setup and host-log defaults out of the remote
   executor. Read/tail local files with the retained CLI rendering behavior.
4. Reintroduce useful higher-level devcontainer/nested-VM assertions from git
   history against the local launcher; clean remaining Nix/release references.
5. Restore full Make/Nix/end-to-end verification, then rename module paths, commands and
   state/cache names to vmjail and tidy dependencies.

## Open scope choices

- Initial interface: a daemonless foreground CLI; defer detached/background VMs.
- Interactive execution and port forwarding.
- MITM interception and HTTP caching are removed. Generic network/proxy hooks
  remain to be reviewed with the launcher.
  Sandboxd no longer creates a proxy or injects its CA; the existing no-proxy
  network path permits direct egress when networking is enabled.
- Devcontainer builds and credential injection. Private scratch disks are
  removed; host-directory mounts are the storage model.
- Diagnostic IO summaries versus additional instrumentation for a complete
  filesystem audit. Existing guest summaries do not provide the latter.
