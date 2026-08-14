# Sandbox egress

When egress is enabled, darkaifactory routes both HTTP and HTTPS proxy traffic
through its inspected HTTPS lane. Cleartext HTTP is never sent upstream. A
plain `http://` request is upgraded to the same origin over HTTPS while
preserving its method, path, headers, and body.

Successful upgrades include `X-Daft-Egress-Upgrade: http-to-https` and appear
as `HTTPS-UPGRADE` in the bounded proxy log. If the origin does not support
HTTPS, the request fails with `502 Bad Gateway` and an explanation that
cleartext HTTP is denied and the automatic upgrade failed. Change the project
or repository configuration to a verified HTTPS origin; do not disable TLS
verification.

The guest receives the proxy address and trust anchor through standard proxy
and CA environment variables, and common system CA-bundle paths are replaced
inside the disposable container root. The interception CA remains stable for
the sandbox node, while per-origin certificates renew before expiry without a
guest restart.

Some runtimes use private trust stores rather than the operating-system CA
contract. Those runtimes must be configured to consume the supplied
`/etc/daft-egress-ca.pem`; darkaifactory should report an unsupported trust
store rather than suggesting an insecure transport flag.
