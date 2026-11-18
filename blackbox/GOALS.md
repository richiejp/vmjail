# Local sandbox blackbox goals

These are test objectives for the local launcher once its integration is
complete. They do not claim a currently working CLI. Use the fixtures listed in
[README.md](README.md) or a reproducible project and image.

1. Run an OCI workload in a microVM, report completion/failure and clean up
   child processes after success, error or cancellation.
2. Read and write explicitly mounted host directories. Verify non-root
   container ownership and distinguish writable direct mounts from overlays.
3. Run a workload for longer than the smoke-test timeout without premature
   termination, while preserving explicit cancellation.
4. Diagnose a silent workload using host resource logs and guest process/IO
   summaries. Keep the evidence readable after the VM exits.
5. Account for network activity and verify the selected local egress policy.
   Proxy replay/cache behavior is conditional on retaining those features.
6. Exercise interactive execution and port forwarding if included in the local
   interface, including terminal resize, cancellation and connection teardown.
7. Confirm that only explicitly shared paths and supplied credentials become
   accessible to the workload. Verify that IO summaries are described as
   diagnostics, not a complete filesystem audit.

Remote registration, workspace transfer/adoption, control-plane recovery and
browser interaction are outside these goals.
