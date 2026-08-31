# Hardened HTTPS deployment

Quasar administrators are host administrators while the node agent can access the Docker socket.
Only trusted operators may hold the admin role. App images must be treated as executable host
policy, not as untrusted tenant input.

App **mounts** are the exception: the host, not the catalog, decides which of its paths a session
sees. A `runtime_spec.mounts` entry originates in an image manifest fetched from a remote catalog,
so the node agent refuses any host path outside the managed-home root and `QUASAR_APP_MOUNT_ALLOW`,
and refuses `/`, `/proc`, `/sys`, `/dev`, `/etc`, `/root`, `/run`, `/var/run`, `/var/lib/docker` and
any directory holding a container-runtime socket whatever the allowlist says — each of those is host
root for the tenant container. Allowlisted paths are read-only unless the entry ends `:rw`. Set
`QUASAR_APP_PRIVILEGE_OPTOUT=deny` as well when running a catalog you do not author: it makes the
host ignore a manifest's `no_new_privileges: false` and `systempaths_unconfined: true` (the shipped
Steam and KDE images need both, so it is not the default).

Set `QUASAR_PUBLIC_HOST`, `QUASAR_TLS_DIR` (containing `fullchain.pem` and `privkey.pem`), strong
database/enrollment/bootstrap secrets, and a short `AUTH_TOKEN_TTL`. Start the stack with:

```sh
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.hardened.yml up -d
```

The overlay removes the control-plane host port, exposes only TLS Caddy, restricts browser
signaling to the configured HTTPS origin, and adds HSTS. The application itself sends CSP,
frame-denial, content-type, referrer, permissions, and opener policies. Quasar ignores forwarding
headers for security/rate-limit identity unless the explicit trusted-proxy policy is configured.

That policy is `QUASAR_TRUSTED_PROXIES` (#438), and this overlay is the one deployment that needs
it. Every public request now arrives from the `quasar-edge` container, so with the variable unset
the login, signaling, agent-enrollment and first-run setup-claim rate limiters all key on that one
address and every client shares a single failure budget. The sharp end is `POST /v1/setup/claim`:
an unauthenticated attacker can burn the shared budget with bad tokens and keep the legitimate
operator from ever creating the first admin. Set it to the **/16 this compose project's own bridge
allocates** — read it off `docker network inspect <project>_default`, e.g. `172.18.0.0/16` — and each
client is limited on the address Caddy actually saw. Do not use the enclosing `172.16.0.0/12`: it
spans every Docker bridge on the box, including the default bridge and the host gateway. Leaving it unset is safe but coarse — the
header is never read, so nobody can spoof a limiter key. Never name a network you do not operate.
`deploy/Caddyfile.hardened` overwrites `X-Forwarded-For` with the address Caddy saw and strips any
inbound `X-Real-IP`, so a client-supplied chain cannot survive the edge. Full semantics:
`docs/configuration.md`.

Bearer tokens remain in browser local storage for the current SPA. Use short TTLs, do not run
untrusted scripts on the Quasar origin, and rotate tokens after suspected browser compromise. A
secure HttpOnly same-site cookie migration requires CSRF protection and is tracked as the preferred
long-term posture.

Sibling app containers run with all Linux capabilities dropped, `no-new-privileges`, a default
512 PID limit, and no network unless the app declares one (`runtime_spec.network` or its runtime
preset's `network`, both restricted to `none`/`bridge`) or `QUASAR_CONTAINER_NETWORK` sets a
host-wide fallback. Prefer the per-app declaration: it grants network to the one app that needs it
instead of every app on the host. **`host` networking is reachable only through
`QUASAR_CONTAINER_NETWORK`, never from an app, preset, or image manifest** — it removes the
container's network namespace (exposing host loopback services and allowing host port binds), and
app-side values are portable across machines, so that choice stays with the operator of the machine.
Set
`QUASAR_APP_READ_ONLY=1` after validating that each catalog image writes only to declared mounts;
the compatibility default leaves the image root writable. These controls constrain workloads but
do not turn Quasar admins into unprivileged tenants: the node agent still has the Docker socket,
GPU/input devices, and (for console KMS) narrowly granted host capabilities.

Validate before exposing the service:

```sh
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.hardened.yml config -q
curl --fail --cacert "$QUASAR_TLS_DIR/fullchain.pem" "https://$QUASAR_PUBLIC_HOST/health"
```

Confirm `Strict-Transport-Security`, `Content-Security-Policy`, and `X-Frame-Options` headers, then
launch a session from the HTTPS origin and confirm its signaling URL upgrades over WSS.
