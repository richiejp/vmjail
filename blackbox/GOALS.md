# Exploratory blackbox goals

These goals describe outcomes, not procedures. A source-blind tester chooses
the public agent-facing interface unless a goal explicitly names a human
interface. Run variations by substituting another suitable Workspace fixture,
image, Node, project size, or failure point. Report the public actions taken,
bounded evidence, and any correctness, UX, or performance issue encountered.

## Workspace palette

- `workspaces/networked-go` — tests which contact a configurable HTTP endpoint.
- `workspaces/bun-package` — a small JavaScript dependency installation and
  test workload.
- `workspaces/http-service` — a long-running service with a health endpoint.
- `workspaces/mutable-tree` — nested client-owned files for synchronisation,
  mutation, adoption, and rejection.
- `workspaces/disk-writer` — a bounded configurable storage-growth workload.
- Any real project may replace a palette entry when its behaviour fits the
  goal. Record the project revision or immutable fixture digest.

## Goals

1. **Account for a test suite's network use.** Run project X's tests in a
   sandbox and determine whether it accesses the network, which endpoints it
   contacts, and how much data it transfers. Workspace: `networked-go`,
   `bun-package`, or a real project.

2. **Explain a denied network dependency.** Complete as much of project X's
   tests as possible with undeclared egress denied, and identify the dependency
   that prevents completion without weakening unrelated network policy.
   Workspace: `networked-go` or `bun-package`.

3. **Repeat a recorded build without external egress.** Record the permitted
   dependency traffic for project X, then reproduce the same build or tests
   without contacting the external network. Workspace: `bun-package` or a real
   dependency-heavy project.

4. **Review a dependency update as an untrusted workload.** Update one
   dependency, build and test the result, and report file changes, processes,
   network destinations, transferred bytes, and suspicious behaviour before
   adopting any output. Workspace: `bun-package`.

5. **Diagnose a silent workload.** Run a build or test which produces no output
   for long enough to require diagnosis, then determine whether it is working,
   sleeping, blocked on I/O, repeatedly failing an operation, or dead.
   Workspace: a real slow project or a controlled variation of `disk-writer`.

6. **Materialise an exact first push.** Start with a fresh Workspace and prove
   that the first accepted client manifest, including nested files, is exactly
   what the sandbox receives. Workspace: `mutable-tree`.

7. **Preserve sandbox changes across stop and relaunch.** Modify client-mounted
   data inside a sandbox, stop it, and prove that the unadopted work survives a
   later launch without being overwritten. Workspace: `mutable-tree`.

8. **Change mount mappings without stale content.** Add, remove, and replace a
   mapped tree, then establish that the next sandbox sees only the current
   declaration and current client content. Workspace: two copies or variations
   of `mutable-tree`.

9. **Adopt the exact sandbox result.** Produce sandbox changes, pull and
   materialise them on the client, acknowledge that exact generation, and show
   that newer sandbox writes remain protected. Workspace: `mutable-tree`.

10. **Reject unadopted work safely.** Produce sandbox changes, reject them while
    stopped, and prove the next launch contains the last client-owned manifest
    rather than the rejected files. Workspace: `mutable-tree`.

11. **Delete a changed Workspace predictably.** Delete a stopped Workspace
    containing unadopted changes and determine whether progress, completion,
    released storage, and subsequent absence are timely and unambiguous.
    Workspace: `mutable-tree` plus `disk-writer`.

12. **Keep a command-driven image usable as a Workspace.** Launch an image whose
    configured command exits quickly and establish that the default Workspace
    remains available for later work. Workspace: a short-command OCI image.

13. **Run an image deliberately to completion.** Preserve a short image command,
    observe its terminal result, and distinguish successful completion from a
    persistent Workspace unexpectedly dying. Workspace: a short-command OCI
    image.

14. **Reach the storage freeze boundary without losing work.** Grow a Workspace
    until its configured safety threshold freezes further growth, then account
    for usage, protected bytes, and the retained sandbox result. Workspace:
    `disk-writer`.

15. **Grow and recover a frozen Workspace.** Increase a frozen Workspace's disk
    allowance, establish that both the virtual disk and filesystem grew, and
    resume writes only after an explicit successful unfreeze. Workspace:
    `disk-writer`.

16. **Use a short-lived secret without retaining it.** Give project X only the
    credential needed for one operation and establish that it is unavailable
    from later runs, pulled Workspace files, logs, traces, and ordinary status
    output. Workspace: `networked-go` or `http-service` with a test credential.

17. **Reach a sandbox service through an explicit forward.** Start project X's
    service, reach its advertised endpoint from the client, account for the
    connection, and establish that unrelated guest ports remain unavailable.
    Workspace: `http-service`.

18. **Understand a Node loss during work.** Interrupt the selected Node while a
    launch or workload is active and determine, using public state and
    diagnostics, what was accepted, what was lost, and what remains safe to
    retry. Workspace: `http-service` or a long-running real project.

19. **Use the browser terminal as a human would.** In the dashboard, establish
    that the terminal is visible, focused, readable, correctly sized, accepts
    keyboard input, displays exact output and exit status, and reconnects
    without implying attachment to a detached AgentRun. Workspace:
    `mutable-tree` or another image with an interactive shell. Interface:
    browser dashboard only.

20. **Operate the dashboard through session and live-update boundaries.** Using
    the dashboard, unlock with each configured alternative, observe Node and
    Workspace changes without interval refresh, survive an ordinary page
    reload, and confirm that expiry/logout removes terminal and administrative
    access. Workspace: `http-service` plus a second inactive Workspace.
    Interface: browser dashboard only.
