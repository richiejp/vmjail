# Scope reduction archive

These documents record vmjail's reduction from darkaifactory to a local VM
sandbox. They are historical evidence, not current implementation guidance.
Earlier reports include components, failing builds and proposed work that later
changes removed or resolved. Source paths and line numbers in audit inventories
refer to the audited revision; deleted source is available in Git history.

Current guidance: [plan](../../PLAN.md), [architecture](../../ARCHITECTURE.md),
[observability](../../OBSERVABILITY.md), [sandbox lifecycle](../architecture/sandbox-lifecycle.md) and
[storage paths](../usage/storage-paths.md).

## Reports and inventories

- [Scope reduction report](scope-reduction.md): original entry-point and dependency audit.
- [Conservative deletion report](conservative-deletions.md): initial proposed cuts.
- [Deletion results](deletion-results.md): intermediate removals and validation.
- [Progress history](progress-history.md): completed milestones and historical checks previously in PLAN.md.
- Function/test inventories: [sandboxd](scope-analysis/sandboxd.md),
  [client](scope-analysis/client.md), [guest](scope-analysis/guest.md),
  [packages](scope-analysis/packages.tsv), [references](scope-analysis/reference-edges.tsv),
  [tests](scope-analysis/tests.tsv). The analysis scripts are preserved alongside them.
- [Import errors](deletion-import-errors.tsv): historical dependencies left after the initial cuts.

## Deletion manifests

- [Initial components](deletion-candidates.tsv)
- [Client](client-deletions.txt)
- [Cluster tests](cluster-test-deletions.txt)
- [MITM proxy and HTTP cache](proxy-deletions.txt)
- [Unused distributed packages and protocols](unused-package-deletions.txt)
- [Node-control and health protocols](proto-deletions.txt)
- [Authentication and identity](auth-deletions.txt)
- [Private workspace disks](private-disk-deletions.txt), with [rationale and implementation notes](private-disk-removal.md)

The latest code removals are in commit `243623e`. Earlier major cuts are in
`cf1f895`; the sealed-memfd asset change is in `b121dd8`. Use the parent of the
relevant removal commit to inspect deleted implementations.
