# Supervisor selection

Use the host's established supervisor and conventions.

- systemd: create a dedicated service with restart-on-failure, a persistent
  state/config directory, and the minimum device/capability access required.
  Check cgroup delegation explicitly; do not assume a user unit can access KVM.
- OpenRC: install an init script plus conf.d file, use its background/supervise
  mechanism, and set explicit user, state directory, and logs.
- runit or s6: create a service directory with a foreground `run` program and
  the supervisor's normal logging service.
- container init, cron-only hosts, or unknown PID 1: discuss the persistence
  requirement with the operator. A temporary `nohup` process is acceptable
  only for a labelled proof of concept, never as a claimed durable install.

Keep the command in the foreground under the supervisor. Never persist agent
OAuth tokens in the node service configuration; those are delivered only to a
specific future AgentRun.
