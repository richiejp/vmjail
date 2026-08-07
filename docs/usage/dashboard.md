# Orchestrator dashboard

The orchestrator can expose its dependency-free dashboard on a separate HTTP
listener. Bind that listener to loopback or a private interface and terminate
HTTPS at a trusted private reverse proxy; it does not share or weaken the mTLS
control-plane listener.

Password protection uses a bcrypt hash file, never a password on the command
line. Generate it from standard input and restrict the resulting file:

```sh
umask 077
daft-orchestrator dashboard password-hash > /path/to/dashboard-password
```

Then type or pipe the password on standard input and start the service with
`--web-password-file /path/to/dashboard-password`. The parameterised NixOS
module exposes `dashboardPasswordHashFile` and passes it through a systemd
credential, keeping the hash out of the Nix store and command line.

The health endpoint remains unauthenticated for service probes. All status,
detail, JSON, and event endpoints require an unlocked session when password
protection is configured. Sessions are held only in memory, expire after 12
hours, and are invalidated by an orchestrator restart. Login and logout require
a matching browser Origin, and authentication decisions are emitted as
structured service-log events without passwords or session tokens.
