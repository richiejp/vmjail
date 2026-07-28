---
name: develop-daft-project
description: Deploy or reconnect a daft sandbox Node over SSH, create and push a local project Workspace through the trusted daft MCP server, start a sandboxed coding AgentRun with ephemeral credentials, and monitor it to completion or a diagnosed stuck state. Use when asked to develop, repair, test, or run a project path in darkaifactory on a specified user@node host.
---

# Develop a project in daft

Coordinate deployment and MCP operations; do not implement a monolithic
`project develop` operation. Atomically maintain a small durable handoff record
containing the Node fingerprint, Workspace UUID/generation, exact
`agent_start` inputs (request UUID, Workspace UUID, profile name, and prompt),
AgentRun UUID, and last event cursor. Keep it on the trusted host and never
record OAuth material.

## 1. Establish the Node

Invoke `$deploy-daft-node` with the SSH destination and expected release. Let
that skill probe the distribution and supervisor, install or update the static
`daft-sandboxd`, establish mutual trust, and verify live registration. Do not
assume systemd or accept an unexplained SSH host key or mTLS fingerprint.

Use MCP `node_registration_status` for the expected fingerprint after
deployment. Require both `authorized` and `connected`, the expected version,
and plausible resources before assigning work.

## 2. Declare and push the Workspace

Resolve the project path to an absolute trusted-host path. Require
`PROJECT/.daft/config.json`; inspect it and the project state before mutation.
Select an owner-scoped stable Workspace name and a target Node fingerprint.

Call `workspace_apply`:

- To create, pass `name`, `expected_generation: 0`, and the complete desired
  `spec`.
- To update, pass `uuid`, the last observed positive `expected_generation`,
  and the complete desired `spec`.
- On a generation conflict, read `workspace_status`, reconcile deliberately,
  and retry with that generation. Never overwrite unseen changes.

Ensure the spec has a usable image or devcontainer, the project mount target,
and `target_node_fingerprint`. Treat `created: false, changed: false` as a
successful retry. Record the returned UUID and generation.

Call `workspace_validate`; resolve every reported missing field. Call
`workspace_push` with the Workspace UUID and absolute project path, then call
`workspace_launch`. This order is required for a devcontainer because launch
resolves its configuration from the pushed workspace content. An exact retry
of apply, push, or launch is safe. Verify the push result names the expected
project path and nonempty mount entries before launching.

## 3. Start exactly one AgentRun

Call `agent_profile_list` and select an immutable profile by name. Explain any
material resource, egress, image, or credential-policy mismatch instead of
silently choosing another profile.

Generate one lowercase canonical UUID for `request_id` and persist it in the
handoff record before calling `agent_start`. Pass that same request UUID,
Workspace UUID, profile name, and exact prompt on every retry. A changed input
with the same request UUID is a conflict; generate a new request UUID only for
a genuinely new run.

Credentials are delivered from the trusted host only while the run is awaiting
them. Never copy OAuth tokens through SSH, store them in the handoff record,
place them in Workspace environment variables, or persist them on the Node.

## 4. Monitor and resume

Record the returned AgentRun UUID. Poll `agent_events` with `after_cursor` set
to the last recorded cursor and a bounded `limit` (normally 50). Persist
`next_cursor` after processing each page. Use `agent_status` for a compact
snapshot after reconnecting or when events are empty.

After an MCP or overseer restart:

1. Reconnect to the same trusted MCP configuration.
2. Query `agent_status` using the recorded AgentRun UUID.
3. Resume `agent_events` from the recorded cursor.
4. If the start response was lost before the AgentRun UUID was recorded, retry
   `agent_start` with the recorded request UUID and identical inputs.

Do not infer a stuck run from quiet output alone. Diagnose using durable state,
event progress, Node registration, and bounded observability. Report Node loss
or a durable `LOST`/`FAILED` state with its reason. Cancel with `agent_cancel`
only when the user requested cancellation or an agreed deadline/policy says to
stop; cancellation is idempotent.

## 5. Collect and close

When `terminal` is true, call `agent_result`. Report state, exit status, reason,
and a concise bounded result summary. The reconciler normally tears down the
run Sandbox and clears the Workspace binding; poll `workspace_status` to verify
that outcome. If cleanup is incomplete, call idempotent `workspace_stop` and
verify again.

Preserve the Workspace and project files unless the user explicitly requests
deletion. Report the Node, Workspace, request/run UUIDs, final cursor, result,
cleanup state, and any exact deployment rollback command.

Direct terminal or browser takeover is not part of this workflow yet. Do not
invent an attachment transport; preserve identifiers so a future attachment
surface can take over the same run.
