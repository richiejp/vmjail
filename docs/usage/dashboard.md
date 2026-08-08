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
or WebAuthn protection is configured. Sessions are held only in memory, expire
after 12 hours, and are invalidated by an orchestrator restart. Login and
logout require a matching browser Origin, and authentication decisions are
emitted as structured service-log events without passwords or session tokens.

## Security-key login

WebAuthn is an alternative to the password, not a second factor. Configure the
relying-party ID as the public dashboard domain and list every exact public
HTTPS origin accepted by browsers:

```sh
daft-orchestrator serve \
  --addr 127.0.0.1:14443 \
  --web-addr 127.0.0.1:18080 \
  --config-dir /path/to/orchestrator-config \
  --web-authn-rp-id dashboard.example.test \
  --web-authn-origin https://dashboard.example.test \
  --web-password-file /path/to/dashboard-password
```

The private HTTP listener may remain behind an HTTPS reverse proxy, but the RP
ID and origins must describe the browser-facing URL. After unlocking with the
password, visit `/security-keys` and register a key. Enrollment is never
available to an unauthenticated browser. Once at least one credential has been
stored, the password flag may be omitted and the key can unlock the dashboard
by itself.

Credential public keys and authenticator counters are stored atomically at
`<config-dir>/state/dashboard-webauthn.json` with mode `0600`. Back up this file
with the rest of the orchestrator state. Removing it removes every enrollment;
starting again then requires temporarily configuring a password. Registration
and login ceremonies are server-side, single-use, expire after five minutes,
and are bounded per client and process.

The NixOS module exposes the same settings as `webAuthnRPID` and
`webAuthnOrigins`. Keep the password configured until a security-key login has
been tested successfully.
