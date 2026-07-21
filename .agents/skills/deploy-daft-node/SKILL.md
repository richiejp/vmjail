---
name: deploy-daft-node
description: Install or update the single-file daft-sandboxd release on a Linux host over SSH, choose a suitable service supervisor, establish mutual trust with a daft orchestrator, and verify node registration. Use when asked to add, repair, upgrade, or reconnect a remote daft sandbox node; especially when the distribution or init system is not known in advance.
---

# Deploy daft Node

Install conservatively from observed host facts. The scripts provide small,
portable primitives; decide how to supervise the process after inspecting the
node instead of assuming systemd.

## Inputs

Obtain the SSH destination, orchestrator URL, local static `daft-sandboxd`
artifact, node config directory, orchestrator config directory, trusted client
config directory, and expected release version. Treat the SSH host key and both
mTLS fingerprints as security decisions: never accept or approve an unexplained
fingerprint automatically.

## Workflow

1. Inspect the local artifact with `scripts/inspect-artifact.sh PATH`. Compare
   its `runtime_architecture` with the normalized remote architecture from the
   probe (`x86_64`/`amd64` and `aarch64`/`arm64` are equivalent spellings).
   Stop if they are incompatible or `version` fails.
2. Run the read-only probe through SSH:

   ```sh
   ssh -T -- user@node 'sh -s' < scripts/probe-node.sh
   ```

   Pin the independently verified host key in a dedicated known-hosts file and
   use `StrictHostKeyChecking=yes`. Read `references/node-contract.md` when
   interpreting KVM, cgroup, privilege, and persistence results.
3. Select an install path writable by the deployment account, or an
   administrator-approved privileged path. Upload to a temporary file on the
   same filesystem as the destination. Do not overwrite a running executable
   in place.
4. Run `scripts/stage-release.sh TEMP DEST SHA256 VERSION`. It verifies the
   digest and executable before an atomic rename, retains one `.previous`
   rollback artifact, and restores it if post-install validation fails.
5. Choose the supervisor from observed facts. Prefer the host's established
   service manager. Read `references/supervision.md`; adapt it when the host
   uses another init system. The persistent command is:

   ```sh
   daft-sandboxd serve --orchestrator https://orchestrator.example:14443 --config-dir /var/lib/daft/config
   ```

   Give it a persistent config/home directory. Preserve its identity between
   upgrades. Do not place OAuth tokens or agent credentials in a service unit,
   command line, or persistent environment file.
6. Establish mutual trust out of band. On the node, obtain the sandbox-host
   fingerprint with `daft-sandboxd auth show --config-dir NODE_CONFIG` and
   approve the orchestrator with `daft-sandboxd auth approve ORCHESTRATOR_FP
   --role orchestrator --config-dir NODE_CONFIG`. Use the same config directory
   for the service. On the
   trusted orchestrator host, approve the node with
   `daft-orchestrator auth approve NODE_FP --role sandbox-host --label LABEL
   --config-dir ORCHESTRATOR_CONFIG`.
   Current releases load the orchestrator peer set at startup, so make the
   approval before starting or restart it after changing that store.
7. Start/restart the selected service and verify locally with
   `scripts/verify-node.sh SANDBOXD_PATH ORCHESTRATOR_URL EXPECTED_VERSION`.
   Then verify registration with MCP `node_list`, or with `daft node list --full
   --orchestrator URL --config-dir TRUSTED_CLIENT_CONFIG`. Require the exact
   fingerprint, expected version/resources, and a fresh last-seen value. A
   running process without orchestrator registration is failure.
8. Report the installed digest/version, supervisor and service location,
   fingerprints approved, registration result, and exact rollback command.

## Safety and recovery

- Probe before changing anything; show the user any privilege escalation.
- Keep all shell values as separate quoted arguments. Never construct a remote
  shell command by concatenating untrusted project paths or credentials.
- Make retries idempotent. A matching installed digest needs no replacement.
- Run probes and verification as the eventual service account. Explicitly
  resolve service-user creation, directory ownership, KVM membership/access,
  cgroup delegation, disk space, existing service state, and outbound
  connectivity before mutation.
- If startup or registration fails, inspect service logs and connectivity,
  then run `scripts/stage-release.sh --rollback DEST` when the new executable
  is the cause. Do not destroy node state or identities.
- Installation does not create projects, workspaces, sandboxes, or agent runs.
  Those are separate orchestrator/MCP operations.
