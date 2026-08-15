# Reliability bug taxonomy

The issue sweep that introduced XFS Workspace disks exposed recurring classes,
not isolated mistakes. Use these names in reviews and regressions:

| Class | Failure found | Enforced guard |
|---|---|---|
| Intent mistaken for acknowledgment | Launch returned after queueing; stop cleared ownership before Node teardown | Exact UID/generation heartbeat waits with deadlines; bindings remain on timeout |
| Stale/ABA update | Older launch or pull state could act on newer Workspace data | Immutable launch generation and exact manifest digest/generation comparisons |
| Namespace/type confusion | A mount sync key was passed where a Workspace prefix was required | Distinct `WorkspaceID` type and exact-key tests |
| Partial-update drift | Local mount declarations and Node registration could diverge | Compensating rollback and failure-injection tests |
| Unbounded transactional fan-out | One manifest could exceed Badger transaction limits | Compact immutable bodies, small atomic pointers, 128-record batches, startup repair |
| Control-data ingestion | Recursive pushes included repository/control metadata | One shared `.git`/`.daft` exclusion policy tested at traversal boundaries |
| Evidence destruction | Terminal reconciliation cleared the only failure details | Durable bounded last-terminal record written before clearing the live binding |
| Unbounded blocking | SSH and subprocess calls could wait forever | Connect/keepalive/request deadlines plus the `commandcontext` and `forloopbound` analyzers |
| Nested deadline inversion | The CLI's 10-second timeout expired before the server's 30-second acknowledgment bound | Outer deadlines exceed inner deadlines and leave margin for one heartbeat |
| Completion/drain race | A process test called `Wait` before `StdoutPipe`/`StderrPipe` readers reached EOF | Drain both streams before `Wait`; assert captured data in failures |
| Accepted-work invisibility | Slow launch preparation was absent from heartbeats and falsely reconciled as lost | Publish a `Pending` liveness record atomically with accepting the launch |
| Diagnostic collapse | A real pre-readiness failure was returned as a generic termination after its binding was cleared | Match the durable terminal UID/generation and return its bounded phase, exit code, and message |
| Split authority | Client push updated CAS while a retained qcow2 remained the relaunch source, producing stale and empty mounts | Refuse running/protected disks; delete recoverable stopped disks before accepting a push |
| Acknowledged volatile state | Stop snapshotted readable page-cache contents, then killed the VM before XFS/qcow2 had made those writes durable | Require a bounded guest `syncfs` and verified protected CAS snapshot before acknowledging teardown; refuse stop on either failure |

The general invariant is: destructive or ownership-changing state transitions
follow exact external acknowledgment, preserve enough prior state for recovery,
and have a bounded failure path. Any intentional forever-running operation must
have a liveness signal and a reasoned static-analysis suppression.
