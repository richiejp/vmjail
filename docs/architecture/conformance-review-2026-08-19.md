# Architecture conformance review — 2026-08-19

This review compares commit `6791e4f` across three independent kinds of
evidence: documented claims, implementation paths, and executed tests. None is
treated as intended merely because it exists. `MATCH` means a focused runtime
test exhibited the documented property; `PARTIAL` means only part of the claim
is observable; `DIVERGENCE` means a test exhibits different behaviour;
`UNTESTED` identifies an evidence gap.

## Results

| Area | Result | Evidence and consequence |
|---|---|---|
| Authentication, roles, startup validation | MATCH | TLS identity, stored-role enforcement, revocation, protocol validation, corrupt-store failure, and listener failure tests passed. |
| Controller readiness before serving | PARTIAL | Workers start immediately before HTTP serving, but have no positive readiness acknowledgement. The documented `ControllersReady` evidence is stronger than the program exposes. |
| Draining refuses mutations | PARTIAL | Shutdown has a five-second bound, but no draining admission state prevents a concurrent mutation. |
| Node reconnect inventory barrier | DIVERGENCE | Registration immediately makes a Node selectable; inventory arrives later by heartbeat. A reconnected Node can receive work before reconciliation. |
| Stale Node launch admission | DIVERGENCE | Reconciliation marks existing bindings unreachable, but launch selection uses registered stream presence without heartbeat freshness. A stale connected Node remains eligible. |
| Node disconnect propagation | DIVERGENCE | Stream teardown forgets the Node before the stale-node scan can find it, and does not directly reconcile bound Workspaces. A binding can remain stale indefinitely. |
| Client compensation and generations | MATCH/PARTIAL | Local/catalogue rollback, exact-generation materialisation, and bounded calls passed. `OutcomeUnknown` is architectural vocabulary, not an explicit API status. |
| Workspace launch and stop fencing | MATCH | Fresh UID/generation binding, concurrent-launch exclusion, exact-UID stop, preservation-before-delete, and stale terminal-update rejection passed. |
| Route teardown ordering | MATCH | Node executor tests observe routes and sessions closing before mount preservation and terminal publication. |
| Natural-exit preservation failure | PARTIAL | The backing tree is retained, but the failure is logged and `Exited` is still published without a durable structured preservation outcome. |
| Terminal evidence boundary | PARTIAL | The Orchestrator accepts an authenticated Node's terminal phase without protocol evidence that routes closed and preservation completed. Correct ordering is local Node behaviour, not an end-to-end invariant. |
| Per-VM resource limits | DIVERGENCE | Tests explicitly show cgroup parse/write failures are logged and launch continues, while the architecture says limits fail closed unless disabled. |
| Vsock network accounting | DIVERGENCE | The full concurrent check received an ARP reply but observed `TxBytes == 0`. Fifty isolated repetitions passed, so accounting evidence is timing-sensitive even when packet handling succeeds. |
| CAS protection and cleanup | MATCH | Focused tests passed for protected migration, weighted cleanup, hard-limit refusal, reference revalidation, shared chunks, and `NeedsRepush`. |
| Adoption, rejection, deletion | MATCH | Exact digest/generation fencing, stale rejection, baseline restoration, shared-reference protection, and stopped-disk deletion passed. |
| Snapshot publication | PARTIAL | The compact current pointer is atomically published, but staging and reference fan-out span bounded transactions with crash repair. “Transactional publication” is accurate only with that narrower meaning. |
| Freeze, grow, unfreeze control | MATCH | Focused tests passed for pressure ordering and qcow2 → VMM → guest-capacity barrier → XFS ordering, bounded retries, re-pause, and explicit unfreeze. |

## Executed evidence

Reviewers ran focused, uncached selections from `orchestrator`, `reconciler`,
`nodectrlclient`, `identity`, `mtls`, `cli`, `daftclient`, `workspacecatalog`,
`sandboxboot`, `sandboxexec`, `sandbox`, `casindex`, `workspace`,
`workspacedisk`, and `workspacesnapshot`; every selected test passed. The
selections covered the named behaviours above, including tests whose asserted
current behaviour diverges from the documents. The reviewed commit's full
pre-commit KVM gate also passed, but this review did not rerun a claim-mapped
real-KVM campaign.

A subsequent full `make check` failed once in
`TestStartVsockNetReceiverRoundTrip` with a zero transmit counter; the exact
test then passed 50 isolated repetitions. This is retained as intermittent
runtime evidence, not dismissed because it could not be reproduced alone.

## Remaining runtime evidence gaps

Real-KVM tests must be mapped explicitly to VM security-boundary enforcement,
guest attach/liveness, kernel-enforced cgroup limits, physical
qcow2/virtio-blk/XFS behaviour, and retained-XFS recovery after abrupt Node
loss. A complete control-plane crash/restart with protected Node state also
remains untested. These are gaps, not inferred defects.

The strongest common theme is that the architecture models evidence-bearing
nested states, while several implementation decisions still reduce state to
“registration exists” or “terminal phase received.”
