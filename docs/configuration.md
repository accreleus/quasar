# Quasar — Configuration reference

Every runtime knob, as actually read by the code (control-plane Go `os.Getenv`,
node-agent Rust `env::var`). Defaults and accepted values are taken from the call
sites, not from memory — if you change a default in code, update the matching row
here.

Configuration is **environment variables only** (no config files). In a compose
deployment they live in `deploy/.env` and are wired to each service in
`deploy/docker-compose.yml` — the single base file every deployment uses, plus
whichever overlays that host needs (`docker-compose.nvidia.yml`,
`overlays/docker-compose.console.yml`, ...; see `deploy/README.md`). A few
are **compose-interpolation only** (consumed by compose, never read by the apps) —
those are in the last section.

> **Per-session vs. process config.** The node-agent's **stream** knobs
> (`QUASAR_WIDTH/HEIGHT/FPS/BITRATE_KBPS/H264_PROFILE`) and the `QUASAR_APP_*`
> launch knobs are **dev / standalone-path defaults**. On a real control-plane
> assignment the per-app catalog values (`default_width`, `runtime_spec`, …) and
> the negotiated tier override them — see [the launch-resolution note](#note-launch-resolution).
> The encoder/GPU/ABR/container knobs below apply to every session on that agent.

---

## Control plane

Read in `control-plane/internal/config/config.go` (and storage in
`internal/storage/storage.go`). `LISTEN_ADDR`/`LOG_LEVEL`/`AUTH_TOKEN_TTL` have
defaults; `DATABASE_URL` and `ENROLLMENT_TOKEN` are **required** (startup fails
without them).

| Variable | Default | Values / notes |
|---|---|---|
| `DATABASE_URL` | — (**required**) | Postgres DSN, `postgres://user:pass@host/db?sslmode=…`. |
| `QUASAR_DB_STATEMENT_TIMEOUT` | `30s` | Postgres `statement_timeout`, applied as a `RuntimeParams` on every connection in the pool (#416) — a lock pile-up or a runaway query now surfaces as a returned error instead of parking the caller (and eventually the whole 20-conn pool) forever. Any positive Go duration; a non-positive or unparseable value fails startup. |
| `QUASAR_DB_LOCK_TIMEOUT` | `10s` | Postgres `lock_timeout`, same mechanism as `QUASAR_DB_STATEMENT_TIMEOUT` above (#416) — bounds how long a statement waits to acquire a row/table lock (e.g. a `FOR UPDATE` behind another transaction) before erroring, rather than queuing indefinitely. Any positive Go duration; a non-positive or unparseable value fails startup. |
| `ENROLLMENT_TOKEN` | unset (optional since #12) | The fleet-wide static enrollment token. **Since #12 this is the fallback, not the primary path**: an admin mints per-host tokens in Admin → Fleet → Enroll host (`POST /v1/admin/hosts/enrollments` — hashed at rest, single-use, one-hour expiry, optionally bound to one `node_name`) and the agent presents either. Unset, the control plane boots with one WARN and only minted per-host tokens can enroll — the right end state once every host has joined. Existing agents that enrolled with it keep reconnecting with their node secret regardless. Keep it set on a single-host install (the local agent dials `ws://localhost` and enrolls with it on first boot). Treat it as a break-glass credential — it can enroll *any* node name, and (#96) it is refused only while the host it names has a live agent. |
| `QUASAR_IMAGE_REGISTRY_HOSTS` | `ghcr.io` | Comma-separated registry-host allowlist for the image-management digest resolver (P3): manifest HEADs and token-realm fetches are refused for any other host, which is the SSRF containment on catalog-supplied registry refs (enforced by the shared `internal/outbound` client since #105). The allowlist names the hosts actually contacted, so a Docker Hub ref needs `docker.io,registry-1.docker.io,auth.docker.io` — the ref's registry, its API endpoint, and its token realm. **Also gates catalog-supplied artwork URLs (#456):** a provider app's `cover_url` is accepted only when it is an `https` URL on one of these hosts; anything else (relative path, plain http, off-allowlist host) falls back to the shipped gradient tile. **The edge release channel reuses this list** for the platform component images, with `QUASAR_PLATFORM_REGISTRY` added to it automatically. **A registry that serves blobs by redirect needs its blob host on this list too:** reading an image's labels fetches a config blob, and GHCR answers that with a `307` to `pkg-containers.githubusercontent.com`, which is followed only if allowed. `ghcr.io` implies that host automatically; any other redirecting registry must have its blob host added here by hand, or edge detection fails with a refused redirect. |
| `QUASAR_LIBRARY_PROVIDERS` | `steam` | Comma-separated allowlist of `library_provider` names the P5 auto-ensure may install on a discovery enable. The local trust boundary on catalog-declared providers: a catalog image marking itself a provider outside this list is never auto-installed. Passed through the base compose file. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address; must contain a port. Agent-facing surface (`/agent/ws` enrollment, `/v1/agent/*`, `/health`) always serves here in plain HTTP. When the HTTPS listener is on (default), **browser-facing routes on this listener redirect to HTTPS** instead of being served — see `QUASAR_HTTP_REDIRECT`. |
| `QUASAR_TLS` | `auto` | HTTPS listener (#376). `auto` = serve the same handler over TLS on `QUASAR_TLS_ADDR`; `off` = HTTP only (prior behaviour). A misconfiguration (bad addr, port in use, unwritable cert dir) is a **fatal** startup error when not `off`. HTTPS gives the browser a secure context, required for in-game Esc capture (Keyboard Lock) and the Gamepad API. |
| `QUASAR_TLS_ADDR` | `:8443` | Address:port for the HTTPS listener. Must contain a port. Published to the host as `${QUASAR_TLS_PORT:-8443}` (nv stack: `18443`). |
| `QUASAR_TLS_DIR` | `/var/lib/quasar-control/tls` | Where a generated self-signed cert/key pair is persisted so an accepted browser exception survives restarts (regenerating every boot would re-prompt). Compose backs this with the `quasar-control-tls` named volume (`quasar-nv-control-tls` on the `nv` stack). In `auto` mode an **unwritable** dir is a clear fatal ("mount a writable volume"). Ignored when `QUASAR_TLS_CERT`/`_KEY` are set. **The pair is generated once and then reused for ~10 years, so SAN changes are not picked up.** Adding a name to `QUASAR_TLS_HOSTS` after first boot therefore does nothing until you re-issue: delete `cert.pem` + `key.pem` from this dir and recreate the container (`docker compose exec quasar-control-plane rm -f /var/lib/quasar-control/tls/{cert,key}.pem` then `docker compose up -d --force-recreate quasar-control-plane`). The new cert is a **new leaf with a new fingerprint**, so every client that already trusted the old one must accept/trust the new one again, including any OS/browser trust store the operator installed it into. `deploy/redeploy.sh` warns (never fails) when the served cert lacks a SAN that `QUASAR_TLS_HOSTS` lists, since re-issuing is the operator's call. |
| `QUASAR_TLS_CERT` / `QUASAR_TLS_KEY` | unset | Operator-provided PEM cert + key (set **both** or neither — setting one is a startup error). When both are set, the self-signed generator is bypassed and these are served as-is. For a browser-trusted cert this is one option; the hardened Caddy overlay (`deploy/docker-compose.hardened.yml`) is the other and remains the recommended real-cert / ACME path. |
| `QUASAR_TLS_HOSTS` | unset | Comma-separated SAN hostnames/IPs baked into the **self-signed** cert (e.g. `play.lan,192.0.2.10`). `localhost`, `127.0.0.1`, `::1`, `QUASAR_PUBLIC_HOST`, and the **control-plane process's own** non-loopback interface IPs are always included. **In a containerised deploy (the default compose stack) that last set is the container's bridge address (e.g. `172.21.0.3`), never the host's LAN IP**, whose addresses live in a namespace the process cannot enumerate. So **LAN access requires this variable**: put the host's LAN IP (and any hostname clients use) in it, or `https://<host-lan-ip>:8443` fails hostname validation with `ERR_CERT_COMMON_NAME_INVALID` permanently. That is a *name* mismatch rather than a trust failure, so trusting the cert does not clear the interstitial. `deploy/seed-tls-hosts.sh` seeds it from the host's primary LAN address and `deploy/redeploy.sh` runs that automatically; a plain `docker compose up -d` operator should run it (or set the variable) **before first boot**, because the cert is generated once and reused. See `QUASAR_TLS_DIR` for the re-issue/re-trust cost of adding names later. Ignored when an operator cert is provided. **Browser note:** with the default self-signed cert the first visit shows a one-time "not secure" warning; accept the exception and the page is then a secure context (`window.isSecureContext === true`). **Three ways out of this, all supported and none mandatory** (first-run wizard v2 §S6-0): (a) keep the self-signed cert and trust it — `GET /v1/tls/certificate.pem` serves it for download, and its SHA-256 fingerprint is logged at startup so the trust step can be verified out of band; (b) mount your own certificate at `QUASAR_TLS_CERT`/`QUASAR_TLS_KEY`; (c) put a reverse proxy (the bundled Caddy hardened overlay) in front — Quasar then keeps its self-signed cert as an internal detail, which is **correct**, and `GET /v1/admin/access-check` detects that topology and stops reporting the cert as a problem. |
| `QUASAR_PUBLIC_HOST` | unset | Single hostname/IP the control plane always folds into the **self-signed** cert's SAN list, alongside whatever `QUASAR_TLS_HOSTS` lists (see above — it is one of the names always included there). On the plain/`nv` compose stacks that is its only effect, and it is optional. The hardened Caddy overlay (`deploy/docker-compose.hardened.yml`) additionally **requires** it (`${QUASAR_PUBLIC_HOST:?Set QUASAR_PUBLIC_HOST}`) and interpolates it to derive `PUBLIC_BASE_URL`, `QUASAR_ALLOWED_ORIGINS`, and the value Caddy itself serves on — that overlay hard-fails to start without it. |
| `QUASAR_HTTP_REDIRECT` | `auto` | HTTP→HTTPS redirect on the plaintext listener. `auto` = when the HTTPS listener is on, browser-facing routes (SPA, `/v1/auth`, sessions, signaling) hit over plain HTTP get a **308** to the HTTPS port (method+body preserved), and plain-HTTP WebSocket upgrades get **426 Upgrade Required**; agent-facing routes (`/agent/ws`, `/v1/agent/*`, `/health`) are exempt and always serve. Rationale: plain HTTP is not a secure context, so the SPA's Keyboard Lock (in-game Esc) and Gamepad APIs silently fail — better to never serve browsers HTTP at all. Requests carrying `X-Forwarded-Proto: https` (hardened Caddy overlay) serve normally, so the redirect never loops behind a TLS-terminating proxy. `off` = serve everything on both listeners (dev escape hatch). No-op when `QUASAR_TLS=off`. |
| `QUASAR_TLS_REDIRECT_PORT` | port of `QUASAR_TLS_ADDR` | The **external** HTTPS port written into the redirect `Location` header. Needed when the host publishes the HTTPS listener on a different port than the container's (e.g. nv stack maps host `18443` → container `8443`; the compose files wire this to `${QUASAR_TLS_PORT}` automatically). `443` is omitted from the URL. |
| `QUASAR_COMPRESSION` | `true` | gzip response compression (#386). Applies to an allowlist of text types (HTML, CSS, JS, JSON, SVG, wasm) over 1400 bytes; already-compressed media (artwork PNG/JPEG/WebP, `woff2`) and WebSocket upgrades pass through untouched. Cuts the SPA's JS+CSS cold load ~3.2× (475 KB → 152 KB), which is what made the web UI usable over a VPN. Set `false` only when a compressing reverse proxy already sits in front (the hardened Caddy overlay does `encode zstd gzip`) — note the plain and `nv` compose overlays have **no** such proxy, which is why the default is on. The web client's bandwidth probe (`?probe=<ts>`) is always served identity-encoded so its throughput sample stays the size the tier gates were calibrated against (#146). |
| `QUASAR_ACCESS_LOG` | `errors` | `off` \| `errors` \| `all`, independent of `LOG_LEVEL` (#517). One `msg=http` line per matching request: method, path, status, `dur_ms`, `bytes_out` (compressed, on-the-wire), `enc`, `remote` (resolved by the `QUASAR_TRUSTED_PROXIES` policy — the direct peer unless a trusted proxy forwarded the request). `/health` is excluded so the compose healthcheck does not bury the log. `all` logs every request (the original #386 behavior, added because the web UI being unusable over a VPN once left **no** server-side evidence at all). `errors` logs only 5xx responses, preserving that forensic value while staying silent on routine traffic. `off` disables the access log completely. Default `errors`: the #517 readiness audit measured 60-120 routine lines/minute for a single active session from browser polling alone (two independent 1 s client polls plus admin polling), before any other traffic, drowning `docker logs` for a self-hoster — with no knob short of `LOG_LEVEL`, which also silences every lifecycle line an operator actually wants. Booleans are still accepted for backward compatibility: `true` maps to `all`, `false` maps to `off`. |
| `QUASAR_TRUSTED_PROXIES` | unset (empty) | Comma-separated CIDRs (or bare IPs, widened to `/32`/`/128`) naming the reverse proxies **you operate** (#438). This is the only thing that makes `X-Forwarded-For` believable: `internal/ratelimit.ClientIP` reads the header **if and only if** the direct peer falls inside one of these networks, then walks the chain **right-to-left** to the first address that is not itself trusted — everything further left is attacker-supplied and discarded. A malformed or empty chain from a trusted proxy falls back to the peer. `X-Real-IP` is never read (the bundled Caddy overlay does not send it and strips any inbound copy). Governs every per-IP limiter and per-IP log line: login/register (`internal/auth`), signaling (`internal/signal`), agent enrollment (`internal/agentws`), first-run setup claim (`internal/setup`), and the `QUASAR_ACCESS_LOG` `remote` field. **Empty is the default and is safe, not broken** — the header is simply never consulted, which is exactly right for a direct-LAN deployment where `RemoteAddr` already is the client. It is also what makes an *unconfigured* proxied deployment coarse rather than exploitable: every client shares one budget (on `POST /v1/setup/claim` that is a pre-auth lockout DoS — an attacker burns the budget with bad tokens and the operator can never create the first admin), but nobody can spoof a key. Set it when running the hardened Caddy overlay: name the **/16 the compose project's own bridge allocates**, read off `docker network inspect <project>_default` (e.g. `172.18.0.0/16`) — **not** the enclosing `172.16.0.0/12`, which spans every Docker bridge on the box including the default bridge and the host gateway and would trust unrelated stacks' containers as this deployment's edge. A `/0` in either family is **rejected at startup** (it would trust every address there is, which disables the limiters outright); anything shorter than `/8` boots with a `WARN`. **Never name a network you do not operate** — anyone on it can then mint unlimited rate-limit budgets. A malformed entry **fails startup**; silently dropping it would leave the deployment looking configured and sharing one budget with nothing in the log to say so. Every boot logs which of the two states the deployment is in. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `AUTH_TOKEN_TTL` | `24h` | Bearer-token lifetime; any positive Go duration (`30m`, `24h`, `168h`). |
| `BOOTSTRAP_ADMIN_EMAIL` | unset | First-admin bootstrap. Set **all three** to provision/promote an admin at boot when none exists (idempotent). Not "first to register wins". |
| `BOOTSTRAP_ADMIN_USERNAME` | unset | — |
| `BOOTSTRAP_ADMIN_PASSWORD` | unset | Subject to the same password policy as every other newly-set password (#513, NIST SP 800-63B style): **at least 12 characters** (128 max), rejected if it is on a small embedded list of common/breached passwords, and rejected if it equals or contains `BOOTSTRAP_ADMIN_USERNAME` or the local-part of `BOOTSTRAP_ADMIN_EMAIL` (case-insensitive). No composition rules (no forced symbols/uppercase/digits). A rejected value fails `EnsureBootstrapAdmin` with `ErrValidation` and creates nothing — the same rule enforced on `/v1/auth/register`, `/v1/setup/claim`, and `POST /v1/me/password`. Applies only to newly-set passwords; an existing stored hash is never re-validated. |
| `QUASAR_DEV_AGENT_AUTH` | `0` (off) | **Dev-only, never for production** (#399). `1` registers `POST /v1/dev/agent-session` — mints a throwaway, auto-reaped identity for automated UI-validation tooling (see `protocol/control-api.md` "Dev-only agent auth"). Absent the flag the route does not exist at all (404 from the mux, not a 403 guard). **The control plane refuses to boot** (`fatal`) when this is `1` and `QUASAR_ENV=production`. Every boot with the flag on emits a `WARN` startup banner naming it. A per-boot random secret (crypto/rand, ≥32 bytes hex) is generated at startup — never persisted across boots — and written to the container log **and** to `/run/quasar/dev-agent-key` (mode `0600`); callers authenticate with it via the `X-Quasar-Dev-Key` header (constant-time compare; wrong/missing key is a bare `401`, no message or timing distinction). `make agent-creds` (`scripts/dx/agentcreds.sh`) reads that file via `docker exec` for a local stack. `ttl_seconds` on the request defaults to `1800` (30 min) and is capped at `28800` (8 h); the token TTL is clamped to the identity TTL. The reaper that deletes expired identities runs **regardless of this flag**, so turning the flag back off never strands an identity that was minted while it was on; it deletes them through the same guarded path as `DELETE /v1/users/{id}`, so an expired identity that is still holding a live session survives until that session goes terminal. |
| `QUASAR_ENV` | unset (= non-production) | Names the deployment environment. Today it gates exactly one thing: `production` makes `QUASAR_DEV_AGENT_AUTH=1` a **fatal boot refusal** (#399) — the control plane exits before it serves a request, rather than running with a dev-only mint endpoint. Any other value (or unset) is treated as non-production. Case-insensitive. `deploy/docker-compose.hardened.yml` — the production TLS-ingress overlay — sets it to `production`, so a real deployment carries it by default; set it explicitly on any other production-shaped stack. |
| `QUASAR_DEV_AGENT_KEY_PATH` | `/run/quasar/dev-agent-key` | Where the per-boot dev-agent key (#399) is written (mode `0600`, directory `0700`). Only read when `QUASAR_DEV_AGENT_AUTH=1`. Override it when `/run` is not writable by the runtime user or is masked by a tmpfs mount; a write failure is **not** fatal — the control plane logs the error and keeps serving with the key available in the log only, but `make agent-creds`' `docker exec` fetch then stops working. |
| `QUASAR_SETUP_TOKEN_PATH` | `/run/quasar/setup-token` | Where the per-boot **first-run setup token** is written (mode `0600`, directory `0700`). The token gates `POST /v1/setup/claim`, the one call that creates the very first admin through the UI instead of the `BOOTSTRAP_ADMIN_*` env dance (`protocol/control-api.md` "First-run setup"). It is minted **only when no admin exists at boot** — a fresh 32-byte `crypto/rand` value, hex-encoded, written to **this file only, never the container log** (the boot log carries the file path and the per-boot rotation note, not the token: unlike the dev-only `QUASAR_DEV_AGENT_KEY_PATH` key, this token creates a production instance's first admin, and log aggregation exposes the WARN stream to principals without host access). Never persisted across boots (restarting before the claim rotates it, so an unclaimed token cannot outlive the process). Once any admin exists — env bootstrap or an earlier claim — **nothing is minted and this file is removed at boot**, so its presence is itself the signal that the instance is still unclaimed. Read it with `docker exec <control-plane> cat /run/quasar/setup-token`; anyone who can do that already has host access, which is the whole trust model. Override when `/run` is not writable by the runtime user or is masked by a tmpfs mount; the write is **mandatory** — because the file is the only place the token exists, a write failure fails the boot with a clear error instead of silently degrading. The token is never returned by any API response and never reaches the SPA. Claim rejections are constant-time, give no missing-vs-wrong distinction, and are rate-limited per source IP (10 failures / minute, bounded in-flight) so a public instance cannot be used to flood the operator's log. |
| `REGISTRATION_MODE` | `closed` | **First-boot seed only** (LP-SEC-01). Seeds `instance_settings.registration_mode` (`closed` \| `invite_only` \| `open`) once, on a fresh DB. `closed` = the invitation system is off; nobody self-registers until an admin turns it on in the UI (`PATCH /v1/admin/settings`). After the row exists this env is **ignored** — the persisted value is authoritative. An existing open-registration deployment keeps that behaviour only if the operator seeds `open` at the upgrade boot. |
| `PUBLIC_BASE_URL` | unset | Optional (LP-SEC-01). When set (e.g. `https://play.example.com`), the invite-mint response includes a ready-to-send magic link (`<base>/register?invite=<code>`). When unset the field is omitted and the admin UI composes the link from `window.location.origin`. **Also authoritative for the `signaling_url` handed to a PROXIED client.** A launch response tells the browser where to open the signaling WebSocket; that address is normally derived from the request's `Host`. A reverse proxy that rewrites `Host` to its upstream address (the nginx default, and easy to end up with in Caddy) therefore makes the control plane advertise its own private listener — the client leaves the proxy, dials that listener directly, and behind a self-signed cert or from outside the network never connects. The page loads while signaling silently never starts, which reads as a launch that hangs forever rather than as a proxy misconfiguration. Set this to the URL users actually type and a Host-rewriting proxy can no longer produce an unreachable signaling address; only the scheme and host are used, so the invite-link path may keep its own. Resolution order for a proxied request is `PUBLIC_BASE_URL` → `X-Forwarded-Host` → `Host`. **A request carrying no `X-Forwarded-*` header is not proxied and keeps its own `Host` regardless**, so setting this for invite links cannot break direct LAN access on a deployment whose public name does not resolve internally. |
| `QUASAR_MIN_CLIENT_VERSION` | unset | Native-client hard floor (P9-08 / #236). Strict `MAJOR.MINOR.PATCH` semver (a `v` prefix is tolerated). When set, a native client sending a `client_version` below it at `POST /v1/auth/login` is rejected with **426 `client_too_old`** before authenticating; a value the server cannot parse as ≥ floor is also rejected (it cannot be proven current). A client that sends **no** version (every web / Phase-1 client) always logs in — the additive baseline is preserved. Echoed to clients as `min_client_version`. Unset = permissive (no floor advertised, no gating). An invalid non-empty value fails startup. Restart to change. **#380 extends the same floor past login onto every bearer-authenticated endpoint** — a client may send `X-Quasar-Client-Version: <semver>` on any authenticated request, and the shared `RequireAuth` middleware answers the identical **426 `client_too_old`** below the floor, so raising the floor now takes effect on clients holding a cached token instead of waiting for that token to expire. Absent header = no gate (web/legacy, unchanged). A **malformed** header value is treated as absent and logged once per distinct value, deliberately unlike the login body (which gates it): the header rides every request, so gating a typo would brick an already-signed-in client on every call it makes, and the gate is cooperative anyway — omitting the header evades it just as effectively. |
| `QUASAR_LATEST_CLIENT_VERSION` | unset | Native-client advisory "latest available" version (P9-08 / #236). Strict `MAJOR.MINOR.PATCH` semver. Echoed to clients as `latest_client_version`; a client below it shows a **non-blocking** "update available" soft-warn — the server never blocks on it (soft-warn is client-side). Unset = field omitted. An invalid non-empty value fails startup. Restart to change. |
| `QUASAR_ALLOWED_ORIGINS` | unset | Comma-separated browser origins allowed to open `/v1/signal`, e.g. `https://play.example.com,http://admin.lan:8080`. Each entry must be an exact `http(s)://host[:port]` origin: no path, query, fragment, credentials, or other scheme; an invalid non-empty entry fails control-plane startup without echoing its value. **Since migration 0064 this is an OVERRIDE, not the only source** (first-run wizard v2 §S6e): the allow-list is now an admin-editable column (`instance_settings.allowed_origins`, `PATCH /v1/admin/settings`), and this variable — **when set, including to the empty string** — wins outright and the column is not consulted. Setting it to `""` is therefore the way to pin the list off from the environment. **Leave it unset to manage the list from the admin UI**; existing deployments that set it keep their exact current behaviour. `GET /v1/admin/access-check` reports which source is in force. When neither is configured, only same-origin browser requests are allowed — which is not "deny all", and is why a plain LAN install works with nothing set. Missing `Origin` remains allowed for browserless tooling and still requires a valid single-use signaling token. |
| `QUASAR_ICE_SERVERS` | unset (no ICE servers) | **EXPERIMENTAL, UNSUPPORTED, OFF BY DEFAULT.** ICE servers handed to the browser (#509), as a JSON array of W3C `RTCIceServer` objects, served on `signaling.ice_servers` with every launch and reconnect. **Unset or `[]` is the default and the only supported configuration**: host candidates only, which is what a shared LAN or a VPN joining the two ends needs, and what Quasar did before this knob existed. Nobody gets a relay without setting it explicitly. It is listed here because the variable exists in the code and is read at startup, not because relaying is a capability Quasar offers or supports: it has never been operated outside our own LAN, Quasar ships no TURN server, and the single static `username`/`credential` pair it carries reaches every user who launches a session. A malformed value, a non-ICE scheme, a `turn:` entry missing credentials, or a `stun:` entry carrying them all fail the boot with a message naming the problem, because the failure this validation prevents is otherwise silent. Startup logs the resolved list with credentials redacted, or says none is configured. |
| `QUASAR_WEB_ROOT` | unset | When set, serve the built SPA from this dir for non-API paths (same-origin deploy). Compose points it at `web/dist` mounted as `/app/web`. |
| `QUASAR_PLACEMENT_POLICY` | spread | `""` \| `spread` \| `least-loaded` → spread (default); `locality` → prefer the host holding the user's home (P3-02/P5-07). Unknown value = startup error. |
| `QUASAR_VRAM_MIN_FREE_MB` | `1024` | Live free-VRAM admission floor (#383). A GPU accepts a new session only if its most recent agent-reported free-VRAM sample, debited for launches the sample cannot see yet, is at least this. **Advisory, not a reservation** — encode slots remain the race-safe reservation; this only refuses a GPU that is *already* out of memory. `0` disables the veto entirely (slots-only admission) and is the kill switch. Fails **open** in every unknown case: no sample, stale sample, or a card whose `vram_mb_total` is ≤ this floor (an AMD APU's UMA carve-out). A veto rejection is a retryable `503 capacity_exhausted` and logs the GPU plus every number it judged on. Note a floor above a card's total does **not** make that card unusable: the veto abstains, the card stays servable, and any rejection there is ordinary encode-slot exhaustion. |
| `QUASAR_VRAM_INFLIGHT_ESTIMATE_MB` | `QUASAR_VRAM_MIN_FREE_MB` | Per-session debit for sessions admitted too recently for the current sample to show their allocation (#383). Split from the floor so raising the floor for safety does not also multiply the burst debit. Applied to sessions in `{assigned, starting, running, stopping}` whose `started_at` is null or newer than the sample minus one staleness window. |
| `QUASAR_VRAM_STALENESS_SECS` | `20` | How old a VRAM sample may be and still be acted on (#383) — 4× the 5 s heartbeat, matching the agent read-deadline convention. Also the grace margin on the in-flight debit. Beyond this the veto abstains and encode slots decide alone. Must be ≥ 1; an invalid value fails startup. |
| `QUASAR_STORAGE_PROVIDER` | auto | **First-boot seed / fallback only** (storage-config). `auto` \| `local`. Seeds `instance_settings.storage_provider` semantics but is now a *fallback*: the authoritative value is the admin setting via `PATCH /v1/admin/settings` (read fresh at every launch). **THE PER-HOST STORAGE ROOT IS THE CONTROL** (operator decision 2026-08-10): `auto` and `local` are the SAME behaviour — managed homes go under the session host's effective home root, and **no root is a loud launch error naming the host and the remedy**, never a silent fallback. **`volume` (the docker-volume driver) was HARD-REMOVED (#473, operator direction 2026-08-25)** — Quasar was unreleased at the time, so this is a clean removal with no back-compat owed to an existing volume-backed home: `PATCH /v1/admin/settings` now rejects `storage_provider=volume` with "the docker-volume home driver was removed; use a mount path (QUASAR_HOME_ROOT)", and migration `0068` coerced any instance still on it to `local`. Unknown (including `volume`) = startup error / 400. The admin UI has **no storage-provider picker** (its two remaining options resolved identically before the removal, and its third is gone); the Storage page states the driver in effect and points at Admin → Hosts. `deploy/redeploy.sh` seeds `QUASAR_HOME_ROOT` on a **fresh** install so `auto` resolves to `local`. (History: the wire enum `protocol/openapi.yaml`/`schema.md` still lists `volume` as a schema value — a frozen contract this control-plane-only removal did not touch — but no runtime path accepts or produces it any more; migration `0065`'s volume-fallback pin predates this removal and is superseded by it.) |
| `QUASAR_HOME_ROOT` | unset | Host directory that holds managed homes (`{root}/{user}/{app}`). **This env is now the lowest-priority fallback.** The control plane resolves a session host's *effective* home root per launch: the per-host `home_root` knob override (admin, `PATCH /v1/admin/config/hosts/{id}`) → the host's last-reported `effective_settings.home_root` (the agent's own `QUASAR_HOME_ROOT`) → this control-plane env → empty. Different hosts may therefore use different roots (v1's uniform-root constraint is relaxed by the per-host knob). Absolute path; must exist (or be creatable by docker) on the node-agent host. Any persistent path works. Recommended per-OS: unraid `/mnt/cache/appdata/quasar/homes` (bypasses the `/mnt/user` FUSE overlay; on unraid-style appliance OSes `/var/lib` is RAM-backed and lost on reboot, so use the persistent data share), generic Linux e.g. `/var/lib/quasar/homes`. **Fresh installs are seeded** with `/var/lib/quasar/homes` by `deploy/redeploy.sh` (see `QUASAR_STORAGE_PROVIDER` above). Since 2026-08-10, a host that ends this resolution with an empty root **cannot run managed-home apps at all** — the launch fails naming the host and pointing at Admin → Hosts. **Mount-path (local) storage is the only driver as of #473 (2026-08-25) — there is no fallback docker-volume driver to fall back to at all any more.** |
| `QUASAR_ARTWORK_DIR` | `/var/lib/quasar-control/artwork` | UI-P7 cover-artwork cache root. Fetched/uploaded images are stored here **content-addressed** (`<sha256>.<jpg\|png\|webp>`, sharded by the first two hex characters) and served at `/v1/artwork/<name>`. Deliberately **not** inside `web/dist`: that is a bind mount rebuilt from scratch on deploy, and a `rm -rf dist` swaps the directory inode (see CLAUDE.md #131), which would delete every cached image on every web deploy. Back it with a persistent volume — art is meant to survive a redeploy. If the directory is unwritable, startup **still succeeds**: artwork is disabled with a warning, the artwork routes answer `503`, and the library renders its gradient tiles. |
| `QUASAR_ARTWORK_PROVIDER` | `steamgriddb` | `steamgriddb` \| `none`. `none` disables the third-party lookup even if a key is set — **including one set from the admin UI**, so an operator who switched the provider off does not get it switched back on by someone typing a key in. An unknown value **fails startup** rather than silently leaving the feature dark, so an operator who set a key and mistyped this gets told. |
| `QUASAR_STEAMGRIDDB_API_KEY` | unset | **The FALLBACK source for the artwork provider credential** — still fully supported, no longer the only one. The key can now also be set from the **admin UI** (app editor → Artwork), which stores it encrypted (see *Encrypted secrets* below); a stored key **takes precedence** over this variable, and clearing it in the UI falls back here. An existing deployment that set this keeps working untouched. With **neither** source set (the default) the feature is SHIP DARK: no outbound request is ever made, no artwork row is written for a game, and every app keeps `cover_url`/`hero_url` `NULL` — the gradient tile, byte-for-byte the pre-UI-P7 behaviour; the admin API reports `provider_configured: false` and the provider-backed routes answer `409`, which is a documented state, not an error. The admin UI shows which source is in effect (`provider_origin`), so neither direction is silent. **Read the terms before setting this** — see the note below. The value is never logged. |
| `QUASAR_ARTWORK_MAX_BYTES` | `8388608` (8 MiB) | Cap on a single fetched **or** uploaded image. Enforced by the server's own reader, not by a client-declared `Content-Length`. Box art and hero banners are a few hundred KB, so the default is generous. |
| `QUASAR_ARTWORK_SWEEP_INTERVAL` | `15m` | How often the background resolver looks for apps with no artwork record. Any positive Go duration; an invalid value fails startup. The sweeper now runs whenever the interval is positive and resolves the credential on each tick — so a key set from the admin UI is picked up **without a restart** — but a tick with no credential returns as soon as it has looked for one, so an unconfigured deployment makes **no third-party request, queries no apps and writes no rows**. Looking for the credential does read the `instance_secrets` row, so the tick is not entirely free of database work — one indexed lookup per interval is what buys a key set in the admin UI taking effect without a restart. The sweep only ever considers apps with **no record at all**, so resolved art, a cached "no match", and an admin override are each looked at exactly once — art is cached, never re-fetched at browse time. |
| `QUASAR_LIBRARY_SCAN_INTERVAL` | unset (env), `360` minutes / `6h` (database default) | Steam library discovery: how often the control-plane janitor enqueues a scan per (user, provider app, host). **Admin-libraries amendment (2026-08-01): this is now an OVERRIDE, not a default.** The operator-UI path is `instance_settings.library_discovery_interval_minutes` (bounds 15..10080 minutes, editable via `PATCH /v1/admin/settings`, admin UI: Libraries → Steam → Discovery settings). When this env var is **set**, it wins over the database column — read once at boot, since an env var cannot change without a process restart. Any **non-negative** Go duration is accepted when set. **`0` disables discovery entirely, regardless of the database column or the `library_discovery_enabled` master switch** — the kill switch, unchanged from before this amendment, so an operator can guarantee a control plane makes no scan and no third-party call without needing database access. An invalid or negative value **fails startup**. When **unset** (the recommended path for a normal deployment), the database column is read **per pass**, so an admin's edit in the UI takes effect on the very next pass with no restart — the same guarantee the master switch already makes. `GET /v1/admin/library/status` reports the **resolved** interval (`scan_interval_secs`) and `interval_overridden_by_env` (true when this env var is the reason), so an operator can always see which source is actually in effect and the UI can grey a control the environment has pinned. Note the interval is only half the gate: the master switch is `library_discovery_enabled` on the `instance_settings` singleton (default **false**, edited via `PATCH /v1/admin/settings`), read **per pass**. Discovery is also inert when no registered host has a managed-home storage root — there is no host path for the node-agent to walk — and `GET /v1/admin/library/status` reports which of these reasons applies, because with auto-publish "nothing appeared" and "nothing ran" look identical otherwise. (The docker-volume driver this inertness used to also name explicitly was hard-removed, #473, 2026-08-25 — it can no longer be configured at all, so the rootless case is now the only one.) |
| `QUASAR_STEAM_APPDETAILS_LOOKUP` | unset (env), `false` (database default) | Opt-in supplement to the built-in Steam denylist. When resolved true, the reconciler consults Valve's undocumented store `appdetails` endpoint for the appids the denylist ladder **would publish**, and suppresses any whose `type` is not `game`. **This discloses to a third party exactly which Steam appids this instance has installed** — the same privacy class as artwork hotlinking, which UI-P7 explicitly rejected — which is why it stays off by default and why turning it on is an operator's decision, never a default. **Admin-libraries amendment (2026-08-01): this is now an OVERRIDE, not a default.** The operator-UI path is `instance_settings.library_discovery_appdetails_enabled`, editable via `PATCH /v1/admin/settings` (admin UI: Libraries → Steam → Discovery settings). When this env var is **set**, it wins over the database column — a privacy-hardened deployment can pin it off (or on) in the environment regardless of what an admin later sets in the UI. When **unset**, the database column is read **per scan report** — never cached — so enabling it in the UI takes effect on the very next report with no restart. `GET /v1/admin/library/status` reports the **resolved** value (`appdetails_lookup`) and `appdetails_overridden_by_env`, same UI treatment as the interval above. It is a *supplement*, never a replacement: it is appended below the denylist ladder, so it can never override an operator-written `allow`/`ignore` rule, and a Valve outage, a rate-limit or an unrecognised appid degrades to "the denylist alone decided" — which is the default behaviour anyway. Bounded at 40 lookups per scan. Accepts `1/0`, `true/false`; anything else **fails startup**. |
| `QUASAR_SECRET_KEY` | unset | **Master key for the encrypted-secrets facility.** Base64 of exactly **32 bytes** (`openssl rand -base64 32`), optionally prefixed `"<version>:"` (e.g. `2:<base64>`) — a bare value is version 1. Standard or URL-safe base64, padded or not. **Unset is a supported state and the default:** the control plane boots normally, everything unrelated works, and secret-backed features report themselves unavailable in the admin UI (`master_key_configured: false`). It is deliberately **never generated and persisted** on first boot — a generated key would diverge across a multi-node deployment and make a database backup unrestorable without the node that happened to invent it. A **malformed** value (not base64, wrong length, bad version prefix) **fails startup**, so a truncated paste is caught at boot rather than when a secret you already saved turns out to be unreadable. Never logged; never returned by any endpoint. **Back it up with your deployment secrets — see the warning below.** |
| `QUASAR_SECRET_KEY_PREVIOUS` | unset | Comma-separated **decrypt-only** predecessor keys, each `"<version>:<base64>"`. Exists so key rotation is possible: give the new key a higher version in `QUASAR_SECRET_KEY`, keep the old one here, and rows written under the old key keep decrypting while new writes use the new one. Setting this **without** `QUASAR_SECRET_KEY` fails startup (a decrypt-only key alone cannot store anything). A duplicate version fails startup. |
| `QUASAR_PPROF_ADDR` | `127.0.0.1:6060` | **Operator debug/profiling listener (PROF-01, #388) — ships ENABLED in production.** A second `http.Server`, separate from the application mux, serving `net/http/pprof` plus the Quasar-specific endpoints below. The **loopback default is the security control**: the port is published in no compose file, so the surface is reachable only from a shell on the host (`docker exec`) or a container sharing the namespace. Must contain a port; a portless value **fails startup** rather than silently leaving profiling off. Set to `off` (or explicitly to the empty string) to run no debug listener at all — note this is the one control-plane knob where set-but-empty means "off" rather than "use the default". A bind failure is **not** fatal: it is logged at `error` and the control plane serves on. Full surface and access recipe below. |
| `GOMEMLIMIT` | `1GiB` (compose) | Go runtime soft heap ceiling. Above it the GC works harder instead of letting RSS drift up toward a container memory cap, which is the failure mode the profiling campaign exists to catch. **Soft** — exceeding it costs CPU, it never causes an OOM kill and it is not a quota. Not read by `config.go`; it is a runtime variable set in the compose files, so raise it there if a deployment legitimately needs a bigger heap. |
| `QUASAR_IMAGE_CATALOG_REPO` | `accreleus/quasar-images` | App-image catalog P1 (image-management spec). `owner/name` of the GitHub repo `POST /v1/admin/images/sync` fetches the manifest from, via `raw.githubusercontent.com/<repo>/<ref>/<path>`. `<ref>` is the separate, admin-editable `instance_settings.image_catalog_ref` (`PATCH`-able, default `main`) — this env is the deploy-time "which repo" the ref is resolved against, for an operator running a private fork or mirror. Read once at boot (`internal/images.NewHTTPFetcher`). |
| `QUASAR_IMAGE_CATALOG_PATH` | `quasar-manifest.json` | The manifest's path within `QUASAR_IMAGE_CATALOG_REPO`. Override when a fork renames or relocates the file. **A fetch that 404s** (wrong ref, wrong path, or the manifest not yet published at the pinned ref) never fails a launch — the cached catalog keeps serving and the error surfaces as `sync_error` in the `GET /v1/admin/images` response, with the exact URL attempted logged at `WARN`. |
| `TEST_DATABASE_URL` | unset | DB integration tests only — they `t.Skip()` unless set (see `scripts/dev/dev.sh go-test-db`). |
| `QUASAR_JOBS` | `1` | Background-jobs framework master switch. `0` stops the dispatcher entirely: adopted jobs then do not run at all (they no longer have their own tickers), the admin Jobs page reports `scheduler_disabled`, and nothing is scheduled on any host. This is an operator kill switch for a misbehaving job framework, not a way to run jobs the old way — use per-job `enabled` to stop a single job. |
| `QUASAR_JOBS_TICK_SECS` | `10` | How often the dispatcher materializes due runs, claims control-plane runs, reaps abandoned claims and prunes history. Lower bounds manual-trigger latency for control-plane jobs; agent-plane latency is bounded by `QUASAR_JOB_POLL_SECS` on the agent instead. Any positive integer; an invalid value fails startup. |
| `QUASAR_JOBS_TIMEZONE` | `UTC` | IANA timezone used as the seed default for a newly registered job's run window (`jobs.timezone`). Per-job values are admin-editable afterwards and this variable is never re-applied to an existing row. Set it to the site's zone so that "02:00-06:00" means what an operator means by it. An unknown zone name fails startup. |
| `QUASAR_JOBS_RUN_RETENTION` | `50` | Per-job run-history cap. Rows beyond this per (job, host) are pruned by the dispatcher. Also the per-job default for `jobs.history_limit`, which an admin may raise to 500 for a job under investigation. |
| `QUASAR_JOBS_RUN_RETENTION_DAYS` | `30` | Age cap on run history, applied in addition to the row cap: a run older than this is pruned even if the job is below its row cap. `0` disables the age rule. |
| `QUASAR_JOBS_CLAIM_TIMEOUT_SECS` | `3600` | How long a claimed (running) run may go without a report before the dispatcher marks it `aborted` and re-materializes. Covers an agent that died mid-run. Must exceed the longest expected job; the #488 warm-up's own bound is `QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS` (600), so the default has 6× headroom. |
| `QUASAR_TELEMETRY_ROLLING_WINDOW` | `1h` | **Session-telemetry retention, rule 1.** While a session is **non-terminal**, its `session_metrics` samples and `session_trace_events` older than this are swept — the bound on a long-lived session. Any positive Go duration; `0`, a negative value, or an unparseable one **fails startup** (unbounded telemetry is not an option an operator gets by typo). Measured against the server-side `created_at` (the ingestion clock), never a reporter's `ts_unix_ms`, so a skewed reporter clock cannot evade the cap. Distinct from the *read* window an admin trace/bundle request asks for (2-10 min, `contract-amendment.md`): that is what a caller wants to see, this is what the server still has. |
| `QUASAR_TELEMETRY_POSTMORTEM_RETENTION` | `24h` | **Session-telemetry retention, rule 2.** When a session reaches a terminal state its telemetry is **frozen, not deleted** — the rolling window stops being applied to it — and kept for this long so `GET .../verdict` and `GET .../diagnostic-bundle` (and `make session-verdict` / `make session-bundle`) still answer on a session that failed hours ago. After it expires the session's samples, its non-capture events, and its `session_trace_clock` row are swept. **Must be >= `QUASAR_TELEMETRY_ROLLING_WINDOW`**, validated at boot: a post-mortem shorter than the window it preserves would delete the evidence it exists to keep. Capture results (`diag.*`) are exempt from this rule and from the rolling window — they leave only with the session row's `ON DELETE CASCADE`. |
| `QUASAR_PLATFORM_RELEASE_REPO` | `accreleus/quasar` | **Platform-release detection (#104/#110).** The `owner/name` repository whose GitHub Releases the `platform.release_detect` job reads. Point it at a fork to follow that fork's releases. Set it to **`off`** (or `none` / `disabled`) to turn detection off entirely: the job still exists in the Jobs page but every run is `skipped` with a reason, no outbound request is made, and the admin Releases page simply reports nothing available. **Empty means the default, not off** — compose forwards every knob as `${VAR:-}`, so a stock install hands the process an empty string and reading that as "off" would disable self-update everywhere. |
| `QUASAR_PLATFORM_RELEASE_API` | `https://api.github.com` | API base for the same job, for a GitHub Enterprise host or a test double. Its host is added to the release client's egress allowlist automatically, so pointing at a different API needs no second knob. Must be `https` — the shared outbound client refuses anything else. |
| `QUASAR_PLATFORM_RELEASE_ASSET_HOSTS` | `github.com,objects.githubusercontent.com,release-assets.githubusercontent.com` | Comma-separated egress allowlist for **release asset downloads**, and the only hosts the download's single redirect may point at. The asset URL and its `Location` are both remote-supplied, so a host outside this list is refused **by name** — the job summary names it and this variable, which is the whole remedy. Separate from the API host because GitHub serves assets from a CDN, and **that CDN moves**: the 302 target was `objects.githubusercontent.com` and is now `release-assets.githubusercontent.com`. Both stay listed; an entry GitHub has stopped using costs nothing, while a missing one stops detection dead (seen live on `v0.2.0-rc.1`). If a future move breaks detection again, the fix is to add the host the summary names. |
| `QUASAR_PLATFORM_REGISTRY` | `ghcr.io` | **Edge channel (#111).** The registry the platform's two component images (`<registry>/<QUASAR_PLATFORM_RELEASE_REPO>/quasar-control-plane` and `.../quasar-node-agent`) are published to. Used only while `release_channel` is `edge`, where detection resolves the branch tag to a digest and reads the build's identity from the image labels — the stable channel reads a release manifest instead and never touches a registry. This host is added to the `QUASAR_IMAGE_REGISTRY_HOSTS` egress allowlist **automatically**, so repointing the platform images at another registry is one knob, not two. A registry that needs credentials is not supported here: resolution is anonymous-pull only. |
| `QUASAR_PLATFORM_RELEASE_TOKEN` | unset | Optional bearer token for the releases listing and asset download. A public repository needs none; set it for a private fork or to lift GitHub's unauthenticated rate limit. Never logged. |
| `QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL` | unset; job default `168h` (7 days) | Standard job `EnvOverride` for `platform.release_detect`, which by default runs weekly inside a one-hour window opening 02:00 Monday UTC. A Go duration that is authoritative over the admin Jobs page while set; `0` stops the job being scheduled at all. "Check now" (`POST /v1/admin/jobs/platform.release_detect/run`) bypasses the window as it does for every job. |
| `QUASAR_TELEMETRY_RETAIN_INTERVAL` | unset; job default `5m` | How often the `telemetry.retain` job applies the two rules above. A standard job `EnvOverride`: a Go duration that is **authoritative over the admin Jobs page** while it is set (and shown as env-locked there), `0` is the kill switch that stops the job being scheduled at all, and a malformed value falls back to the job row rather than failing startup. One pass deletes in bounded batches, logs one `INFO` line with the counts, and `WARN`s if it took over 30s or could not drain its backlog. **This job is the only thing that deletes session telemetry** — no ingest path prunes, and reaching a terminal state prunes nothing, so with it disabled telemetry grows without bound. |

### Encrypted secrets — the master key, and what losing it costs

Operator credentials that are set from the **admin UI** (today: the SteamGridDB
artwork key; more are coming) are stored **encrypted** in Postgres, in the
`instance_secrets` table. The construction is AES-256-GCM with a fresh random
nonce per encryption and the secret's own name bound in as additional
authenticated data, so a ciphertext copied onto another row fails to decrypt
rather than silently becoming that other secret.

**The master key is `QUASAR_SECRET_KEY` and it lives only in the environment.**
It is never written to the database, which is the entire point: a database dump
is not enough to read a stored credential.

Three states, and they are deliberately distinguishable:

| state | what happens |
|---|---|
| **Unset** | Supported and the default. The control plane boots; the admin UI says storing credentials is unavailable on this deployment and points at this variable; any env-var fallback (e.g. `QUASAR_STEAMGRIDDB_API_KEY`) keeps working. Nothing panics and nothing else is affected. |
| **Correct** | Secrets store and read normally. The admin UI shows *configured / not*, a masked hint (the last 4 characters, and nothing at all for a short value), and which source is in effect. |
| **Wrong or changed** | Decryption fails **loudly and specifically**: "the master key does not match the stored secret". It is never reported as "not configured", because the operator's fix is completely different — restore the original key, or re-enter the secret to re-encrypt it under the current one. A stored-but-unreadable secret also does **not** silently fall back to its env var: quietly using a different credential than the one an admin configured is precisely what this facility is built to prevent. |

> **Back up `QUASAR_SECRET_KEY` with your deployment secrets.**
> **If you lose it, every value stored through the admin UI is unrecoverable** —
> there is no recovery path by design, and no copy anywhere in the database or
> on disk. The affected secrets must be re-entered (and, for API keys, possibly
> re-issued at the provider). A database backup restored onto a control plane
> with a different master key will show those secrets as *configured but not
> readable*, with that exact message.

**Rotation** is not implemented, but is not designed out: every row records the
key version that wrote it, and `QUASAR_SECRET_KEY_PREVIOUS` supplies
decrypt-only predecessors, so a rotated deployment keeps reading rows written
under the old key. A re-encrypt sweep can be added later with no schema change
and no wire change.

**Precedence.** A secret stored in the database **wins** over its declared
environment variable. That direction is chosen so a key typed into the admin UI
is never silently overridden by a stale env var — a control that appears to save
and then does nothing is the worst outcome available. Because the env var stays
the *fallback*, an existing deployment upgrades with no change at all, and
clearing a secret in the UI falls back to the env var rather than off a cliff.
The admin UI shows which source is live either way.

### Cover artwork — bring your own API key

**Quasar ships no API key and never fetches artwork on anyone's behalf.** Quasar is
self-hosted, not a SaaS: an operator supplies their own SteamGridDB key, and the
relationship with the provider is theirs. That is why the feature is off by default
— not because the project is blocked on anything, but because there is nothing for
it to use until you provide a key.

**Where to put the key.** Either the `QUASAR_STEAMGRIDDB_API_KEY` environment
variable (unchanged, still supported), or the **admin UI** — app editor →
Artwork → *SteamGridDB API key* — which stores it encrypted (see *Encrypted
secrets* above) and takes effect **immediately, with no restart**. A key stored
that way takes precedence over the environment variable; clearing it falls back
to the variable. The panel always states which of the two is in effect.

Notes from SteamGridDB's public documentation, so an operator knows what they are
agreeing to when they create their own account (read without an account; anything
behind a login could not be verified):

- **Attribution:** the site terms state **no attribution requirement**. Quasar stores
  and can render "Artwork via SteamGridDB" beside the art anyway, so a deployment's
  source is visible rather than implicit.
- **Redistribution / caching:** the terms are **site-wide, not API-specific**, and bar
  distributing "any part or parts of the Site or the Service" without written
  permission, with no API-specific caching carve-out. Quasar caches locally by design
  — it must not hotlink, both so browsing does not depend on a third party being
  reachable and so the library contents are not leaked to them. A personal
  deployment caching art for its own users is the ordinary use of the API; a
  deployment that redistributes it more widely is the operator's call to make.
- **Commercial use:** the terms limit use to "personal, non-commercial" purposes.
  A commercially operated deployment should take that up with the provider.
- **Rate limits:** **none are published**, logged in or out. The client therefore
  self-throttles (one request per 500 ms) and treats a `429` as a distinct,
  named error rather than a generic outage.
- **Images are served from `cdn2.steamgriddb.com`** and reportedly carry no CORS
  headers — another reason art is cached and re-served locally rather than
  hotlinked.

None of the above is legal advice. The key is the operator's, and so are the terms
attached to it. `QUASAR_ARTWORK_PROVIDER=none` disables the third-party half
outright while keeping admin upload and explicit-URL override working, which is the
right setting for a deployment that would rather supply its own art.

### Debug / profiling listener (`QUASAR_PPROF_ADDR`)

`control-plane/` had no profiling hooks at all until PROF-01 (#388): no `pprof`,
no `expvar`, no `runtime/metrics`. There was no way to answer "why has this box
grown to 4 GB after three weeks" on an operator's own hardware.

**It ships enabled** (decision, Michael 2026-07-31). Diagnosing a long-uptime box
on somebody else's machine, without asking them to rebuild an image, is the whole
point of having it.

**The listener is a separate `http.Server`, never a route on the application
mux.** That is deliberate and load-bearing:

- The application mux is internet-reachable on a hardened deploy. A loopback bind
  is a *network-level* guarantee that no future routing or middleware mistake can
  widen.
- `RegisterRoutes` feeds the OpenAPI drift test, which asserts the registered
  `/v1` surface matches `protocol/openapi.yaml` exactly. A separate server touches
  none of that.
- The `RequireAuth` → `RequireAdmin` gate is unchanged. pprof is not admin
  surface, it is *operator* surface, and the operator has shell.

Two unit tests hold this shape in place: one asserts `RegisterRoutes` registers
nothing under `/debug`, the other asserts no compose file publishes 6060.

**Access.** There is no host port. Use a shell in the container:

```bash
# heap, the one that answers "what is growing"
docker exec quasar-control-plane wget -qO- 127.0.0.1:6060/debug/pprof/heap > heap.pb.gz
go tool pprof -http=: heap.pb.gz

# two snapshots hours apart, diffed — this is how a leak shows itself
go tool pprof -base heap-t0.pb.gz heap-t1.pb.gz

# a leaked goroutine names its own bug in the blocked stack
docker exec quasar-control-plane wget -qO- '127.0.0.1:6060/debug/pprof/goroutine?debug=2'

# 30 s CPU profile
docker exec quasar-control-plane wget -qO- '127.0.0.1:6060/debug/pprof/profile?seconds=30' > cpu.pb.gz
```

The production image is built `-trimpath -ldflags="-s -w"`, which strips DWARF and
the ELF symbol table but **not** Go's `pclntab`. `go tool pprof` symbolises Go
frames from `pclntab`, so profiles off the shipped binary resolve to function
names and line numbers with no build change.

**Quasar-specific endpoints** on the same listener:

| Endpoint | Purpose |
|---|---|
| `GET /debug/quasar/pool` | `pgxpool.Stat()` as JSON. Pool exhaustion is invisible otherwise and is the most likely Go-side production stall: `acquired_conns` pinned at `max_conns` with a rising `empty_acquire_count` is the signature. Answers `503` if no pool is wired. |
| `GET /debug/quasar/runtime` | Goroutine count, `GOMAXPROCS`, `GOMEMLIMIT`, and the heap counters, without downloading and parsing a profile. This is what the soak harness samples per cycle. |
| `POST /debug/quasar/mutexprofile?fraction=N` | Arms mutex profiling (`0` disables, `1` samples every event, `N>1` samples 1/N). Returns the fraction that was in force before. |
| `POST /debug/quasar/blockprofile?rate=N` | Arms block profiling (`0` disables, `1` tracks every blocking event, `N>1` samples one event per N ns blocked). |

**Mutex and block profiling are off at boot and are runtime toggles, not boot
config**, because both carry a real steady-state cost. Arm one for a ten-minute
window, capture `/debug/pprof/mutex` or `/debug/pprof/block`, then disarm.

**Threat model**, stated because the folklore is wrong in both directions:

- A Go **heap profile does not contain memory contents.** It is aggregated
  allocation stacks and counts. It cannot leak a token or a password hash.
- What *does* disclose: `/debug/pprof/cmdline` (argv),
  `/debug/pprof/goroutine?debug=2` (full goroutine stacks, i.e. internal structure
  plus partial argument values as raw words), `/debug/pprof/trace` (an execution
  timeline).
- The availability risk is an unbounded `?seconds=`: anyone who can reach the
  endpoint can hold a profile open indefinitely.

The loopback bind answers all three, which is why it is the default and why the
port is published nowhere. **If a profile is needed from a remote box, pull it
over SSH — do not widen the bind address:**

```bash
ssh operator@host 'docker exec quasar-control-plane wget -qO- 127.0.0.1:6060/debug/pprof/heap' > heap.pb.gz
```

Note the listener is on the *container's* loopback, so a plain `ssh -L` port
forward to the host reaches nothing: the hop has to go through `docker exec` (or a
container sharing the namespace).

---

## Node agent — connection & identity

Read in `node-agent/src/config.rs`. `CONTROL_PLANE_URL` is **required**.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ENROLLMENT` | unset | **The one-paste way to join a second host (#12).** Admin → Fleet → Enroll host also prints a one-line installer (`deploy/enroll-host.sh`, #100, served by the control plane itself at `/enroll-host.sh`; a self-signed control plane is fetched with `curl --pinnedpubkey`) that consumes this string on the new machine: host preflights, the agent image pinned to the running release, an agent-only Compose file plus a 0600 `.env` holding this variable, the agent started and watched until enrolled — see `deploy/README.md` "Multi-host". The enrollment string itself: `qenr1.<FINGERPRINT>.<base64url(wss-url)>.<token>`. It supplies the control-plane URL, the certificate fingerprint to pin, and a minted single-use enrollment token in one value — the fingerprint is first and verbatim (uppercase colon-separated SHA-256, exactly as the control plane logs it) so you can compare it by eye before pasting. The agent refuses a string carrying a `ws://` URL. Precedence when other variables are also set: `CONTROL_PLANE_URL` overrides the URL inside it (logged at WARN — split-horizon deployments) **but only with another `wss://` address — a `ws://` override is fatal even with `QUASAR_ALLOW_PLAINTEXT_AGENT=1`**, because it would send the string's own token in cleartext (unset `QUASAR_ENROLLMENT` and use `ENROLLMENT_TOKEN` if cleartext is really intended); `CONTROL_PLANE_FINGERPRINT` overrides the fingerprint inside it (WARN — the certificate-rotation path); an `ENROLLMENT_TOKEN` that *differs* from the token inside it is fatal; a saved pin that differs from the configured one is superseded with a WARN. An empty fingerprint segment (`qenr1..`) means the control plane is behind a real-CA certificate and is logged as such at connect, so a mispasted string is visible. Once enrolled, the pin is saved beside the node secret (`NODE_SECRET_PATH` + `.tls`) and this variable can be removed. |
| `CONTROL_PLANE_URL` | — (**required** unless `QUASAR_ENROLLMENT` supplies it) | Control-plane WebSocket, e.g. `ws://localhost:8080` or `wss://cp.example:8443` (HTTP base for the agent pull channels is derived from it: `ws→http`, `wss→https`, strips `/agent/ws`). **`wss://` works (#12)** and both agent clients — the websocket and the node-secret HTTP polls — verify the same way: **pinned** to the control plane's leaf certificate when a fingerprint is known (from `QUASAR_ENROLLMENT`, `CONTROL_PLANE_FINGERPRINT`, or the saved pin), else against the bundled WebPKI roots (a real certificate, e.g. the Caddy overlay). Under a pin, SAN and expiry are not checked — the pin is the identity, and the self-signed default routinely lacks the LAN name or IP you dial. **`ws://` is cleartext**: the enrollment token and the node secret cross it as plain JSON. It is allowed without ceremony only to loopback (`localhost` / `127.0.0.0/8` / `::1`, the single-host compose default); to any other host the agent refuses to start unless `QUASAR_ALLOW_PLAINTEXT_AGENT=1`. |
| `CONTROL_PLANE_FINGERPRINT` | unset | Manual certificate pin: the SHA-256 the control plane logs at startup (`fingerprint=…`, also on the admin Access panel and `GET /v1/admin/access-check`). Accepts `AB:CD:…`, lowercase, bare hex, or a `sha256:` prefix. Use it to **rotate**: after a control-plane certificate is re-issued every pinned agent stops connecting (it logs `token="cp-tls-pin-mismatch"` with the expected and observed values) until this is updated — one value per host, no re-enrollment, the node secret and host row survive. Overrides the fingerprint inside `QUASAR_ENROLLMENT` and the saved pin. Does nothing on a `ws://` URL (logged at WARN). |
| `QUASAR_ALLOW_PLAINTEXT_AGENT` | unset (off) | `1`/`true` permits a `ws://` control-plane URL to a **non-loopback** host. Only for a network you own end to end (a VPN or private link) and only as a transition: the credentials are on the wire. Every connect logs the policy. Existing single-host installs dial loopback and never need this. |
| `ENROLLMENT_TOKEN` | unset | Presented on first enrollment; must match the control-plane's. Optional once the node secret is persisted (`NODE_SECRET_PATH`). An empty/whitespace-only value is treated as unset. **#519: if there is no persisted node secret AND this is unset/empty, the agent cannot ever register — it logs `token="boot-enrollment-unconfigured"` naming this variable and the admin Hosts page, then exits non-zero** (after a short delay, so a `restart: unless-stopped` compose policy crash-loops visibly rather than hot-spinning) instead of idling forever in the reconnect loop while `docker compose ps` shows it healthy. Get a token from Admin → Fleet → Enroll host (a minted per-host token, or the whole enrollment string via `QUASAR_ENROLLMENT`), or copy the static `ENROLLMENT_TOKEN` from the control plane's `deploy/.env`. |
| `NODE_NAME` | system hostname | Stable identity for the host; also scopes the default secret path. |
| `NODE_SECRET_PATH` | `/tmp/quasar-{NODE_NAME}-secret` | Where the per-node secret (issued at enrollment) is persisted. In compose this is a named volume so it survives restarts. The certificate pin learned at the first verified `wss://` connection is saved next to it as `<path>.tls` (0600, created atomically, never following a symlink). It is refreshed only when `CONTROL_PLANE_FINGERPRINT` supplies a different pin — the operator-driven rotation path; a pin arriving via the enrollment string never overwrites an existing file, and the managed-image state as `<path>.images.json`. |
| `RUST_LOG` | `info` | `tracing` env-filter (e.g. `debug`, `quasar_node_agent=debug,info`). |
| `QUASAR_LOG_FORMAT` | `text` | `text` \| `json`. `text` is the human format operators read out of `docker logs`; a session's lines carry a `session{id=<session_id> host=<node_name>}` span prefix. `json` emits one object per line with the event's own fields flattened to the top level and the open spans under `spans` — for a log shipper, not for reading. An unrecognised value warns (`token="log-format-unrecognised"`) and falls back to `text`. Logs go to stderr either way. Every WARN/ERROR line also carries a stable `token=<kebab-name>`; see `.claude/rules/agent-logging.md`. |
| `QUASAR_JOB_POLL_SECS` | `60` | How often the agent polls `GET /v1/agent/jobs/pending` for work the control plane has scheduled for this host. Also the worst-case latency of an admin "Run now" on an agent-plane job, which the API response says out loud. `0` disables the job client on this host: scheduled agent-plane jobs are then never claimed and show as overdue in the admin viewer. |

---

## Node agent — encoder & GPU

Intel inventory does not require AMD's `mem_info_vram_total` attribute. The agent
uses the DRM card's `lmem_total_bytes` when available, then sums exposed
`device/tile*/physical_vram_size_bytes` values. Without a local-memory figure it
reports **half of host `MemTotal` as an estimated shared-memory capacity**, logged
as `token=intel-shared-memory-capacity`. This supports iGPUs without dedicated
VRAM; it is neither reserved memory nor a measured free-VRAM value. Intel live
used/free VRAM remains unknown. A driver that omits local-memory attributes on a
discrete Intel GPU also takes this estimate; inspect that log when validating
capacity on new hardware. Malformed local-memory data, incomplete tile readings,
or an unavailable host-memory fallback keep detection fail-closed.

Inventory success does not establish encoder support. Intel's default remains
`va`, and explicit encoder settings still win. The runtime must provide a working
Intel VA driver for that path; selecting Vulkan requires hardware/driver support
for Vulkan Video encode. The Vulkan resolver has no automatic OpenH264 fallback.

Read in `node-agent/src/session/mod.rs` (`SessionConfig::from_env`). Apply to every
session the agent runs. Integer knobs use a parser that **ignores junk/≤0 and falls
back to the default** (except `QUASAR_CUDA_DEVICE`, which allows `0`).

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ENCODER` | unset → **auto-detect** | Unset or empty → the agent detects the GPU vendor (configured render node's sysfs vendor, then `/dev/nvidia*` device nodes, then a `/dev/dri/renderD*` scan) and defaults **nvidia → `vulkan`, amd → `vulkan`, intel → `va`, no GPU → `openh264`** (logged at startup as `token=encoder-autodetect`). Explicit values always win: `va`/`vaapi` → AMD/Intel VA-API HW; `nvenc`/`nvidia` → NVENC HW; `vulkan` → `vulkanh264enc` (zero-copy NV12 `memory:VulkanImage` from `waylanddisplaysrc vulkan=true`); `openh264` → software. Any other non-empty value falls back to `openh264` with a `token=encoder-env-unrecognized` warn. Compose passes the var through unset by default (`docker-compose.yml`; the NVIDIA overlay sets nothing — see [NVIDIA hosts default to Vulkan](#nvidia-hosts-default-to-vulkan)). An explicit `QUASAR_ENCODER=nvenc` in `deploy/.env` (or an admin host override) still selects NVENC everywhere. |
| `QUASAR_RENDER_NODE` | `software` | DRM render node for the HW compositor/encode. Recommend the stable `/dev/dri/by-path/pci-<addr>-render` form over a raw `/dev/dri/renderD1xx` — render-node numbering is PCI-enumeration order and can flip across reboots (e.g. after a kernel/driver update reorders GPU probing). The agent canonicalizes a `by-path`/symlink value to its resolved `renderD*` target at load (host-observability), logging `render_node <raw> -> <canonical>`; this is what's reported in `effective_settings` and used to pin the VA encoder. Leave `software` only for the SW path. |
| `QUASAR_REQUIRE_HW_RENDER` | `1` (require) | #378 W2.3 fail-closed knob. The compositor bus can post a **Warning** — not an Error — when it silently falls back to software rendering (gst-wayland-display's `wolf-renderer-degraded` bus marker (the historical `quasar-renderer-degraded` spelling is still matched), emitted only from its dmabuf-import failure path). Default `1`/unset **fails the session closed** on a *repeat* of that signal (debounced: a single marker only warns + arms a 30s window; a second marker within the window trips the fail-closed path — a one-off transient during compositor warmup does not) instead of streaming a silently software-rendered picture (black, for GL clients). `0`/`false` permits the degraded mode and lets the session keep running. The default is global (not gated on GPU assignment) but is safe as such: the marker is only ever emitted from the dmabuf-import path, which a `render-node=software` session's compositor never takes — a software session simply never has anything to trigger on. |
| `QUASAR_CUDA_DEVICE` | `0` | NVENC GPU index (≥0). Pins the CUDA device for multi-GPU NVENC boxes. |
| `QUASAR_SYNTHETIC_GPU_CAPACITY` | off | **Dev-only.** `1`/`true`/`yes` (case-insensitive); anything else, including unset, is off. When real GPU capacity detection (`/sys/class/drm`) fails to find anything, the off (default) path reports zero GPUs and logs `token="gpu-capacity-unavailable"` — the host is unschedulable for GPU work. With this on, a failed real detection instead fabricates one synthetic `unknown`-vendor GPU (`vram_mb_total: 8192`, `encode_slots_total: 1`) and logs `token="synthetic-gpu-capacity"`, so a GPU-less dev box can still be admitted sessions for pipeline/API testing. Read once at capacity detection (agent startup). |
| `QUASAR_GOP` | `60` | Keyframe interval, in frames **at the 60 fps reference** — the agent scales it to the session's real fps so the keyframe cadence in TIME is constant (`60` ⇒ one keyframe per second at 30/60/120 fps alike; a 120 fps session gets `idr-period=120`). Unscaled frame counts halved the period at 120 fps, and AV1's whole-frame-intra keyframe burst under tight CBR then reads as a 2 Hz micro-glitch. |
| `QUASAR_SLICES` | `8` | H.264 slices per frame — a lost packet damages one strip, not the whole frame. |
| `QUASAR_TARGET_USAGE` | `6` | VA encoder quality/speed: `1`=quality … `7`=speed/lowest-latency. |
| `QUASAR_QUEUE_BUFFERS` | `3` | Encode-path queue depth (latency vs. burst smoothing). |
| `QUASAR_ZEROCOPY` | off | `1`/`true` → full zero-copy VA (compositor emits `memory:DMABuf`, no system-memory hop). **VA encoder only**, and only on a non-deep-trace session (ZC-03). |
| `QUASAR_LATENCY_PROBE` | off | `1`/`true` arms the **host-stage latency probe**: the always-on encode pad probes additionally time each frame across compositor emit → encoder input → encoder output → payloader → the post-`rtpbin` egress seam, and publish per-window p50/p95 as additive `probe_*` keys on `session_metrics` (`probe_capture_to_enc_in_*`, `probe_enc_out_to_send_*`, `probe_pay_to_send_*`, `probe_pts_to_emit_*`, `probe_compositor_frame_interval_p95_ms`, plus the diagnostic counters `probe_send_desyncs` / `probe_pts_unmatched`). Adds **no element and no new pad probe** — the extra work rides callbacks that already fire, ~5 clock reads per frame — and reads buffer/RTP header metadata only, never pixels (#270). Encoder-agnostic (NVENC/Vulkan/VA). A measurement instrument for attributing the T1 latency residual (`docs/reports/2026-08-18-overnight-optimisation/t1-drops-latency.md` §2), **not a production setting**: leave it off unless you are running a latency attribution. Design + drafted protocol amendment: `docs/superpowers/specs/2026-08-18-latency-probe-design.md`. |
| `QUASAR_H264_PROFILE` | `constrained-baseline` | Per-session H.264 profile (dev-path default; the assignment overrides it). Browsers reject AMD's default High profile, hence the constrained-baseline default. Applies only when the session codec is `h264`; ignored for `h265`/`av1`. |
| `QUASAR_CODEC` | unset (h264) | Agent-side force of the session video codec (`h264` \| `h265` \| `av1`), for harness/diagnostic use — same escape-hatch pattern as `QUASAR_H264_PROFILE`. In normal operation the codec is chosen server-side (see [Multi-codec](#multi-codec-hevc--av1)) and sent in `session_assign.stream.codec`; this env only forces the agent when set. `h265`/`av1` require the host's active encoder path to provide the element **and** payloader. |
| `QUASAR_VULKAN_H264` | **on** | **Vulkan encoder only.** Set `0`/`false`/`off` to stop producing H.264 with `vulkanh264enc` on this host; same parsing as the other two knobs. H.264 is the guaranteed codec floor, so disabling it only ever moves H.264 to the vendor HW encoder (`nvcudah264enc`/`vah264enc`). On a host with no vendor H.264 encoder to borrow, the agent logs an **error** and keeps H.264 on `vulkanh264enc` anyway — a host with no H.264 cannot serve any client. |
| `QUASAR_VULKAN_HEVC` | **on** | **Vulkan encoder only.** Set `0`/`false`/`off` to stop producing H.265 with `vulkanh265enc` on this host. Unset (the default), empty, `1`, `true` or `on` all mean enabled; any other value is ignored with a warning and the codec stays enabled. Enabled is what makes a `vulkan`-encoder host report `h265` in its codec set, and it also **auto-pins the compositor's Vulkan encode-src ring to a double-buffered depth** (`WOLF_VULKAN_RING=2`, set by the node-agent — no operator action). `RING=2`, not the single stable slot `RING=1`, is pinned: a single slot starves under the G1 `ParentBufferMeta` buffer-reuse gate (spec `docs/design/plans/2026-07-25-vulkan-multisession-spec.md` §2c) — frame N's child buffer legitimately still sits downstream when the compositor needs the slot back for N+1 (measured 0.45 fps, `busy-drop` warnings at `RING=1`). `RING=2` fixes the starvation and still eliminates NVIDIA's H.265 encode-src pool per-slot tiling defect that blacks 1-in-4 frames at the default `RING=4` (0 black frames observed at `RING=2` over the Tower rung-2 validation, 2026-07-25, on both h264 and h265). The pin is host-wide (it covers h264 vulkan sessions too — both codecs beat develop's single-buffer encode latency at this depth) while HEVC is enabled; disabling HEVC removes the pin. Set a non-empty `WOLF_VULKAN_RING` (`1`..`4`, the gst-wayland-display ring-depth knob) explicitly to override the auto-pin — e.g. `WOLF_VULKAN_RING=4` for A/B. The clean uniform-tiling fix is infeasible on NVIDIA (its Vulkan encoder rejects a LINEAR encode-src image). Disabling H.265 here does not necessarily remove h265 from the host: see the fallback table below. |
| `WOLF_VULKAN_G1_BUDGET_MS` | unset (2 frame intervals from the negotiated framerate, clamped 4..100 ms; 33 ms fallback) | **Compositor (gst-wayland-display fork, pin 880e9b3a+).** How long the Vulkan encode-src G1 reuse gate may wait on the compositor thread for the encoder to release a ring slot before it drops the frame and re-emits the last completed one (`busy_drops`). Before #504 this was a flat 1 s poll, so any downstream back pressure (an encoder blocked on a push into a never-connected PeerConnection) starved the Wayland loop and the app never presented. `1`..`5000` ms override; `1000` restores the old behaviour for A/B. The gate itself is unchanged: a slot the encoder still references is never overwritten. The busy WARN is rate-limited (first after 1 s busy, then every 5 s, one INFO line on recovery). |
| `QUASAR_VULKAN_AV1` | **on** | **Vulkan encoder only.** Set `0`/`false`/`off` to stop producing AV1 with `vulkanav1enc` on this host; same parsing as the other two knobs. Enabled needs a build carrying the vendored `vulkanav1enc.patch`; on an unpatched image the element is simply absent from the registry and AV1 resolves to the vendor-HW fallback (`nvcudaav1enc`/`vaav1enc`) exactly as a disabled knob would — never a session-launch failure. AV1 needs **no** ring pin: proven live on the RTX 5090, 2026-08-21, zero black frames at the stock `RING=4`, so this knob does not change `WOLF_VULKAN_RING`. |
| `QUASAR_VULKAN_MAX_SESSIONS` | `2` | Default `2` — the Tower rung-3 soak (2026-07-25) proved 2 concurrent Vulkan sessions healthy under a twice-repeated per-session kill test (session B held a flat 60fps through session A's kill, no restart): kill-test-validated per-session isolation, not just a happy-path run. Rung 4 (N=4) probed all-healthy (60fps, encode p95 <8ms, ~710MiB VRAM/session) but that was a probe, not a soak — a deeper soak gates raising the default above 2 (see `docs/design/plans/2026-07-25-vulkan-multisession-spec.md` §2d/§4). **Vulkan encoder only.** Sets the advertised `encode_slots_total` — the concurrent Vulkan-encode session ceiling the scheduler admits against — for the GPU the agent's configured render node (`QUASAR_RENDER_NODE`) resolves to, in place of that GPU's per-vendor stub. On a multi-GPU vulkan host, GPUs that don't match the configured render node keep their vendor stub (not the override) — a display-only iGPU sitting alongside a discrete vulkan GPU no longer advertises vulkan capacity it can't serve. If the configured render node can't be resolved to any detected GPU, the override falls back to applying to every GPU (fail-open — a vulkan host must never advertise zero vulkan capacity). Read once at capacity detection (agent startup), not per session. Bounded above by the driver's real cap (the NVENC consumer session cap, currently 8; the RTX 5090 has 3 encode engines). Set it higher than the driver cap and the driver, not Quasar, rejects the surplus session's encoder creation — so keep it at or below the measured ceiling. A non-Vulkan host ignores this knob (keeps the vendor stub). Unset, non-numeric, or non-positive values fall back to the default `2` with a warn log. |
| `QUASAR_NVENC_MAX_SESSIONS` | unset (vendor stub `3`) | **#489 interim mitigation knob.** When set to a positive integer, caps the advertised `encode_slots_total` on every **NVIDIA** GPU (the admission ceiling the scheduler enforces), replacing the `nvidia:3` vendor stub. The NVIDIA 610-branch driver (610.57.04 **and** 610.43.02, both live-refuted on the devbox 2026-08-11) has a use-after-free in `libnvcuvid` that SIGSEGVs the **whole node-agent** — killing every session on the host — whenever one session's NVENC teardown overlaps another live or starting NVENC session. **Set `1` on multi-session NVENC hosts to make that overlap unschedulable** (single-session hosts are unaffected by the bug): trades host capacity for eliminating the host-wide blast radius. Applies to NVIDIA GPUs regardless of `QUASAR_ENCODER` (NVENC also serves the per-session AV1 fallback on vulkan hosts). Unset, non-numeric, or non-positive values are ignored with a warn log (the vendor stub stands — never a silent zero capacity). Read once at capacity detection (agent startup). Remove the cap once a fixed driver ships. |
| `QUASAR_NVENC_DEFER_TEARDOWN` | `1` (on) | **#489 mitigation, on by default on NVENC hosts.** At session end the encode pipeline is *parked* (kept alive, no state transition, so no NVENC destroy call is made) instead of set to NULL; the real destroy happens once the host holds **zero** live encode leases, serialized so no session can open an encoder mid-drain. Rationale: #489's trigger is destroy-while-another-encoder-is-live (an NVIDIA driver use-after-free in `libnvcuvid`, confirmed on 595.91.07, 610.43.02 and 610.57.04), which otherwise SIGSEGVs the node-agent and loses **every** session on the host. Live result on the devbox (RTX 5090, 610.57.04, 2026-08-12): the overlap repro crashed 2/2 with it off and was clean 10/10 with it on, both sessions streaming green. **Cost:** a parked pipeline holds its NVENC session and ~190 MiB VRAM until the host next goes idle, and the parked list is bounded only by how many sessions end while another is live, so a host under continuous churn accumulates them (5 back-to-back sessions parked 5 pipelines, +959 MiB, draining only at the end; VRAM returned exactly, no leak). The live free-VRAM admission veto (`QUASAR_VRAM_MIN_FREE_MB`, #383) is what stops that becoming unbounded: a creeping host refuses new launches rather than failing. Set `0` to opt out and accept the crash risk. NVENC only; VA/software ignore it, and whether NVIDIA's Vulkan video encode shares the same defect is untested as of 2026-08-12. |
| `QUASAR_CUDA_CONTEXT_SHARED` | on (process-global sharing) | **#489 experiment knob.** `0`/`false`/`no` hands every session its **own** `GstCudaContext` instead of the process-global one every session shares by default — still one per session, so a session's own source and encode pipelines keep sharing it (the ZC-02 cross-interpipe CUDAMemory contract is intact); only the sharing *between different sessions* goes away. Exists to test whether the `libnvcuvid` use-after-free that kills the agent when one session's NVENC encoder is destroyed while another is live (#489) is scoped to the shared CUDA context. Costs ~70ms of extra session start and a second context's VRAM. Any other value keeps the default (shared). |
| `QUASAR_FEC_MODE` | unset (derived) | Send-side ULPFEC/RED operating mode: `off` \| `fixed` \| `auto`. Unset/empty is **derived from `QUASAR_FEC_PERCENTAGE`** for back-compat (`0` ⇒ `off`, `>0` ⇒ `fixed`), so existing deployments are unchanged. `off` → no FEC (transceiver untouched, no `red`/`ulpfec` lines). `fixed` → static `ulp-red` at `QUASAR_FEC_PERCENTAGE`% for the whole session (today's behaviour). `auto` → negotiate `ulp-red` at **0%** up front (no SDP renegotiation later) and **ramp `fec-percentage` 0→N→0 mid-session** on an agent-local loss signal: zero overhead on clean links, proactive repair on lossy ones. The armed level `N` = `QUASAR_FEC_PERCENTAGE` if set (`>0`), else `20`. An unrecognised value warns and falls back to the derived mode. Node-agent-only; no wire/protocol change. |
| `QUASAR_FEC_PERCENTAGE` | `0` (off) | The ULPFEC/RED redundancy percentage (`0`-`100`). Its meaning depends on `QUASAR_FEC_MODE`: in **`fixed`** it is the static per-session redundancy; in **`auto`** it is the *armed* level (default `20` when unset); with `QUASAR_FEC_MODE` unset it also selects the mode (`0` ⇒ off, `>0` ⇒ fixed). Enabling `ulp-red` proactively repairs burst packet loss on lossy client links (WiFi) without waiting on a NACK/RTX round-trip or the next keyframe. **Bandwidth overhead**: redundancy adds roughly `percentage`% to the video bitrate on the wire *while armed* — GCC/ABR sees the *total* (FEC + media) when estimating available bandwidth, so a high percentage eats into the ABR headroom (in `auto` the overhead is only paid during loss). Out-of-range (>100) or unparseable values warn and fall back to `0`. Recommended: `20` for lossy WiFi clients. |
| `QUASAR_FEC_ARM_LOSS_PCT` | `0.5` | **`auto` mode only.** Per-window packet-loss percentage at/above which a window counts toward arming. Loss is measured agent-locally from the video `webrtcbin` `get-stats` (`remote-inbound-rtp.packets-lost` / `outbound-rtp.packets-sent`) once per evaluation window. |
| `QUASAR_FEC_WINDOW_S` | `5` | **`auto` mode only.** Loss-evaluation window length (seconds) — the controller's poll cadence. |
| `QUASAR_FEC_ARM_WINDOWS` | `2` | **`auto` mode only.** Consecutive over-threshold windows required to arm (default `2` × `5` s ≈ 10 s of loss). On arming, `fec-percentage` jumps to the armed level in one step (burst loss is the target). |
| `QUASAR_FEC_DISARM_WINDOWS` | `6` | **`auto` mode only.** Consecutive clean windows (loss `< 0.1%`) required to disarm back to `0%` (default `6` × `5` s ≈ 30 s clean). |
| `QUASAR_FEC_MAX_FLAPS` | `4` | **`auto` mode only.** After this many arm events in one session the controller **latches armed** (a link that keeps cycling is lossy; stop oscillating). |
| `QUASAR_INTRA_REFRESH` | `0` (off) | **Vulkan encoder only** (`QUASAR_ENCODER=vulkan`), and only on a **patched image** carrying `deploy/patches/vulkan/vkh264enc-intra-refresh.patch` (#227 A1) — `vulkanh264enc` has no `intra-refresh` property on stock GStreamer. `1`/`true` → enable rolling intra refresh (replaces the periodic full-IDR spike with a rolling wave of intra-coded macroblocks, bounding loss-recovery time to one refresh cycle instead of the next IDR). On an unpatched build the property is absent — the agent warns and no-ops rather than failing the session, same as `QUASAR_SLICES` on stock builds. **Recommended with `QUASAR_SLICES` > 1**: in the driver-preferred per-picture-partition mode the refresh cycle length equals the effective slice count — which is itself clamped to the macroblock-row count and the driver's `maxIntraRefreshCycleDuration`, so the cycle always matches the slices actually coded — so `QUASAR_SLICES=1` degenerates to refreshing the whole frame every frame (no bandwidth win) — the agent warns clearly when this combination is used. Default off ⇒ byte-identical output to today. |
| `QUASAR_INTRA_REFRESH_PERIOD` | `0` (continuous) | **Recommended: `60`** (validated 2026-07-22, `docs/reports/ir-period-experiment-2026-07-22.html`: a 1 s wave matched continuous mode's loss recovery — zero hitch windows across clean/steady-loss/burst — while intra-coding a fraction of the data; continuous showed no measurable benefit over it).  `vulkanh264enc`'s `intra-refresh-period` in frames — frames between the start of one refresh cycle and the next. `0` runs back-to-back cycles continuously. Ignored unless `QUASAR_INTRA_REFRESH=1`. Same Vulkan-only, patched-image-only scope as `QUASAR_INTRA_REFRESH`. |

### Golden-home templates (#488 warm-up)

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_HOME_TEMPLATES` | `0` (off) | **Feature gate.** `1`/`true` enables the golden-home template system: the agent may build, store, and seed golden templates from managed app homes (WP1). Default off; the feature is SHIP-DARK on all hosts until explicitly enabled. When off, every session homes from scratch. Build a template by running `POST /v1/admin/images/ensure` on a profile with `QUASAR_TEMPLATE_WARMUP=1` on the target host; the warm-up will then build a template on its next scheduled run. |
| `QUASAR_TEMPLATE_ROOT` | (default: `{QUASAR_HOME_ROOT}/../templates`) | Host directory that holds built templates, indexed by image ID. Must be an absolute path, a sibling of (never inside) `QUASAR_HOME_ROOT` — Michael's operator constraint enforced at startup. Leave unset to use the default sibling path; set it to a custom location when templates live on a separate mount (e.g., `/mnt/ssd/quasar-templates`). Unset or an empty string defaults to `{QUASAR_HOME_ROOT}/../templates` (so if `QUASAR_HOME_ROOT=/var/lib/quasar/homes`, templates default to `/var/lib/quasar/templates`). An invalid path (not absolute, or inside `QUASAR_HOME_ROOT`) logs ERROR and the feature becomes unavailable — templates are disabled for this host, `GET /v1/agent/template/config` shows the misconfiguration, and every session homes cold. |
| `QUASAR_TEMPLATE_ALLOW_CROSSFS` | `0` (off) | **Escape hatch for the split-namespace guard.** When the template root and `QUASAR_HOME_ROOT` are on different filesystems, the agent logs a WARN at startup and **refuses warm-up builds** (`failed`, with the reason in the jobs viewer). Seeding is unaffected — it degrades to the tier-2 full copy. The refusal exists because on a containerized agent a cross-filesystem template root almost always means the template directory is *not bind-mounted*: the build writes gigabytes into the container overlay, invisible on the host and discarded by the next redeploy, and it can never be reflink-cloned. `1`/`true` builds anyway — for the one legitimate case, an operator who deliberately put the template root on a separate *bind-mounted* mount and accepts the slower copy. The real fix is almost always the bind mount in `deploy/docker-compose.yml`. |
| `QUASAR_TEMPLATE_CLONE_MODE` | `auto` | How to seed a managed home from a template: `auto` (probe reflink support; use reflink if available, else copy), `reflink` (require reflink, fail open to cold boot if unsupported), `copy` (always full-copy, no probe), or `off` (cloning disabled, use cold boot). Recommend `auto` for most deployments. A reflink is copy-on-write and ~0.76s for a 2.5GiB home; a full copy is ~3.9s. `off` is the fallback when the root is misconfigured or unavailable. |
| `QUASAR_TEMPLATE_WARMUP` | `0` (off) | **Build gate.** `1`/`true` allows this host to build templates (builds are expensive: ~3-4 minutes for a large game home). Default off; the host consumes pre-built templates but never produces them. Set on a smaller set of dedicated builder hosts so the primary capacity is not consumed by warm-ups. When off, a warm-up job run requested for this host completes as skipped. Separate from `QUASAR_HOME_TEMPLATES` (that gate gates *consumption*; this gates *production*). |
| `QUASAR_TEMPLATE_SETTLE_SECS` | `60` | Settle time (seconds) after the app reaches "presented" state and before the home is snapshotted for the template. The agent additionally requires write quiescence (no mtime change for 10 seconds) before snapshotting to ensure any pending writes are flushed. This allows background processes to finish housekeeping after the app is visible. Unset or non-numeric falls back to `60`. |
| `QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS` | `600` | Per-template build timeout (seconds). If a warm-up run exceeds this the job reports failed with a timeout message. Must exceed the expected build time for the largest game home on the host. The control plane's `QUASAR_JOBS_CLAIM_TIMEOUT_SECS` default (3600) has 6× headroom over this. Unset or non-numeric falls back to `600`. |
| `QUASAR_TEMPLATE_MIN_FREE_BYTES` | `20971520000` (20 GiB) | Free disk space required under `QUASAR_TEMPLATE_ROOT` before a warm-up build is permitted. If the filesystem has less free space a warm-up run defers with the reason. Allows the operator to reserve space for other workloads. Any bytes value; unset/non-numeric falls back to 20 GiB. |

### Throwaway-home GC (#500, node-agent)

Every qses/bench/harness run mints an ephemeral identity (username
`agent-<8hex>-<8hex>`, `internal/auth.MintEphemeral`), and each managed-home
session materialises `{QUASAR_HOME_ROOT}/{username}/{app}/`. The #175 reaper only
removes homes the control plane still tracks, so anything that fell out of that
chain stayed on disk forever — 213 homes / ~10 GB and a 97 % root filesystem on
the devbox. This sweep is the floor underneath it: it runs on the node-agent at
startup and every 24 h thereafter, and deletes only directories that are (a)
named exactly like an ephemeral user, (b) not mounted by any live container, and
(c) idle longer than the retention window. A real account's home is never a
candidate, and `templates` is never touched.

These are **process-level env knobs, not hostcfg/Host Settings knobs**: the
catalog delivers a knob as a per-connection `config_update` overlay, and this
sweep runs before (and independently of) any control-plane connection — a
catalog entry here would silently do nothing. Set them in `deploy/.env`; an
agent restart applies them.

One-shot run (an operator reclaiming disk now, or previewing what would go):
`make homes-gc HOST=<host> ARGS='--dry-run'`.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_HOMES_GC` | `1` (**on**) | Master switch for the throwaway-home sweep. On by default because the failure mode it prevents is a full disk, and the selection rule is conservative enough to be safe unattended: the name must match `agent-` + 8 hex + `-` + 8 hex exactly, nothing may have it mounted, and it must be past `QUASAR_HOMES_GC_RETENTION_HOURS`. `0`/`false` disables it entirely. Inert anyway when `QUASAR_HOME_ROOT` is unset (no local-driver root — nothing on disk to sweep; the docker-volume driver this used to also cover was hard-removed, #473, 2026-08-25). **Caveat:** a *registered* account deliberately named `agent-<8hex>-<8hex>` would match the pattern; nothing prevents that username today, so do not create one. |
| `QUASAR_HOMES_GC_RETENTION_HOURS` | `72` | How long a throwaway home must be idle before it is collectable. Age is the newest mtime in the home's top two levels (the home dir alone only changes when an app subdir appears/disappears, so a long-lived session inside `{home}/{app}/` would otherwise look untouched). Raise it to keep a weekend's bench artifacts; lower it on a small disk with a nightly harness. Non-numeric falls back to `72`. |
| `QUASAR_HOMES_GC_DRY_RUN` | `0` (off) | `1`/`true` logs one `WOULD delete …` line per home (with size and idle hours) and the sweep summary, and removes nothing. Use it for the first rollout on a host with unusual homes. |

Every deletion logs one line with the path, reclaimed size and idle age, and each
sweep ends with a summary line (`scanned / candidates / deleted / live /
too-young / purged / errors`). Removal is rename-then-`rm` inside the home root
(`.gc-trash-*`), so an interrupted removal leaves an inert name the next sweep
purges rather than a half-emptied home the provisioner would treat as warm. A
home root that is not absolute, is a system directory, or is fewer than two
components deep is refused outright.

### Image build (`image_build` agent-api, node-agent)

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_IMAGE_SOURCE_HOSTS` | `codeload.github.com,github.com` | Comma-separated, whitespace-trimmed, lowercased host allowlist a template-build `context_url` may point at (`protocol/agent-api.md` `image_build` SSRF containment — the node-agent-side twin of the control-plane's `QUASAR_IMAGE_REGISTRY_HOSTS` above; the two are easy to confuse but gate different things — this one gates where the agent downloads a build **context tarball** from, that one gates registry/artwork hosts). Empty entries are dropped. Alongside the allowlist, the download is capped at 512 MiB compressed (`MAX_CONTEXT_BYTES`), 2 GiB declared-uncompressed (`MAX_EXTRACTED_BYTES`, a decompression-bomb guard), and 100,000 tar entries (`MAX_ENTRIES`, an inode-exhaustion guard) — all compiled-in, not configurable. |

> See `CLAUDE.md` for the hard-won encoder gotchas (VA stale-registry rescan,
> `softpipe` hiding the VA encoder, NVENC preset GUIDs on Blackwell, etc.).

### `QUASAR_ENCODER=vulkan`

`vulkanh264enc` comes from the patched GStreamer 1.28.4 in `deploy/Dockerfile.vulkan`
(Fedora 43, carrying `deploy/patches/vulkan/vkh264enc-rc-fix.patch` for a post-PLAYING
rate-control rearm bug upstream hasn't fixed). Since W0 (#367) **every** image lineage
— `quasar-node-agent`, `quasar-nv`, `dev`, `runtime` — is built from that one Dockerfile
and shares the patched `/opt/gst`, so there is no separate "Vulkan image" to install:
the `quasar-nv` CUDA image encodes Vulkan just as well (proven 16/16 sessions on a
driver-volume NVIDIA host, 2026-08-12). The hostcfg catalog's `encoder` enum accepts
`"vulkan"` (`control-plane/internal/hostcfg/catalog.go`), so it is also selectable
per-host from the admin Settings page.

#### NVIDIA hosts default to Vulkan

With `QUASAR_ENCODER` unset the agent's vendor auto-detect picks `vulkan` on
NVIDIA hosts itself (neither compose file sets a value; the base file passes any
operator override through). Rationale: the #489 NVENC teardown
use-after-free lives in the NVIDIA driver's `libnvcuvid` and spans both the 595 and
610 branches (no driver pin escapes it), while the Vulkan encode path is immune
(8/8 clean against same-day NVENC controls). Defaulting to Vulkan moves the normal
streaming path off the affected library entirely.

What this means in practice:

- **H.264 (the default codec) encodes with `vulkanh264enc`.** Profiles ship
  h264-only, so this is what a stock deployment does.
- **All three codecs run on the Vulkan encoder by default** (2026-08-22).
  `QUASAR_VULKAN_H264`, `QUASAR_VULKAN_HEVC` and `QUASAR_VULKAN_AV1` are all ON
  unless an operator sets one to `0`/`false`/`off`; the env vars exist only to
  deliberately disable a codec. A `vulkan`-encoder host therefore advertises
  `h264`, `h265` and `av1` as long as the elements and RTP payloaders are present.
- **Disabling a codec moves it to the vendor HW encoder, it does not delete it.**
  `pipeline::effective_encoder` borrows `nvcuda<codec>enc` (NVIDIA) or
  `va<codec>enc` (AMD/Intel) per session, and the codec stays in the host's
  advertised set. Only when no vendor element exists does the codec drop out of
  the set. The same fallback covers a Vulkan element that is missing from the
  image — e.g. `vulkanav1enc` needs the vendored `vulkanav1enc.patch`, and an
  unpatched build silently uses `nvcudaav1enc` instead of failing the session.
- **`QUASAR_NVENC_DEFER_TEARDOWN` keys on the session's *effective* encoder**, so
  a session that fell back to NVENC keeps the #489 protection.
- **This is why the NVIDIA overlay pins the CUDA-bearing `quasar-nv` image.**
  `quasar-node-agent` is built `CUDA_ENABLE=0` and has no `cudaconvert`, so a session
  that falls back to the CUDA arm on it would die at pipeline build.
- **H.264 is the floor and cannot be removed.** Disabling it on a host with no
  vendor H.264 encoder logs an error and keeps `vulkanh264enc`.
- **Opting back into NVENC** is `QUASAR_ENCODER=nvenc` in `deploy/.env`, or an admin
  host override (`PATCH /v1/admin/hosts/{id}/settings`,
  `{"overrides":{"encoder":"nvenc"},"restart_confirm":true}`). Both still work
  unchanged; `QUASAR_ENCODER` is a restart-class knob either way.
- **Intel hosts auto-detect to `va`; AMD hosts auto-detect to `vulkan`** (set
  `QUASAR_ENCODER=va` in `deploy/.env` to keep an AMD host on VA-API).

Where each codec ends up on a `vulkan` host:

| knob | vendor H.264/H.265/AV1 element present | result |
|---|---|---|
| enabled (default) | either | the Vulkan element (`vulkanh264enc` / `vulkanh265enc` / `vulkanav1enc`) |
| enabled, Vulkan element absent from the image | yes | vendor HW element, per session; codec stays advertised |
| enabled, Vulkan element absent from the image | no | codec dropped from the host's advertised set |
| disabled (`=0`) | yes | vendor HW element, per session; codec stays advertised |
| disabled (`=0`) | no | codec dropped — **except H.264**, which stays on `vulkanh264enc` with an error logged |

The agent prints this per codec at startup (and again whenever a `config_update`
changes the effective encoder), naming the source of each knob value:

```
vulkan codec plan: h264=vulkan (default), h265=fallback:nvcudah265enc (env), av1=disabled (env)
codec support probed for Vulkan encoder: ["h264", "h265"]
```

That line is INFO while every enabled codec is on the Vulkan encoder or was
deliberately disabled. It logs at **WARN** as soon as a codec whose knob is still
ON has no registered Vulkan element (`fallback:` or `unavailable` above) — that is
a broken or mis-built image, not an operator choice, so it does not scroll past as
routine startup chatter. Each affected session logs one more WARN at resolution:

```
WARN vulkanav1enc not registered on this image, falling back to nvcudaav1enc; check the image
     contract (deploy/image-contract.json) - this host is configured for the vulkan encoder but
     is not running it for codec=av1
```

A codec an operator turned off with `=0` stays at INFO on both lines.

Bitrate is kbps, same as `va`/`nvenc`. ABR writability is **bitrate-only** — like
`openh264`, there is no CPB/VBV property and no encoder speed-bias lever, so the
SPT-08 smoothness-ladder speed rung is inert on this encoder. Measured encode_ms on
Renoir VCN2 at 1440p60 is roughly 7–13.5 ms (vs. ~19 ms for VA at 1080p) — the
fastest encoder path measured to date; also validated on the RTX 5090.

**Caveat:** `vulkanh264enc` has no `slices` property upstream, so it always emits
single-slice frames. A **headless software H.264 decoder presents 1440p+ slowly**
under that constraint — this is a decode-side artifact of the diagnostic harness,
not a stream defect. Real browsers hardware-decode and are unaffected; validate
this encoder with a real browser peer, not the headless troubleshoot harness, at
higher resolutions.

### Multi-codec (HEVC / AV1)

One codec per session, chosen **server-side at launch** and sent to the agent in
`session_assign.stream.codec`. Ships dark: every profile is H.264-only by default,
so merging changes nothing until an admin flips a profile's codec status.

**Vocabulary trap.** Two vocabularies are in play and the control plane bridges
them in exactly one place (`control-plane/internal/session/codec.go`
`catalogToWire`):
- **Wire / session / host** vocabulary: `h264` \| `h265` \| `av1` — the
  `sessions.codec` column, the `stream.codec` field, the host's reported codec
  set, and the `stream.codec` launch override.
- **Catalog** vocabulary (the profile catalog and its admin write path): `h264` \|
  `hevc` \| `av1`. Catalog `hevc` maps to wire `h265`.

**Resolution (control plane, at launch).** Candidates = the profile's codecs whose
status is `launchable`, in catalog (preference) order. They are clamped by (1) the
placed host's reported encoder codec set, (2) the launching device's decode probe
(`hevc`/`av1` are hard-gated on `capabilities.codecs.<codec> == true` — a
stale/absent probe means **no** HEVC/AV1: an undecodable codec is a black stream,
not a quality drop). The first surviving candidate wins; the fallback is always
`h264`, so a session can never fail to resolve a codec. Preference when enabled:
`av1 > hevc > h264` on AV1-capable hosts, `hevc > h264` otherwise. The decision
(candidates, clamps, result) is logged (`"codec resolved"`).

**Enabling a codec (operator).** Flip a profile's codec status future→launchable
through the existing admin write path
(`PATCH /v1/admin/stream-profiles/{id}` with a `codecs: [{codec,status}]` body,
catalog vocabulary). Stored in `stream_profiles.codecs` (JSONB; NULL ⇒ the in-code
default). **H.264 is the unconditional resolution floor and cannot be disabled**:
a non-empty `codecs` list must keep `h264` `launchable` (and may not repeat a
codec), else the write is rejected `400` — a session always falls back to H.264,
so the catalog must never present it as disabled. Start with `1080p60` on hosts that report the codec. Hosts advertise
what their active encoder path can produce (element + payloader both present) in
the additive `codecs` field of the capacity report → `hosts.codecs` (NULL ⇒ the
control plane assumes `h264` only, so old agents keep working).

**Overrides / escape hatches.** `stream.codec` on the launch request (admin/
diagnostic, wire vocabulary, validated) forces the codec authoritatively;
`QUASAR_CODEC` forces it agent-side (harness/diag); `QUASAR_VULKAN_HEVC=0`
disables H.265 on a Vulkan host's own encoder (it then falls back to the vendor
HW encoder, or drops out of the host's codec set if there is none). HEVC end-to-end decode requires a hardware
HEVC-decode browser (macOS/Windows Chrome) — Linux headless Chrome will not offer
H.265; AV1 validates in the Linux harness via dav1d.

---

## Node agent — adaptive bitrate (ABR / AS-03, SPT-02)

Read in `SessionConfig::from_env` / `abr_config()`. **`smooth` by default**
(SPT-10 verdict, 2026-06-27 — was `protective` since the AS10-05 ABR-on flip). Floor
is the explicit kbps if set, else `ceiling × ratio`, clamped to `[500, ceiling-1]`.
Ceiling = the session's configured bitrate.

### `QUASAR_ABR_MODE` — SPT-02 / SPT-04

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ABR_MODE` | `smooth` (SPT-10) | `protective` → AS-03 governor (one-step-down / EWMA-gated-up). `off` → fixed CBR; rtpgccbwe stays attached for GCC telemetry but never retargets the encoder. `smooth` (SPT-04) → encoder-aware, smoothness-biased down path: a non-emergency downshift is capped to −12.5% with a 7 s inter-step dwell and estimate smoothing while the encoder is *freshly* saturated, and GCC alone never forces a >50% cliff while the encoder is over budget; a CONFIRMED network-congestion window still triggers the FAST protective one-step descent (the #68 emergency guard). The up-path is identical to `protective` (slow +10% ramp). |

**Precedence (highest first):**
1. `QUASAR_ABR_MODE` — explicit value; unknown values warn + fall through.
2. Legacy `QUASAR_ABR=0` or `QUASAR_ABR_DISABLED=1` → maps to `off`.
3. Default: `smooth` (SPT-10; before 2026-06-27 the default was `protective`).

The legacy flags remain fully supported. If both `QUASAR_ABR_MODE` and a legacy
flag are set, `QUASAR_ABR_MODE` wins (it is checked first and short-circuits).

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ABR` | `1` (on) | `0`/`false` → legacy disable alias; equivalent to `QUASAR_ABR_MODE=off`. Superseded by `QUASAR_ABR_MODE` when both are set. |
| `QUASAR_ABR_DISABLED` | unset | `1`/`true` → legacy disable alias; equivalent to `QUASAR_ABR_MODE=off`. Superseded by `QUASAR_ABR_MODE` when both are set. |
| `QUASAR_ABR_FLOOR_KBPS` | unset | Explicit floor in kbit/s (>0). Overrides the ratio-derived floor. Applies in `protective` and `smooth` modes (any mode except `off`). |
| `QUASAR_ABR_FLOOR_RATIO` | `0.3` | Floor as a fraction of the ceiling when no explicit floor is set. Finite, >0. Applies in `protective` and `smooth` modes (any mode except `off`). |
| `QUASAR_ABR_LADDER` | on (in `smooth`) | SPT-08 smoothness ladder — the encoder-speed-bias rung (VA `target-usage` / NVENC preset escalation under sustained encoder saturation, unwound after 4 contiguous healthy windows). Only engaged in `smooth` mode; `0`/`false` disables it (bitrate-only SPT-04 behaviour). `protective`/`off` never engage the ladder. **Scope (#502, 2026-08-22): this knob gates the encoder-speed-bias rung ONLY.** It used to short-circuit `SessionConfig::ladder_config()` and silently disarm the resolution/fps rungs with it; now `abr_ladder=false` is expressed as `max_bias=0` (that rung inert) and every other rung keeps its own switch. |
| `QUASAR_ABR_LADDER_RESOLUTION` | `off` (ship dark) | abr-resolution-fps-ladder amendment: the resolution rung — steps the session's external (encoded) size down/up on sustained bitrate pressure via the same live-resize lever as a manual `PATCH .../display`. `0`/`false` (default) leaves the rung inert; flip per host after a netem soak. **Requires `abr_mode=smooth`** — and nothing else: since #502 it is independent of the `abr_ladder` master flag (before 2026-08-22 that flag silently disarmed this rung, costing two netem runs: `docs/reports/2026-08-22-vulkanscale-validation/REPORT.md`). A rung armed while `abr_mode` is `protective`/`off` is still inert, and the agent now WARNs once per session when that is the case. Inert on a Vulkan host without `vulkanscale` in the image, or for an h265 session on any Vulkan host (`external_resize_supported=false`); live for h264/av1 on a Vulkan host that carries `vulkanscale` plus the output-state resize patch (#501), same as VA/NVENC/openh264. |
| `QUASAR_ABR_LADDER_FPS` | `off` (ship dark) | **Shipped 2026-08-17** (live-class knob, `config_update`-overridable per host). The fps rung: halves the ENCODED frame rate (120 → 60) and restores it, on the same setpoint/hysteresis shape as the resolution rung. `0`/`false` (default) leaves it inert. **Requires `abr_mode=smooth`** (and, since #502, nothing else — the `abr_ladder` master flag no longer gates it; a non-`smooth` mode WARNs once per session). Only a launch rate that admits a halving has a rung at all (a 60 fps session never steps). **Independent of `QUASAR_ABR_LADDER_RESOLUTION`** — it works with the resolution rung on or off; with resolution off it is the only rung that steps. **Prerequisite: `videorate` must be in the host's image**; the agent inserts it at the head of the encode scale stage *only when this knob is on*, so with the knob off the graph is byte-identical to a pre-rung one. On an image without `videorate` the rung degrades to `fps_lever=false` plus exactly one warning at session start, and the resolution rung is unaffected. Inert on a Vulkan host without `vulkanscale` in the image, or for an h265 session on any Vulkan host (no scale stage at all); with `vulkanscale` present for an h264/av1 session (#501) the Vulkan arm gets `videorate` exactly like the other hardware arms. **Not yet validated on a VA host** — see issue #499 before enabling it on AMD/Intel. |
| `QUASAR_ABR_LADDER_FLOOR_FOLLOWS_RUNG` | `on` | **Amendment 5, 2026-08-17.** While a ladder rung is engaged, the ABR **floor** is scaled by the same factor the rung scales the comfort bitrate by: `floor_eff = launch_floor × (B_eff / ceiling)`, with `B_eff = B(r) × 0.6` when the fps rung is down — and it rises again as the ladder climbs back. Without this a rung is only half a lever: it changes bits per pixel, not bandwidth. Measured (run C): a 1080p120 h264 session with bounds `[4000, 11500]` behind a 3.5 Mbps link bottomed the setpoint at ~4.4 Mbps and flooded the pipe for a full 90 s window — 83 packets lost, 107 ms jitter buffer, ~6 fps at the browser — having taken every rung it had. With this on, that session's floor is 2400 kbps after the fps step alone and ≈1307 kbps at 720p60. **Inert with the ladder off or at the launch rung** (at rung 0 `B_eff == ceiling`, so the floor is today's exactly). Never below 300 kbps, never above the launch floor. A **host-level** `abr_floor_kbps` / `QUASAR_ABR_FLOOR_KBPS` is an absolute bound that always wins; a per-profile wire floor is NOT — it describes that profile's picture, so it travels with the rung (when the floor is instead `ratio × ceiling`, the formula reduces exactly to `abr_floor_ratio × B_eff`). The move applies to **both** bounds — the governor's floor and `rtpgccbwe`'s own `min-bitrate`; moving only the governor's is a measured no-op (run D: GCC pinned at exactly the launch floor and nothing downstream changed). `0`/`false` restores the pre-amendment fixed floor. Published per window as `abr_floor_kbps` in `session_metrics` (omitted while at the launch floor) and as `floor_kbps` in the `abr.ladder.step` trace event. Live-proven on the devbox NVENC path 2026-08-17 (run E: under a 3.5 Mbps cap the browser holds 60 fps at 720p60 where it previously sat at ~0); **not yet run on VA** — #499 gates the fps rung there. |
| `QUASAR_ABR_LADDER_ORDER` | `hybrid` | **Shipped 2026-08-17**, actuated whenever both rungs can move. Enum `res_first`\|`fps_first`\|`hybrid`. `hybrid` = resolution rungs down to 1080p, **then** fps 120→60, **then** the deeper resolution rungs (a frame-rate halving is the smaller perceptual cost at 1080p). `res_first`/`fps_first` are the two pure orders, for A/B. **Recovery always unwinds in the exact reverse of the engage order**, derived per window from the two rungs' positions — never a replayed stack, so a manual resize cannot desync it. With only one rung enabled the order is moot (the other rung is retired and every window falls through). |

#### Resolution ladder tuning (abr-resolution-fps-ladder)

Operator tuning knobs for the resolution rung's comfort-bitrate table and decision hysteresis
(`node-agent/src/session/ladder.rs`, spec `docs/superpowers/specs/2026-08-16-abr-resolution-fps-ladder-design.md`
§D1/§D2). Take effect when `abr_ladder_resolution` is on — and, because the **fps** rung shares this
same comfort table and dwell/hysteresis shape, they also govern the fps rung when
`abr_ladder_fps` is on (an invalid policy disables both rungs for the session). Every default
matched the spec's worked example **except `abr_ladder_res_recover_dwell`, flipped 2026-08-18**
(Amendment 6 in the spec — see below). Cross-knob validated server-side (see below).

| Variable | Default | Range | Notes |
|---|---|---|---|
| `QUASAR_ABR_LADDER_RES_EXPONENT` | `0.75` | `0.5..1.0` | The comfort-bitrate power-law exponent `k` in `B(r) = ceiling × (px(r)/px(0))^k` — the empirical bits-per-pixel law for H.264/HEVC/AV1 at fixed fps. Worked example: on a 1440p120 @ 10 Mbps session, `k=0.75` gives comfort bitrates 1080p 6.5 / 900p 4.9 / 720p 3.5 Mbps. |
| `QUASAR_ABR_LADDER_RES_ENGAGE_FRAC` | `0.6` | `0.2..0.95` | Step **down** one rung when the ABR setpoint stays below `engage_frac × B(r)` for `res_engage_dwell` consecutive windows. |
| `QUASAR_ABR_LADDER_RES_RECOVER_FRAC` | `0.8` | `0.3..1.0` | Step **up** one rung when the setpoint reaches `recover_frac × B(r-1)` for `res_recover_dwell` consecutive windows. |
| `QUASAR_ABR_LADDER_RES_ENGAGE_DWELL` | `2` | `1..60` | Consecutive pressure windows (~5 s each) required before a down-step. |
| `QUASAR_ABR_LADDER_RES_RECOVER_DWELL` | `2` | `1..60` | Consecutive recovered windows required before an up-step. **2026-08-18 flip** (was `4`; T3 `ladder_recover_fast` — time-to-launch-rung 55.2s → 37.6s (−32%), 0 oscillations: `docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md`). |
| `QUASAR_ABR_LADDER_RES_MIN_STEP_S` | `10` | `5..120` | Minimum wall-time between two resolution steps (anti-thrash), on top of the settle-window IDR-spike guard. |
| `QUASAR_ABR_LADDER_RES_MIN_HEIGHT` | `720` | `360..2160` | Floor rung — the ladder never steps the encoded size below this height. |

`abr_ladder_res_recover_frac` must exceed `abr_ladder_res_engage_frac` by at least `0.05` (the D2
hysteresis rule) — because the comfort-bitrate table is monotonic, this guarantees the per-pair band
never inverts (`engage_frac × B(r) < recover_frac × B(r-1)`). The control plane rejects a PATCH that
would collapse this band on the resolved (merged) settings map with `400 validation_failed`, naming
both `abr_ladder_res_engage_frac` and `abr_ladder_res_recover_frac` in the error.

`abr_mode` supersedes the legacy `abr_enabled` bool (kept in the catalog, deprecated, for existing
overrides and scripted PATCHes). Both the resolution and fps ladder rungs ship dark
(`abr_ladder_resolution=false`, `abr_ladder_fps=false`) — enabling them is an explicit per-host admin
opt-in. **Since #502 each rung has exactly one switch**: `abr_ladder` arms the encoder-speed-bias
rung, `abr_ladder_resolution` / `abr_ladder_fps` arm theirs — the master flag no longer disarms the
other two. The one gate they all share is `abr_mode=smooth` (the rungs ride the smooth-mode
classifier hook), so arming a rung on a `protective`/`off` host does nothing; the agent now logs
`SPT-08 ladder: … INERT for this session` at session start when it sees that combination. Every knob
above is live-class: a change applies to the *next* session on that host, not the
one in progress.

#### Governor hysteresis tuning (advanced)

Operator tuning knobs for the ABR governor's decision hysteresis
(`node-agent/src/session/abr.rs`). Every default matched the value previously
hardcoded **except `QUASAR_ABR_MAX_UP_STEP` / `QUASAR_ABR_MIN_INTERVAL_MS`, flipped
2026-08-18** (see the table below) — leaving the rest unset reproduces the
historical behaviour exactly. Invalid or out-of-range values warn once and fall
back to the default. All apply in `protective` and `smooth` modes (any mode except
`off`), except the three noted `smooth`-only.

Settable via `deploy/.env`/`deploy/docker-compose.yml` (a container recreate) **and
via Host Settings since 2026-08-18** (live: applies on the next session, no restart —
`hostcfg` keys `abr_ewma_alpha`, `abr_deadband`, `abr_max_up_step`,
`abr_min_interval_ms`, `abr_max_down_step`, `abr_down_dwell_ms`,
`abr_cliff_guard_frac`). Before that date these were documented but unsettable by
either path — absent from both the compose passthrough and the hostcfg catalog
(docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md).

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ABR_EWMA_ALPHA` | `0.3` | Up-path EWMA smoothing factor, `(0, 1]`. Higher tracks the estimate up faster. Operator tuning knob; default matches the previous hardcoded value. Settable via Host Settings since 2026-08-18. |
| `QUASAR_ABR_DEADBAND` | `0.15` | Relative departure from the setpoint that must be exceeded before any retarget, `(0, 1)`. Operator tuning knob; default matches the previous hardcoded value. Settable via Host Settings since 2026-08-18. |
| `QUASAR_ABR_MAX_UP_STEP` | `0.25` | Max fractional increase per up-step, `>0`. **2026-08-18 flip** (was `0.10`; T3 `abr_up_fast` — post-impairment setpoint ramp 37.8s → 14.0s, client-present drops 12.2% → 1.7%, 0 oscillations: `docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md`). Settable via Host Settings since 2026-08-18. |
| `QUASAR_ABR_MIN_INTERVAL_MS` | `1000` | Minimum wall-time (ms) between retargets, both directions (anti-thrash), `>= 1`. **2026-08-18 flip** (was `2000`; moved together with `QUASAR_ABR_MAX_UP_STEP` above as the `abr_up_fast` arm). Settable via Host Settings since 2026-08-18. |
| `QUASAR_ABR_MAX_DOWN_STEP` | `0.125` | **`smooth` only.** Max fractional down-step per non-emergency retarget, `(0, 1)`. Operator tuning knob; default matches the previous hardcoded −12.5%. Settable via Host Settings since 2026-08-18. |
| `QUASAR_ABR_DOWN_DWELL_MS` | `7000` | **`smooth` only.** Minimum wall-time (ms) between non-emergency downshifts, `>= 0` (`0` disables the dwell). Emergency drops on confirmed congestion bypass it. Operator tuning knob; default matches the previous hardcoded 7 s. Settable via Host Settings since 2026-08-18. |
| `QUASAR_ABR_CLIFF_GUARD_FRAC` | `0.50` | **`smooth` only.** Under fresh encoder saturation, GCC alone never drives the setpoint below this fraction of the current setpoint in one move, `(0, 1)`. Operator tuning knob; default matches the previous hardcoded 50%. Settable via Host Settings since 2026-08-18. |

#### Ladder hysteresis tuning (advanced)

Operator tuning knobs for the SPT-08 smoothness ladder
(`node-agent/src/session/ladder.rs`). Only take effect when the ladder is engaged
(`smooth` mode + `QUASAR_ABR_LADDER` on). Every default matched the previously
hardcoded value **except `QUASAR_ABR_LADDER_RECOVER_DWELL`, flipped 2026-08-18**
(see below). Invalid values warn once and fall back to the default.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ABR_LADDER_MAX_BIAS` | `2` | Max encoder-speed-bias rungs, `0..=255` (`0` = the rung is inert). Operator tuning knob; default matches the previous hardcoded value. |
| `QUASAR_ABR_LADDER_ENGAGE_DWELL` | `2` | Consecutive `encoder_saturated` windows required before stepping a rung up, `>= 1`. Operator tuning knob; default matches the previous hardcoded value. |
| `QUASAR_ABR_LADDER_RECOVER_DWELL` | `2` | Consecutive `healthy` windows required before stepping a rung down, `>= 1`. **2026-08-18 flip** (was `4`; T3 `ladder_recover_fast`, alongside `QUASAR_ABR_LADDER_RES_RECOVER_DWELL` below: `docs/reports/2026-08-18-overnight-optimisation/t3-ladder-step-recovery.md`). |

#### Adaptation classifier tuning (advanced)

Operator tuning knobs for the SPT-03 live adaptation classifier
(`node-agent/src/session/adaptation.rs`), which labels each metrics window
(`healthy` / `network_congested` / `encoder_saturated` / `unknown`). **Every default
matches the previously hardcoded value.** Each is a fraction in `(0, 1]`; invalid or
out-of-range values warn once and fall back to the default. These change only the
telemetry label and (in `smooth` mode) which bottleneck the governor/ladder react to;
they never bypass the #68 emergency descent.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_ADAPT_ENCODE_BUDGET_FRAC` | `0.70` | Fraction of the per-frame encode budget (`1000 / target_fps`) at/above which the encoder counts as "near budget". Operator tuning knob; default matches the previous hardcoded value. |
| `QUASAR_ADAPT_FPS_STEADY_FRAC` | `0.85` | Fraction of target fps below which realized fps counts as "not steady". Operator tuning knob; default matches the previous hardcoded value. |
| `QUASAR_ADAPT_GCC_BELOW_FRAC` | `0.85` | Fraction below the setpoint the GCC estimate must fall to count as the network-congestion signal. Operator tuning knob; default matches the previous hardcoded value. |
| `QUASAR_ADAPT_SEND_AT_CAP_FRAC` | `0.85` | Fraction of the setpoint the realized send bitrate must reach to count as "sending at the cap". Operator tuning knob; default matches the previous hardcoded value. |

> Known limitation: on the **NVENC** path the encoder can overshoot the ABR
> setpoint on high-entropy content — tracked in issue #182.

---

## Node agent — app container & runtime

Read in `node-agent/src/session/container.rs` / `audio.rs`. The `QUASAR_APP_*`
trio is the **dev/standalone** launch path; on a control-plane assignment the
app's catalog `runtime_spec` (image/args/env/mounts/gpu) is used instead.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_CONTAINER_RUNTIME` | `docker` | Container CLI, e.g. `podman`. The agent shells out to it via the host socket. |
| `QUASAR_CONTAINER_NETWORK` | `none` | Host-wide fallback `--network` for app containers, applied only when the app itself states none. **Prefer the per-app knob below** — the network is an app requirement, so setting it here to fix one title (Steam sign-in/downloads) opens the network for every app on the host. Accepted: `none` \| `bridge` \| `host`; anything else fails the session with a named error rather than being handed to the runtime. **This is the only place `host` can be selected**, deliberately: it is set by the operator of one specific machine and travels nowhere. `--network host` removes the container's network namespace — the app then reaches every service on host loopback (control plane, Postgres, any admin-only port) and can bind host ports — so it is a host-administration decision, not an app property. |
| *(per-app)* `runtime_spec.network` / preset `network` | inherit | Not an env var — the per-app container network (first-run experience §S2). Resolved at launch as **app `runtime_spec.network` → its runtime preset's `network` column → `QUASAR_CONTAINER_NETWORK` → `none`**. Accepted at every layer: `""` (inherit) \| `none` \| `bridge`. **`host` is refused here even though the env knob above accepts it** — these values are portable (a preset is materialized from a catalog image manifest authored elsewhere), so an app-authored `host` would dissolve container network isolation on every host that installs the image. A rejected value is a 400 from the admin preset API, a failed image install from a manifest `runtime` block, a failed launch from an app's `runtime_spec`, and a failed session at the agent. Steam's catalog image declares `bridge` because its first boot must download `steamui.so` — without it the app clean-exits and the session surfaces as "media path interrupted" (#463). |
| `QUASAR_APP_PUID` | unset | Run-as **user** id for app containers, forwarded as `PUID` (not docker `--user`, which would bypass the images' root init). The quasar-images base entrypoint starts as root, then drops to `PUID`/`PGID`. Unset ⇒ image default (unchanged). Unraid convention: `99`. An app-catalog `PUID` in the app's `runtime_spec.env` overrides this host default. |
| `QUASAR_APP_PGID` | unset | Run-as **group** id for app containers, forwarded as `PGID` (see `QUASAR_APP_PUID`). Unraid convention: `100`. |
| `QUASAR_APP_SHM_SIZE` | `1g` | `--shm-size` for app containers. Docker's 64 MB default breaks Chromium-embedding apps (Steam's UI renders black/flashing on failed GPU command buffers). shm is tmpfs — allocated on use, so the roomy default is free for small apps. |
| `QUASAR_APP_STOP_TIMEOUT_SECS` | `10` | Graceful-stop window for app containers: session teardown runs `docker stop -t N` (SIGTERM, SIGKILL after N s) before the `rm -f` backstop, so apps can exit cleanly (Steam otherwise marks its install unclean and re-verifies every executable checksum on the next launch, ~9 s). `0` restores kill-only teardown. Well-behaved apps exit on TERM immediately, so the window costs nothing for them. |
| `QUASAR_SWAP_APP_READY_TIMEOUT_MS` | `45000` | How long a quick-switch (session app swap) waits for the **replacement app** to actually present a frame before rolling back. The swap is serialised (2026-08-05): the outgoing app container is stopped and reaped first — both generations bind-mount the same managed home, and a second instance of a home-locking app (Steam) hands off to the first and exits 0 — so this budget covers container start + app startup, not just a first frame. Generous by default because a Steam-derived image needs 4.5–10.6 s just to reach its ready gate (#384) and a cold image pull is on top. A separate, fixed 20 s budget covers the earlier compositor-startup phase. Non-numeric or `0` ⇒ the default (a typo must not make every swap fail instantly). Raise it for very slow titles; lowering it only makes rollback happen sooner. |
| `QUASAR_APP_MOUNT_ALLOW` | unset (managed-home root only) | Comma-separated list of host directories an app's `runtime_spec.mounts` / preset `mounts` may bind on **this** host, each optionally suffixed `:rw` (default read-only, and an entry's `:ro` is honoured verbatim). The managed-home root (`QUASAR_HOME_ROOT`, or the per-host root pushed from Admin → Hosts) is always allowed read-write and needs no entry, so the shipped catalog — whose images mount nothing else — works with this unset. Enforced by the node agent at assign and at app swap, because it is the agent that spawns the container and a mount string originates in a manifest authored on another machine; a mount naming anything else fails the assign with `session-assign-rejected` and nothing is spawned. A **deny list beats this allowlist**: `/`, `/proc`, `/sys`, `/dev`, `/etc`, `/root`, `/boot`, `/run`, `/var/run`, `/var/lib/docker` (and the other container-runtime state dirs), `/lib/modules`, any directory containing a container-runtime socket, and any source containing `..` are refused whatever is listed here — binding any of them hands the session the host daemon and therefore host root. Sources are matched component-wise, so `/opt/games` does not allow `/opt/gamesecret`. The control plane applies the same deny list at image install and at admin preset writes (400), but that is a second line only: the host decides which of its own paths a session sees. |
| `QUASAR_APP_PRIVILEGE_OPTOUT` | `allow` | Whether this host honours an app's `runtime_spec.no_new_privileges: false` and `runtime_spec.systempaths_unconfined: true` (both rows below). `deny` ignores both, keeping `no-new-privileges` on and `/proc`/`/sys` masked, and logs `token="app-privilege-optout-denied"` — for an operator running a catalog they do not author. It is not the default because the shipped catalog needs both (Steam re-escalates via `sudo`, KDE needs an unmasked `/proc` for `bwrap`), so denying by default would break the default library out of the box; an unrecognised value warns and stays permissive rather than silently hardening a working host. |
| `QUASAR_APP_SECCOMP` | `unconfined` | Seccomp profile for app containers. Docker's builtin profile denies unprivileged user-namespace creation, which Steam's pressure-vessel (bwrap) requires — hence the GOW/Wolf-style `unconfined` default. `default` restores Docker's builtin profile; any other value is passed as a profile path. |
| `QUASAR_APP_APPARMOR_PROFILE` | unset (auto) | AppArmor profile for app containers, on hosts that enforce AppArmor (#76; an SELinux host gets no `apparmor=` flag whatever this says). Unset ⇒ the agent uses the scoped **`quasar-app`** profile (`deploy/apparmor/quasar-app`) when the host has it loaded, and `unconfined` when it does not — Docker's `docker-default` denies the mounts Steam's pressure-vessel and Flatpak's `bwrap` perform inside their user namespace, so plain `docker-default` is not an option. Loading the profile needs root **on the host** and the agent never does it: `sudo apparmor_parser -r -W <compose dir>/apparmor/quasar-app`, which `deploy/enroll-host.sh` runs at enrollment. The agent reads the loaded-profile list through the `/sys/kernel/security` bind in `deploy/docker-compose.yml`; without that mount it cannot tell and stays unconfined. Set to `unconfined` to force the pre-profile behaviour for a title the profile breaks, or to another profile name your host loads itself (asserted, not verified — a name that is not loaded makes the container runtime refuse every launch). Reported on the host's readiness card as `app_apparmor_profile`. |
| *(per-app)* `runtime_spec.no_new_privileges` | `true` | Not an env var — an additive boolean key in an app's `runtime_spec` (admin app catalog). `false` drops `--security-opt no-new-privileges` for that app only: upstream GOW desktop images (e.g. `ghcr.io/games-on-whales/xfce`) `sudo` inside their startup scripts, which the flag turns into a container exit 1 — the session then streams the bare (black) compositor. Leave the default for everything else. A host can refuse this opt-out with `QUASAR_APP_PRIVILEGE_OPTOUT=deny`. |
| *(per-app)* `runtime_spec.systempaths_unconfined` | `false` | Not an env var — an additive boolean key in an app's `runtime_spec` (admin app catalog). `true` adds `--security-opt systempaths=unconfined` for that app only, unmasking `/proc`/`/sys` paths Docker hides by default. Needed for desktop-session images (KDE Plasma with a user Flatpak install): Flatpak's sandbox helper (`bwrap`) mounts a fresh `/proc` inside the app's own mount namespace, which Docker's masked paths block even with `seccomp=unconfined` already set — `flatpak install` works today, `flatpak run` does not without this (live-verified 2026-08-13). Leave the default off for everything else. A host can refuse this opt-out with `QUASAR_APP_PRIVILEGE_OPTOUT=deny`. |
| *(per-app)* `runtime_spec.mounts` / preset `mounts` | `[]` | Not an env var — the host paths an app binds. Both the control plane (image install, admin preset write) and the node agent vet them; what a given host actually permits is `QUASAR_APP_MOUNT_ALLOW` above, which is default-deny apart from the managed-home root. |
| *(per-app)* `runtime_spec.on_app_exit` | `fail` | Not an env var — an additive string key (`"fail"` \| `"keep"`) in an app's `runtime_spec` (admin app catalog). App-liveness: policy for a steady-state app-container exit (crash, OOM, or a clean quit), detected via a dedicated `docker wait` on the container. `fail` (default) ends the session — a dead app streaming a stale frame forever is the bug this closes. `keep` logs the exit and lets the session continue; set it explicitly on catalog rows whose app legitimately exits mid-session (e.g. a console/local_only desktop process). A container torn down by Quasar itself (session stop, launcher↔game swap) is never misclassified as an app exit either way. |
| `QUASAR_APP_EXIT_POLICY` | `fail` | Host default for `runtime_spec.on_app_exit` on the dev/standalone `QUASAR_APP_*` launch path (`from_env`). `keep` restores the pre-liveness behaviour of ignoring app exits; any other value (including unset) is `fail`. A catalog app's own `runtime_spec.on_app_exit` always wins on a control-plane assignment — this only affects the direct demo/dev path. |
| `QUASAR_GPU_NVIDIA` | off | `1`/`true` → NVIDIA passthrough (`--gpus all` / CDI); otherwise `--device /dev/dri` (AMD/Intel). |
| `QUASAR_NV_LIB32_PATH` | unset (auto-detect) | #375: host directory holding the **32-bit** NVIDIA driver libs (e.g. `/usr/lib` on unraid), bind-mounted read-only into NVIDIA app containers at `/opt/quasar/nvidia-lib32` so native 32-bit Linux titles resolve `libGLX_nvidia.so.*` (the container ships only 64-bit driver libs; the container toolkit/CDI spec never injects 32-bit). Must be empty or an absolute path. Empty ⇒ the agent auto-detects at startup via a short-lived probe container that globs the host `/usr` (`busybox`/`alpine`), and failing that falls back to the `lib32/` half of the Quasar-provisioned driver volume (`QUASAR_NVIDIA_DRIVER_VOLUME`) — which reuses this exact mount mechanism, just pointed at the volume's host path; if both fail (no network on a locked-down host), set this explicitly. Also a per-host `nvidia_lib32_path` admin knob (live-class); the override wins over auto-detect. **NVIDIA-only** — inert on VA/AMD hosts. Requires the quasar-images `ld.so.conf.d` entry for `/opt/quasar/nvidia-lib32` (the mount deliberately avoids GOW's `/usr/nvidia` driver-volume path — upstream GOW images' cont-init treats that as a full driver volume and exits 1 when it isn't one). |
| `QUASAR_NVIDIA_DRIVER_VOLUME` | `1` (on) | First-run S1: Wolf-style **NVIDIA driver-volume auto-provisioning**. When the host readiness probe reports the NVIDIA graphics gap (no EGL vendor json / no `libnvidia-eglcore` / no 32-bit GL — the CUDA-only install of #462) the agent downloads `https://download.nvidia.com/XFree86/Linux-x86_64/<ver>/NVIDIA-Linux-x86_64-<ver>.run` for the **loaded kernel-module version** (`/sys/module/nvidia/version`), runs it `--extract-only` (**never** `--install`; nothing is written outside the volume), and populates the named volume `quasar-nvidia-driver` (`lib64/`, `lib32/`, `glvnd/egl_vendor.d/10_nvidia.json`, `egl_external_platform.d/`, `vulkan/icd.d/nvidia_icd.json`, `gbm/`, `ld.so.conf.d/`, `manifest.json`). Precedence: a host with its own graphics driver (CDI injection) **never** provisions. Re-provisions when the host driver version changes; concurrency-guarded by a lockfile in the volume. After a successful provision that closed an **EGL** gap the agent restarts itself (the dynamic loader latches `LD_LIBRARY_PATH` at exec); a 32-bit-only gap takes effect on the next session launch with no restart. Progress is reported on the readiness card as the additive `provisioning` status and logged on the `quasar.nvidia_volume` tracing target. **A virgin NVIDIA deploy therefore logs an INFO `vulkan-codec-plan-pending-driver-volume` line on its first boot** — the Vulkan ICD isn't visible to the GStreamer registry scan until the volume is provisioned — and only re-probes as the healthy `vulkan codec plan: h264=vulkan, …` line after the self-restart above; a WARN `vulkan-codec-plan-degraded` on a **later** boot (volume already adopted) means a codec's vulkan element is genuinely missing from the image and is worth investigating. The volume carries **vendor libraries only** — the installer's own vendor-neutral glvnd dispatch copies (`libEGL.so.*`, `libGLdispatch.so.*`, `libGL.so.*`, `libGLX.so.*`, `libOpenGL.so.*`, `libGLESv*.so.*`, `libOpenCL.so.*`) are deliberately excluded, because with the volume on `LD_LIBRARY_PATH` they shadow the image's libglvnd and NVIDIA's legacy pre-glvnd `libEGL.so.<ver>` wins the `libEGL.so.1` SONAME, stripping `EGL_EXT_device_enumeration` and panicking the compositor. **Known limitation:** on driver 610.57.04 that same exclusion leaves NVIDIA's Vulkan ICD unusable (it only initialises when `libEGL.so.1` *is* NVIDIA's legacy library — mutually exclusive with what the compositor needs), so **`QUASAR_ENCODER=vulkan` is not supported on a driver-volume host**; NVENC and VA are unaffected, and this is not a regression (a CUDA-only host had no NVIDIA Vulkan ICD at all). Set `0` (or `false`/`no`) to keep the manual remediation path — appropriate for an air-gapped or bandwidth-capped host, or one that mirrors drivers internally. Requires `deploy/docker-compose.nvidia.yml` (it mounts the volume and sets `LD_LIBRARY_PATH`); without that mount the agent logs the gap and does nothing. **Safety rails (#475–#478):** the trigger is **file presence only** — the runtime EGL self-test can turn the readiness card red but can never start a download or the self-restart, so a slow/timed-out/killed self-test on a healthy host does nothing (a timeout is `Indeterminate`, not "broken"); the installer's sha256 is **verified before the `.run` is executed** — first against `REVIEWED_DRIVER_DIGESTS` in `node-agent/src/nvidia_volume.rs` (reviewed digests compiled into the agent; the only control that covers a *first* provision — see `docs/third-party-pins.md`), then against the per-host pins in `driver-digests.json` inside the volume, which refuse a changed digest for an already-accepted version (delete that file, or the volume, to re-pin; a **corrupt** pin file is itself a refusal, never an empty pin set). A version with neither pin is accepted on trust and pinned (WARN `drvvol-trust-on-first-use`) unless `QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE=0`, which refuses it with a message naming the staging and opt-out routes; a `statvfs` preflight refuses to start without ~3 GiB free in the volume's filesystem (the download plus the extracted tree land in the docker data root); scratch is removed on every exit path; and repeated **failures** back off (5 min doubling to a 6 h cap, tracked in `.provision-attempts.json`, reset on success or on a driver-version change) so an agent crash-looping for an unrelated reason cannot become a download loop. |
| `QUASAR_NVIDIA_DRIVER_RUN` | unset | Absolute path to an **operator-staged** NVIDIA `.run` installer inside the agent container, used instead of downloading one. The air-gapped hatch, and the answer to a driver version this agent carries no reviewed digest for: the file is the operator's to vouch for, so no reviewed digest is required (one that exists is still enforced, and a mismatch refuses). The file is copied into the volume's scratch before it is hashed, so it cannot be swapped between the check and the execution; its sha256 is then pinned per host exactly as a downloaded one is. Inert unless `QUASAR_NVIDIA_DRIVER_VOLUME` is on and the readiness probe reports the graphics gap. |
| `QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE` | `1` (on) | Governs what happens when the agent has **no** reviewed digest for a driver version and this host has **no** pin for it. On (the default) the agent executes the downloaded `.run`, accepting whatever the pinned origin returned over TLS, and pins that digest for the version from then on — every later fetch must match it. NVIDIA publishes no digest to review against, so refusing here would break first provision on exactly the CUDA-only hosts this feature exists to rescue. The payload is a shell script that runs with the agent's privileges (host networking, `NET_ADMIN`, devices, the docker socket), so it is logged at WARN with `token="drvvol-trust-on-first-use"`. Set `0`/`false`/`no` to refuse instead — the right posture once `REVIEWED_DRIVER_DIGESTS` covers the drivers you run (`docs/third-party-pins.md`), or when `QUASAR_NVIDIA_DRIVER_RUN` stages the installer. |
| `QUASAR_CUDA_RUNTIME` | `1` (on) | #545: **runtime provisioning of the CUDA userspace (NVRTC)** into `cuda/` inside the same `quasar-nvidia-driver` volume. This is what registers `cudaconvert` / `cudaconvertscale` / `cudascale` / `cudacompositor`, and therefore what makes the per-session **NVENC fallback** (`nvcuda<codec>enc`, taken when a codec's Vulkan knob is off or its Vulkan element is missing) able to build a pipeline at all. It exists because `libnvrtc` is CUDA *toolkit* userspace, not driver userspace — no driver `.run` contains it, so `QUASAR_NVIDIA_DRIVER_VOLUME` cannot produce it — and carrying that one ~115 MB library was the entire reason a separate `quasar-nv` image lineage existed. The agent downloads NVIDIA's **pinned** redistributable (version + published sha256 are compile-time constants in `node-agent/src/cuda_runtime.rs`; unlike the driver `.run`'s trust-on-first-use pin this is verified on the very first provision), places only `libnvrtc*.so*` + `libnvrtc-builtins*.so*` (never the `*_static.a` archives, never `lib/stubs/`), runs `ldconfig -n` for the SONAME links, and creates the **unversioned `libnvrtc.so` symlink** that `gstcuda` dlopens — the trap `Dockerfile.nv` documented and #384 rediscovered. Own manifest keyed on the **NVRTC version**, own lock and own backoff counter, in a sibling directory of the driver's `lib64/`, so a driver re-provision (which clears `lib64/`+`lib32/`) leaves it alone. **Driver gate:** only fetched when `/sys/module/nvidia/version` is **r580 or newer** — CUDA 13 NVRTC needs that, and a *failing* NVRTC is worse than an absent one (the four elements would register and then break a live session). **Every failure path is soft:** refusal, download failure and `0`/`false`/`no` all leave the host exactly as the universal image finds it — no `cuda*` elements, Vulkan encode (the NVIDIA default) unaffected — and none of them can block registration or a launch. Logged on `quasar.cuda_runtime` (and `quasar.artifact` with `artifact="cuda-nvrtc"` for the download); grep `token="cudart-provision-failed"` or `token="cudart-ld-library-path-missing"` when the elements are missing. If the libraries land after the process has already scanned the GStreamer registry, the agent restarts itself once (`token="cudart-agent-restart-scheduled"`) — plugin features are registered at scan time and cannot be added in place. |
| `QUASAR_CUDA_RUNTIME_DIR` | unset | Air-gapped escape hatch for `QUASAR_CUDA_RUNTIME`: an absolute path to a directory that already holds `libnvrtc*.so*` (e.g. unpacked from NVIDIA's redistributable by hand, or copied out of a CUDA install). The agent uses it as the **source** — no download, and no digest check, since the operator staged it — and otherwise does exactly the same placement. It is a source and not a destination on purpose: `LD_LIBRARY_PATH` is latched at `execve`, so a hatch that merely *named* a directory would also need a compose edit and would fail silently without one; copying into the volume reuses the path compose already sets. The r580 driver gate still applies (a pre-staged CUDA 13 NVRTC is just as unusable on an older driver). |
| `LD_LIBRARY_PATH` | set by `docker-compose.nvidia.yml` | Prefixed with `/opt/quasar/nvidia-driver/lib64` **and** `/opt/quasar/nvidia-driver/cuda/lib64` **unconditionally** on NVIDIA hosts (driver userspace and runtime-provisioned NVRTC respectively — see `QUASAR_CUDA_RUNTIME`). Safe when the driver volume is empty — a nonexistent/empty directory in `LD_LIBRARY_PATH` is inert. It must come from compose rather than the agent process because glibc latches the value at `execve`; `setenv` at runtime changes nothing about `dlopen`. Any operator value from `.env` is appended after it. |
| `QUASAR_PULSE_IMAGE` | follows the agent image | Image for the PulseAudio sidecar. Compose defaults it to whatever `QUASAR_AGENT_IMAGE` (or the legacy `QUASAR_NODE_IMAGE`) resolves to, on every host: a sidecar from another lineage may have no `pulseaudio` binary and silently mutes every session. |
| `QUASAR_APP_IMAGE` | unset | Dev path: image to launch as the app. |
| `QUASAR_APP_ARGS` | unset | Dev path: JSON array of the app's argv. |
| `QUASAR_APP_GPU` | off | Dev path: `1`/`true` → give the app container GPU access. |
| `QUASAR_APP_DISPLAY_ENV` | on | #384: inject the session's display mode (resolution + refresh) into every app container as env — see the injection note below. `0`/`false`/`no` injects nothing, so an app that cannot read the Wayland output falls back to its **image default** and the selected profile has no effect inside the container (the #384 bug, kept as an escape hatch). The session trace records `app_display.source = "disabled"` when off. |
| `QUASAR_APP_GAMESCOPE_ENV` | on | #384: also emit the `GAMESCOPE_WIDTH`/`GAMESCOPE_HEIGHT`/`GAMESCOPE_REFRESH` compatibility shim. `0`/`false`/`no` emits only the `QUASAR_STREAM_*` contract — set it once every app image reads those. Ignored when `QUASAR_APP_DISPLAY_ENV` is off. |

The agent also **injects** the session's **display mode** into every app container
(#384): `QUASAR_STREAM_WIDTH`, `QUASAR_STREAM_HEIGHT`, `QUASAR_STREAM_FPS` (the
generic contract images should read) plus `GAMESCOPE_WIDTH`, `GAMESCOPE_HEIGHT`,
`GAMESCOPE_REFRESH` (a shim for images that only read gamescope's own variables —
a runtime `-e` overrides the image `ENV`, so it works without an image rebuild).
The compositor's `wl_output` already advertises the mode, but an app that never
reads the Wayland output — most importantly a **nested gamescope**, which sizes its
virtual output from `-W/-H/-r` — has no other way to learn it, and would otherwise
render at its own baked default and be upscaled into the stream. An app-catalog
`runtime_spec.env` entry **wins per key**, so an app deliberately pinned to a mode
keeps it. The launched values (and which of these three sources supplied them) are
reported in the `session.effective_media` trace event as `app_display`, visible in
the admin session detail and the diagnostic bundle.

The agent also **injects** three PulseAudio env vars into every app container it
launches (not operator knobs — they point at the per-session sidecar):
`PULSE_SERVER` (the sidecar's Unix socket), `PULSE_COOKIE` (the shared auth
cookie), and `PULSE_SINK=quasar_output` (the session's baked-in null-sink, so a
client picking a sink by name routes to it). `PULSE_SINK` is only defaulted when
the app's catalog `runtime_spec.env` does not already set one.

---

## Node agent — console / local display

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_LOCAL_DISPLAY` | unset (off) | **Dev-only fallback.** Any non-empty value enables the local-display leg (weston + `waylandsink`, CM-01) when the session has **no** `console_config` pushed from the control plane — a real operator console session is driven by `console_config.enabled` instead (admin UI → host → Console; see below), which this env never overrides. The env path is also best-effort/soft-skip on failure, unlike a real console session's fail-loud requirement. Dev/standalone launch path only. |
| `QUASAR_CONSOLE_DDC` | on (any value other than `0`, including unset) | CM-09 monitor **power** detection via DDC/CI (VCP `0xd6`) — distinguishes a monitor actually powered off from one merely idle, which DRM connector status alone cannot (an off panel still reports `status=connected`). `QUASAR_CONSOLE_DDC=0` disables the DDC/CI probe entirely; any other value (or unset) leaves it enabled **if** the `ddcutil` binary is present on the host — a missing binary silently degrades the whole module to the always-powered/physical-connected behaviour, byte-identical to disabling it. |
| `QUASAR_EXPERIMENTAL_LOCAL_DMABUF` | off | **Experimental, local-display-only.** `1`/`true`/`TRUE` (case-sensitive on the latter two — lowercase `true` and exact `1` match, `TRUE` matches, but e.g. `True` does not) switches the encoder-free local-display leg from ordinary system-memory buffers to DRM PRIME DMABuf, matching what `waylandsink` can import directly. Only takes effect when the session's `video_topology` is `LocalOnly` and it is not using the synthetic test source (`QUASAR_USE_TEST_SRC`); otherwise this knob is inert. |

---

## Node agent — lifecycle & test toggles

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_HEALTH_ADDR` | `127.0.0.1:9091` | Bind address for the liveness/health HTTP listener (`GET /health` → `200 {"status":"ok","sessions":<n>,"connected":<bool>}`). Empty string or `"0"` disables the endpoint entirely — a hand-rolled dependency-free TCP listener (`node-agent/src/health.rs`), not a framework. `sessions` is the agent's current running-session count; `connected` reflects the control-plane WebSocket lifecycle. Wired into the prod image `HEALTHCHECK` (`deploy/Dockerfile.agent.prod`, and `deploy/Dockerfile.vulkan`'s `HEALTHCHECK ... curl -fsS http://127.0.0.1:9091/health`). **#519:** after 5 consecutive failed connect/register cycles with no intervening successful registration (a rejected `ENROLLMENT_TOKEN`, or a control plane that stays unreachable), the endpoint flips to `503 {"status":"unhealthy","sessions":<n>,"connected":<bool>,"consecutive_registration_failures":<n>,"reason":"<last error>"}` — `curl -f` treats any non-2xx as a failed probe, so this is what turns into `docker compose ps` reporting the container unhealthy. The streak resets to 0 (and the endpoint returns to `200`/`"ok"`) the moment a `Registered` response arrives; a purely transient blip (control plane restarting) recovers before the threshold and never flips health. This is process-local health only — it does not touch `protocol/agent-api.md`. |
| `QUASAR_IDLE_TIMEOUT_SECS` | `120` | Idle-session reap window in seconds. `0` disables reaping. |
| `QUASAR_APP_BOOT_TIMEOUT_SECS` | `300` | How long a launched app container may take to present its first frame before the session fails with `app_never_presented`. `0` disables the watchdog. Only applies to sessions that launch an app container. |
| `QUASAR_USE_TEST_SRC` | off | `1`/`true` → synthetic `videotestsrc` instead of the compositor (smoke tests). |
| `QUASAR_USE_TEST_AUDIO` | off | `1`/`true` → synthetic audio (also implied by `QUASAR_USE_TEST_SRC`). |
| `QUASAR_AUDIO_REQUIRED` | off | `1`/`true` → an unavailable PulseAudio sidecar **fails** the session instead of silently degrading to silent audio. Off by default so hosts with no audio stack (headless CI, smoke tests) still run; **release deployments should set it**. Regardless of this knob, a degraded session now reports `effective_media.audio.{path,degraded,reason}` and emits an `audio.degraded` trace event — before 2026-07-26 the fallback was a single WARN line and the session still reported `running`, so a sidecar image with no `pulseaudio` binary muted every Tower session (streamed *and* console-local, since local audio also captures from the sidecar via `pulsesrc`) for days with nothing surfacing it. |
| `QUASAR_WIDTH` / `QUASAR_HEIGHT` | `1280` / `720` | Dev-path stream size (overridden per-assignment). |
| `QUASAR_FPS` | `60` | Dev-path frame rate (overridden per-assignment). |
| `QUASAR_BITRATE_KBPS` | `8000` | Dev-path target bitrate, kbit/s (overridden per-assignment). |
| `QUASAR_MALLOC_TRIM` | `0` (off) | `1`/`true`/`yes` → run glibc `malloc_trim(0)` at each session teardown and log `(freed_anything, rss_kib_after)`; off is a no-op with nothing logged. `cfg(target_os = "linux", target_env = "gnu")`-gated (a no-op build on other targets). Also surfaced read-only in the startup env-dump line (`node-agent/src/memstat.rs`) alongside `MALLOC_ARENA_MAX`/`MALLOC_TRIM_THRESHOLD_`/`MALLOC_MMAP_THRESHOLD_`, which are plain glibc tunables Quasar does not set defaults for. |

---

## Node agent — input

Relative-mouse motion is coalesced agent-side into one uinput write per
~1 ms window (Moonlight-derived batching), with a fractional remainder carried
across `mm` messages so sub-pixel motion converges to the true sum. The input
DataChannel is reliable+ordered by default. See `protocol/input.md` for the wire
shapes and `CLAUDE.md` (GStreamer gotchas) for the design rationale.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_INPUT_BATCH_MS` | `1` | Relative-mouse batching window in ms. `0` = disabled (per-arrival uinput writes; fractional accumulation still active). |
| `QUASAR_INPUT_CHANNEL_MODE` | unset (reliable+ordered) | `legacy` / `unreliable` / `unordered` / `unreliable-unordered` → restore the Phase-0 unreliable+unordered `"input"` DataChannel for A/B. |
| `QUASAR_APP_PIDS_LIMIT` | `8192` | Fork-bomb backstop: max processes/threads per app container. 512 (the former default) strangled a full Steam client — Steam + a game's threads + shader-compile workers exceed it, `pthread_create` fails, and the game aborts. 8192 still bounds a runaway while fitting real games. |
| `QUASAR_APP_READ_ONLY` | unset | Set to `1`, `true`, or `yes` to launch app containers with a read-only root filesystem. Validate the catalog first and provide explicit writable mounts/tmpfs where required. |
| `QUASAR_INPUT_TRACE` | off | `1`/`true` → per-`mm` `{seq, tc, recv_ms, inject_ms, dwell_ms, dx, dy}` debug logging under the `quasar.input.trace` target. The browser attaches `seq`+`tc` when `?itrace=1` is in the URL. |
| `QUASAR_INPUT_CONTROLLER_NUDGE` | on | `0` / `false` (case-insensitive) disables. **BPM controller-focus heal:** on the first gamepad (`gp`) event of a session that has seen no real client mouse-motion, the agent injects one net-zero `±1px REL_X` pointer jiggle into the session's virtual mouse — Steam Big Picture under nested gamescope suppresses gamepad focus promotion on a fresh game-detail route until the cursor has entered the window via the real uinput→compositor path, and this jiggle heals it (grey/unselectable Play, dead B). Fires at most once per app process (re-armed on launcher↔game swap). **Any** `gp` frame triggers it — including a neutral controller-presence frame with no buttons/axes pressed; this is intentional (the presence of controller input is the signal). Imperceptible; app/vendor-neutral. Applies per session (probed once per process). |

> **Live vs restart:** `QUASAR_INPUT_BATCH_MS` and `QUASAR_INPUT_CHANNEL_MODE`
> are read at `VirtualDevices::create` (per session), so a change applies on the
> **next session** — no agent restart needed. `QUASAR_INPUT_TRACE` is probed
> lazily on first `mm`, so it too applies without a restart (toggling mid-session
> is not supported; start a new session to change it).

---

## Node agent — audio diagnostics (#304)

Two diagnostic flags for investigating browser A/V clock coupling (issue #304).
Both are purely diagnostic and default off. See the issue for the Phase 0
validation methodology and measured results.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_AUDIO_DISABLED` | off | `1`/`true` → strip the audio m-line from the WebRTC offer entirely (video + DataChannel only). Measured: browser g2g p95 drops ~15ms (24→9ms on hermes VA), confirming Chrome's A/V sync inflates video playout. |
| `QUASAR_AUDIO_NO_CLOCK` | off | `1`/`true` → set pulsesrc `provide-clock=false` + `do-timestamp=false` to decouple the audio clock from the pipeline clock domain. Measured: ~2ms g2g improvement — clock skew is a minor contributor vs A/V sync. |
| `QUASAR_MIC_DISABLED` | off | `1`/`true` → host-level **emergency off** for microphone capture (client → host). Every session on this host then behaves as if `session_assign.stream.mic` were false: no `recvonly` Opus transceiver, no mic m-line in the audio offer, no receive bin — `effective_media.mic` reports `off`. Per-session activation is the control plane's (`instance_settings.mic_capture_enabled` ∧ the launch request's `mic`); this knob only ever subtracts. Note the mic m-line rides the audio PeerConnection, so `QUASAR_AUDIO_DISABLED=1` (or a topology with stream audio off) already implies no microphone. The sidecar's mic devices (`quasar_mic` sink + `quasar_mic_src` remapped source) are loaded unconditionally either way — they are simply silent. |
| `QUASAR_MIC` | off | Dev/demo only: `1`/`true` forces `mic` on for the **env-configured** session path (`SessionConfig::from_env`), so the receive path can be exercised without a mic-aware control plane. Ignored on the agent/control-plane path, where `session_assign.stream.mic` is the source of truth. |
| `QUASAR_MIC_JITTER_MS` | `60` | **(#425)** The AUDIO PC's `webrtcbin` `latency` property (rtpbin jitter-buffer target, milliseconds), applied to the **audio webrtcbin only** (the video PC's buffering is untouched). Since the audio PC's only receive direction is the microphone (host→client audio is send-only on that PC), this is effectively the mic receive-leg jitter buffer. webrtcbin's built-in default is 200 ms, which dominated the ~250 ms round-trip perceived on the Steam mic-loopback test; `60` sits inside the issue's 50-75 ms target (250 ms expected to drop to ~100 ms). Any positive integer is accepted, including outside that range (treated as a deliberate operator experiment, not junk); unset, empty, unparseable, `0`, or negative falls back to `60` with a warn log (`token="knob-invalid-mic-jitter-ms"`). A lossy WAN link risks voice crackle/gaps at a small buffer, so validate with the netem harness before lowering it further. Live-verify: the agent logs `token="mic-jitter-latency-applied"` ("audio webrtcbin latency = N ms ...") once per session with an audio PC, from `set_audio_receive_latency` (`node-agent/src/session/pipeline/webrtc.rs`); a build whose webrtcbin lacks the `latency` property instead logs `token="mic-jitter-latency-unavailable"` and mic receive latency stays at the 200 ms default. |

> **Live:** all five are read at pipeline build (per session), so a change applies
> on the **next session** — no agent restart needed (but the compose
> `environment:` passthrough must include them; see `deploy/docker-compose.yml`).

---

## Node agent — on-demand capture (`session_capture`)

An admin can ask a **running** session a bounded question and get one answer back:
the encode graph as graphviz (`pipeline_dot`), the encoder's live allow-listed
properties and negotiated caps (`encoder_props`), or a short burst of telemetry at
sub-heartbeat resolution (`burst_stats`). The control plane addresses it over the
admin API; the agent runs it on the session runner's existing 100 ms supervision
tick and returns the result as a `session_trace_event` with event `diag.<kind>`.

**There are no environment variables for this.** Everything is per-request and
carried on the wire, deliberately: a capture is an admin action with an audit
trail, not a host posture, and a host-wide knob would either be a footgun (leave
it on and every session pays) or dead weight (leave it off and the admin API
lies). The caps below are compiled-in floors and ceilings the agent applies to
whatever the control plane asks for.

| Knob | Where it comes from | Default | Clamped to |
|---|---|---|---|
| `budget.max_bytes` | the `session_capture` message | 256 KiB | ≥ 1 KiB. The **compressed** payload never exceeds it; an over-cap result is truncated (text at a line boundary, JSON by dropping raw samples and then sub-windows) and reported with `truncated: true` + `original_bytes`. |
| `budget.max_ms` | the `session_capture` message | 10 s | 100 ms … 60 s. A capture still running at the deadline emits what it has with `error: "deadline"`. |
| `params.windows` | `burst_stats` only | 20 | 1 … 40, and further reduced so `windows × window_ms ≤ budget.max_ms`. |
| `params.window_ms` | `burst_stats` only | 250 | 100 … 1000 ms. Sampling rides the 100 ms runner tick, so a sub-100 ms window is not representable. |

Raw telemetry samples per sub-window are capped at 200 per series; the summary
percentiles always cover the whole window, whatever that cap drops.

**What a capture can never contain**, whatever is asked for: pixel, audio or
bitstream content; input events or microphone data; the environment read in bulk;
the node secret or an enrollment token; file paths outside the session scratch.
`pipeline_dot` uses the `CAPS_DETAILS | STATES` graph detail flags only —
`NON_DEFAULT_PARAMS` / `FULL_PARAMS` render element property *values* into the
graph and are banned. `encoder_props` reads an allow-list of property *names*
(`session::capture::ENCODER_PROP_ALLOW`), so a future encoder element cannot leak a
new property by default, and any string value over 256 characters is replaced by
its length. The one environment variable a capture reads is `WOLF_VULKAN_RING`, by
name, because the compositor's ring pin is a process-global the pipeline cannot
report back. Each of these rules has a negative test in
`node-agent/src/session/capture/tests.rs` — they are the contract, not a
description of the current implementation.

One capture at a time per session: a second request while one is in flight is
refused (`busy`), never queued. A session with no encode pipeline (a local-only
console session) refuses every kind with `unsupported`. Neither refusal touches
the session — a rejected capture is a pure no-op, and no capture can fail a
session.

Bitstream dumping stays what it always was: a build/operator knob on the host,
**not** a capture kind. It is not reachable through this surface — the knob is
`QUASAR_CAPTURE_BITSTREAM` below.

### Encode-path build/operator diagnostics (env, not `session_capture`)

Unlike the on-demand capture above, these are process-level env knobs read at
pipeline build time — set them on the host and restart, or bake them into a
diagnostic image. All default off/unset and cost nothing when unused.

| Variable | Default | Values / notes |
|---|---|---|
| `QUASAR_CAPTURE_BITSTREAM` | unset | Path to write the raw encoded bitstream (H.264/HEVC/AV1) the encoder produces, captured via a pad probe on the encoder's src pad — for offline decode / root-causing a decode failure, or the M2 codec-validate harness's strict ffmpeg decode gate (#260). Empty value is treated as unset. Wins over `QUASAR_CAPTURE_H264` if both are set. |
| `QUASAR_CAPTURE_H264` | unset | Legacy alias for `QUASAR_CAPTURE_BITSTREAM`, kept for existing tooling/scripts. |
| `QUASAR_DIAG_NO_OBS` | off | **Leak-bisection diagnostic, not for production use.** `1` skips attaching every agent-side probe/signal on the encode chain and stage probes (encode timing, frame/byte counters, abs-capture-time egress/verification probes) so per-session element leaks can be attributed to (or exonerated from) the observability layer itself. Any other value (or unset) leaves probes attached. With it on, `session_metrics` windows are empty. |
| `QUASAR_TRACE_ENC_PTS` | off | VK diagnostic: attaches a pad-probe trace of encoder input-vs-output PTS, to localise a scrambled-RTP-timestamp defect to before or after the encoder. Any non-empty value other than `0` enables it. |
| `QUASAR_TRACE_RTP_MARKER` | off | VK diagnostic: per-access-unit RTP marker-bit layout trace on the payloader's src pad, for confirming/refuting a Vulkan-vs-VA marker-bit difference on real content (the synthetic `videotestsrc` path can't reproduce `rtph264pay`'s real marker logic). Any non-empty value other than `0` enables it. |
| `QUASAR_TRACE_RTP_TS` | off | VK diagnostic: per-access-unit RTP timestamp-continuity and sequence-number-contiguity trace on the payloader's src pad — flags duplicate timestamps across an AU, non-1500 timestamp deltas, and sequence gaps. Any non-empty value other than `0` enables it. |

---

## System / GStreamer / GPU environment

Not Quasar-specific, but load-bearing for the media path (set in the run scripts /
compose). See `CLAUDE.md` for the full rationale.

| Variable | Typical | Notes |
|---|---|---|
| `XDG_RUNTIME_DIR` | `/tmp/runtime-quasar` (agent default) / `/run/quasar-agent` (compose) | Wayland + PulseAudio sockets. Must be a host bind-mount in compose so DooD sibling mounts resolve. |
| `MESA_LOADER_DRIVER_OVERRIDE` | unset for HW | `softpipe` forces software Mesa — **must stay unset** for VA/NVENC and a real render node, or the VA encoder disappears. |
| `LIBGL_ALWAYS_SOFTWARE` | unset for HW | Same: forces software GL; unset on any hardware-encode run. |
| `LIBVA_TRACE` | unset | Diagnostic only. libva treats mere PRESENCE as "tracing on" — an empty value is NOT "off" to libva itself (#94: a present-but-empty value from an unset operator var wrote 66GB of `.<pid>.thd-*` files, one per encode thread per session, into the container's writable layer and filled the disk). The entrypoint (`deploy/Dockerfile.vulkan`) and the agent binary both unset it when it arrives empty, mirroring `MESA_LOADER_DRIVER_OVERRIDE`/`LIBGL_ALWAYS_SOFTWARE` above. If you deliberately set it, point it at a bounded, volume-backed path — never a bare prefix in the container's own filesystem. |
| `GST_REGISTRY` | per-process (VA/NVENC) | Pointed at a fresh path so GStreamer rescans and registers the HW encoder for the GPU present at runtime. |
| `GST_PLUGIN_PATH` | `/usr/local/lib/.../gstreamer-1.0` | Lists the from-source plugins (nvcodec/waylanddisplaysrc/interpipe) first so they shadow the apt builds. |

Host kernel/network tuning (UDP `wmem_default`, etc.) lives in `deploy/host-tuning.md`.
The node agent's device and capability grants (`/dev/dri`, `/dev/uinput`, `/dev/kmsg`
read-only, `NET_ADMIN` + `SYSLOG`) are compose-level, not environment variables — they
are listed in `deploy/README.md` §"Prerequisites in detail", and each one is what makes
a specific readiness check answerable rather than `skip`.

---

## Deploy / compose-interpolation only

Consumed by `deploy/docker-compose*.yml` interpolation — **not** read by the app
code directly. Set in `deploy/.env`.

| Variable | Default | Notes |
|---|---|---|
| `CONTROL_PORT` | `8080` | Host port mapped to the control-plane's container `:8080`. Set it (with `QUASAR_TLS_PORT`) on a box whose low ports are taken — e.g. an unraid host uses 18080/18443. |
| `QUASAR_TLS_PORT` | `8443` | Host port for the HTTPS listener. Also becomes `QUASAR_TLS_REDIRECT_PORT`, so the plain-HTTP 308 redirect points at the right external port. |
| `POSTGRES_USER` | `quasar` | Interpolated into `DATABASE_URL`. |
| `POSTGRES_PASSWORD` | — (**required** in `.env`) | Interpolated into `DATABASE_URL`. |
| `QUASAR_AGENT_IMAGE` | `quasar-node-agent:latest` | Node-agent runtime image. Set it to a digest to pin a release — with `QUASAR_CONTROL_IMAGE` it is the entire release install, since `docker-compose.release.yml` was retired. `QUASAR_PULSE_IMAGE` follows it unless set explicitly. |
| `QUASAR_NODE_IMAGE` | unset | **Legacy alias for `QUASAR_AGENT_IMAGE`**, still honoured so existing hosts and harnesses keep working. `QUASAR_AGENT_IMAGE` wins when both are set. Use the new name in new configuration. |
| `QUASAR_POSTGRES_IMAGE` | `postgres:16-alpine` | Database image, pinnable to a digest like the two above. |
| `QUASAR_CONSOLE` | unset | `1` tells `deploy/redeploy.sh` to add `overlays/docker-compose.console.yml` to the chain. The overlay itself must also be in the host's `compose_files`. |
| `QUASAR_CORE_DIR` | — (**required** by `docker-compose.cores.yml`) | Host directory for core dumps. Must be a real filesystem: **the kernel cannot write a core to FUSE**, and a share path silently produces zero cores. |
| `QUASAR_CONTROL_IMAGE` / `QUASAR_AGENT_IMAGE` | the `:latest` local tags | Digest-pinned release artifacts. Setting both is what makes `deploy/docker-compose.yml` a pinned release install; unset, the stack runs the locally-built `:latest` images. `release-preflight.sh` rejects anything that is not `name@sha256:<64 hex>`. |
| `QUASAR_POSTGRES_VOLUME` / `QUASAR_AGENT_VOLUME` / `QUASAR_CONTROL_VOLUME` | unset | **Upgrades only, AND only take effect with `-f deploy/overlays/docker-compose.adopt-volumes.yml` in the compose chain** (#448: Compose v5 rejects an empty `name:` default on the base file at `up`, so the override moved to this opt-in overlay). `deploy/redeploy.sh` adds the overlay automatically when all three are set (and refuses a partial set); manual compose invocations must pass it explicitly. Unset, or the overlay omitted = Compose's normal `<project>_<key>` naming. Set (with the overlay) to adopt volumes that already exist under other names, so a stack that used to run a forked compose file keeps its data instead of starting against an empty database. `scripts/dev/migrate-compose-volumes.sh` prints the exact values and the invocation. |
| `QUASAR_UPDATER_IMAGE` | `quasar-updater:latest` | The updater's image. A **tag, not a digest**, on purpose: the updater is what applies a release, so it is not one of the images a release moves, and it is not in the release manifest. Registry installs set `ghcr.io/accreleus/quasar/quasar-updater:latest`; updating it is a separate manual step (`docs/upgrading.md`). |
| `QUASAR_STACK_DIR` | `/var/lib/quasar/stack-dir-unset` (a sentinel) | The stack directory's absolute **host** path, bind-mounted into the updater at that same path. Required for the updater to function: it rebuilds its compose invocation from its own container's compose labels, which record host paths, so the same absolute path must resolve inside the container. `deploy/redeploy.sh` seeds it and warns when a moved checkout makes it stale. Left at the sentinel, the updater refuses to serve and logs which label it could not resolve. |
| `QUASAR_DOCKER_SOCKET` | `/var/run/docker.sock` | Host path of the container-runtime socket mounted into the updater. Rootless Podman: `/run/user/<uid>/podman/podman.sock`. |
| `QUASAR_APP_IMAGE` | `quasar-agent-dev:latest` | Override for `scripts/dev/seed-benchmark-apps.sh` so seeded apps use the host's runtime image. |

---

## Updater (`quasar-updater`)

Read by the updater process (`control-plane/cmd/quasar-updater`); the control
plane also reads `QUASAR_UPDATER_SOCKET`, because it applies **itself** over
that socket rather than through any agent. Set in
`deploy/.env`; the compose service passes them through. The socket path and the
result-file layout are **not** a frozen interface (`protocol/schema.md` §"Not
frozen: the updater's local socket").

| Variable | Default | Notes |
|---|---|---|
| `QUASAR_UPDATER_ALLOWED_NAMESPACES` | `ghcr.io/accreleus/quasar` | Comma-separated registry namespaces this host will pull platform images from. Matched on `host/path/` segment boundaries, so `ghcr.io/accreleus/quasar` does not admit `ghcr.io/accreleus/quasar-evil/x`. Anything outside is refused `namespace_rejected` before a byte is pulled. **Unset or blank is the org default, never "allow nothing" and never "allow everything"** — the compose default is `${QUASAR_UPDATER_ALLOWED_NAMESPACES:-}`, so the program is handed an empty string on every stack that does not set it (guarded by `TestUnsetNamespaceKnobIsTheOrgDefault`). To lock a host down, name a namespace nothing matches. |
| `QUASAR_UPDATER_WAIT_TIMEOUT_S` | `300` | `docker compose up --wait --wait-timeout`. A request may name its own value. |
| `QUASAR_UPDATER_PULL_TIMEOUT_S` | `3600` | Wall-clock bound on the `pull` step. |
| `QUASAR_UPDATER_RECREATE_TIMEOUT_S` | `900` | Wall-clock bound on the `up` step, independent of compose's health wait. |
| `QUASAR_UPDATER_SOCKET` | `/run/quasar-updater/updater.sock` | Where the socket is created, mode 0666, in the volume shared with the control plane and the agent. Read by all three: an absent socket makes the control-plane target of an update `updater_absent` rather than an apply that fails halfway. |
| `QUASAR_UPDATER_RESULTS_DIR` | `/run/quasar-updater/results` | One result file per request id, written tmp+rename. Directory is root-owned 0755: the other containers read, only the updater writes. |
| `QUASAR_UPDATER_DOCKER_BIN` | `docker` | The CLI the updater drives. |

The node agent reads the first two of those paths too, under the same names, to
find the socket it POSTs to and the result files it relays
(`agent-api.md` `release_state`).

---

## Benchmark harness (`make bench-*`) — workstation environment only

Read by `scripts/dx/bench_*.{sh,py}` on the machine driving the benchmark. They
are **not** read by the control plane, the node agent, or any container, and they
belong in the operator's shell, never in `deploy/.env` and never in the repo.

| Variable | Default | Notes |
|---|---|---|
| `BENCH_URL` | `http://localhost:9400` | Base URL of the quasar-bench results service (dashboard on the same URL). |
| `BENCH_KEY` | — (**required**) | A `BENCH_API_KEYS` secret from the quasar-bench deployment's own `deploy/.env`. **Never commit it**; `make verify` fails if a literal key value appears under `scripts/dx/` or in the `Makefile`. Read it from the service host, e.g. `sed -n 's/^BENCH_API_KEYS=harness://p' ~/quasar-bench/deploy/.env`. |

`bench-run` and `bench-suite` additionally need `QSES_ADMIN_TOKEN` (and usually
`QSES_DEV_KEY`) exactly as `.claude/skills/quasar-session/SKILL.md` documents —
they launch real sessions and read/PATCH admin endpoints. See the benchmark table
in `AGENTS.md` for the verbs themselves.

---

## Reading a live session (`make session-*`) — workstation environment only

Read by `scripts/dx/session.sh` and `scripts/dx/admin_token.sh` on the machine
doing the diagnosing. Not read by the control plane, the node agent, or any
container; they belong in the operator's shell, never in `deploy/.env`.

```
make session-list     HOST=gpu-test                       # what is running
make session-verdict  SID=latest HOST=gpu-test            # the classifier's answer
make session-diagnose SID=latest HOST=gpu-test            # the full analysis
make session-metrics  SID=latest HOST=gpu-test SINCE=5m   # the sample table
make session-trace    SID=latest HOST=gpu-test            # window + events
make session-bundle   SID=latest HOST=gpu-test            # raw bundle JSON to a file
make session-logs     SID=<id>   HOST=gpu-test SINCE=10m  # the session's containers
```

`SID=latest` is the newest `state=running` session (resolved client-side —
`GET /v1/admin/sessions` has no state filter). `WINDOW=<from_ms>,<to_ms>` scopes
verdict/trace/bundle/diagnose; `JSON=1` makes any verb machine-readable;
`N=<rows>` and `SINCE=` bound `session-metrics`; `GREP=<pattern>` filters
`session-logs`; `OUT=<path>` places the bundle (default
`.diagnostics/bundle-<sid>-<ts>.json`, mode 0600).

### On-demand capture (`make session-capture`)

```
make session-capture SID=latest HOST=gpu-test KIND=pipeline_dot
make session-capture SID=latest HOST=gpu-test KIND=all OUT=.diagnostics/
```

A **capture** is a bounded, admin-triggered observation of a live session: arm it,
the agent observes within a byte *and* time budget, and reports once. It exists
so that three questions stop requiring an ssh hop or a rebuild — what the encode
graph is actually wired as right now, what the encoder's live properties are, and
what encode times look like at a finer grain than the heartbeat.

| KIND | what it captures | on disk |
|---|---|---|
| `pipeline_dot` | the encode pipeline's graph (`CAPS_DETAILS \| STATES`) | `.dot`, plus `.svg` when graphviz is installed |
| `encoder_props` | the live encoder snapshot: factory, codec, allow-listed properties, negotiated caps | `.json` |
| `burst_stats` | a short dense window series — encode/dwell percentiles, fps, bitrate, drops, setpoint | `.json` |
| `all` | the three above, **sequentially** (captures are single-flight per session, so a parallel fan-out would refuse two of its own three requests) | three files |

**What can and cannot be captured.** Never pixels, audio, or bitstream; never
input events or the microphone; never `node_secret`, a token, or the environment
wholesale; never a path outside the session's scratch directory. `encoder_props`
reads an *allow-list* of property names and elides long string values, and
`pipeline_dot` deliberately omits `NON_DEFAULT_PARAMS` because element parameters
can print paths. A capture is **not a probe** — nothing is inserted into the media
path.

**Bounds.** 256 KiB compressed and 10 s wall clock per capture; over the byte
budget the agent truncates at a line boundary and reports `truncated=true` with
`original_bytes`. `WINDOWS` (1–40) × `WINDOW_MS` (100–1000) plan `burst_stats`,
clamped by the control plane and again by the agent, and clamped once more so the
plan fits inside the time budget.

**Retention.** Captures are **exempt from the trace's one-hour rolling prune and
from the terminal prune** — they leave with the session row's `ON DELETE CASCADE`
and nothing else. "Why did that session behave that way" is asked in the past
tense, so a capture taken before a session was stopped must still be readable
after.

| Variable | Default | Notes |
|---|---|---|
| `KIND` | *(required)* | `pipeline_dot` \| `encoder_props` \| `burst_stats` \| `all`. |
| `OUT` | `.diagnostics/` | Directory (not a file — a capture may write several). Files are `capture-<sid>-<kind>-<ts>.{dot,json}`, mode 0600. |
| `WINDOWS` / `WINDOW_MS` | `20` / `250` | `burst_stats` only. |
| `CAPTURE_TIMEOUT_S` | `15` | How long to poll for the result: the agent's 10 s budget plus slack. A `404` while polling is the poll **signal**, not an error. |

Exit 2 always names the next command. The one to know: **`501` means this host's
node-agent predates captures** — it never acked, so retrying will never help;
`make rebuild HOST=<h>` will. A `409` means a capture is already in flight
(single-flight, never queued): wait. The captures also ride inside
`make session-bundle` as `captures[]`, so an archived bundle carries them.

### The admin-token ladder — one place, `scripts/dx/admin_token.sh`

`make admin-token HOST=<h>` prints a bearer on stdout (diagnostics go to stderr,
so `TOK="$(make -s admin-token HOST=gpu-test)"` is safe). Order:

| tier | source | notes |
|---|---|---|
| 1 | `$QUASAR_ADMIN_TOKEN` | always wins, **never cached** — it is often the identity that also owns the session, which owner-gated verbs need. `$QSES_ADMIN_TOKEN` feeds this tier for the older callers. |
| 2 | `${XDG_CACHE_HOME:-~/.cache}/quasar/<host>.token` | mode 0600, two lines: expiry epoch, then the token. Valid while more than 60 s remain. `--fresh` bypasses and overwrites it. |
| 3a | the host's per-boot dev key → `POST /v1/dev/agent-session` | one ssh hop; reads `/run/quasar/dev-agent-key` out of the `control-plane` container. Needs `QUASAR_DEV_AGENT_AUTH=1` on that stack. Survives a rotated bootstrap password. |
| 3b | `BOOTSTRAP_ADMIN_*` from that host's `<dir>/deploy/.env` → `POST /v1/auth/login` | last resort; 401s once the password is rotated. |

Tier 3 uses the host's **own** `api` URL from `.claude/skills/_shared/hosts.json`,
so no credential crosses the network. With `HOST=local` there is no ssh at all:
the key comes from this worktree's compose stack and the API is
`http://127.0.0.1:$DX_CP_PORT`. An empty result is **exit 2** listing every tier
tried and naming the next command — never a bare `Authorization: Bearer `, which
comes back 401 and reads like a real rejection.

| Variable | Default | Notes |
|---|---|---|
| `QUASAR_ADMIN_TOKEN` | unset | Tier 1 of the ladder. Mint a long-TTL one with `make agent-creds ARGS='--role admin --ttl 2h'` when a verb must act as the session's owner. |
| `QSES_ADMIN_TOKEN` | unset | The historical name; still honoured by `qses`, `session_soak.sh`, `bench_run.sh`, `bench_suite.sh`, and fed into tier 1. |
| `XDG_CACHE_HOME` | `~/.cache` | Parent of the token cache (`quasar/<host>.token`). Delete the file, or pass `--fresh`, to force a re-mint. |
| `QUASAR_CURL_INSECURE` | `0` | `1` adds `curl -k`. The fleet certs are trusted on the workstation, so the default is a **verified** TLS connection. A host may instead carry `"tls_insecure": true` in `hosts.json`. |
| `QDIAG_CONFIG` | `.claude/skills/quasar-diagnose/config.json` | Analysis facts (classifier thresholds, experiment matrices, netem shapes) for `scripts/dx/session_diagnose/`. |

The workstation reaches a remote stack on its `api_external`; the host itself
uses `api`. Both live in `.claude/skills/_shared/hosts.json`, which is the only
host registry — no script keeps a second copy (the copy `quasar-diagnose`'s
`config.json` used to keep never learned about devbox).

### What `session-logs` reads

Container names are the agent's own (`node-agent/src/session/container.rs`,
`audio.rs`):

| container | what |
|---|---|
| `quasar-sess-<sid>-g<N>` | the app container for that session; `N` is the generation (a swap runs old and new side by side), so match by prefix |
| `quasar-pulse-<sid>` | its PulseAudio sidecar (absent when the session has no audio) |
| the `node-agent` container | resolved by `docker ps --filter name=node-agent`, filtered to the sid and stripped of EGL/GL/smithay renderer chatter |

**Where a host has a Dozzle MCP endpoint recorded in `.claude/skills/_shared/hosts.json`,
prefer it** — the same logs, already scoped, with no ssh round trip.

### The exit / RESULT contract

Every verb ends with exactly one line on stdout:

```
RESULT status=<ok|degraded|failed|error> target=session-<verb> sid=<sid> host=<host> [verdict=…] [reason=…]
```

With `JSON=1` the same fields also appear inside the JSON object.

| exit | meaning |
|---|---|
| 0 | ok — **including a classifier verdict the tooling does not recognise.** The control plane owns that vocabulary and grows it; an unknown string is printed verbatim with a note and `reason=unrecognised-verdict`. |
| 1 | degraded — a `likely_network_congestion` / `likely_encoder_saturation` / `likely_client_presentation_limit` verdict |
| 2 | tool error (auth, unreachable, 404). The message always names the next command: a 401 names `scripts/dx/admin_token.sh --host <h> --fresh`, a 404 names `make session-list HOST=<h>` (a stopped session keeps its row, so a 404 usually means the wrong stack). |
| 3 | usage |

Nothing prompts. `make session-verdict` is safe in a script or a cron line.

---

## Per-host runtime overrides (admin UI)

Most node-agent runtime knobs above are also settable **per host** from the admin
UI (`/admin/hosts` → **Settings**), so an operator can adjust them without editing
a host's `.env` and restarting the container by hand. The value is stored in the
control plane (`host_settings`, migration 0010) and pushed to the agent over the
agent WebSocket as a `config_update` message.

**Resolution precedence: `admin override → agent env (`QUASAR_*`) → catalog default`.**
The `config_update` carries only the host's **explicit admin overrides**; the agent
overlays them on its own env-derived baseline. So a knob you never override keeps the
**agent's env value** (e.g. a GPU host with `QUASAR_ENCODER=nvenc` and no override runs
NVENC — it is *not* forced to the `openh264` catalog default), and **clearing** an override
reverts the knob to the agent env, not the catalog default. A deployment that never touches
a knob behaves exactly as its `QUASAR_*` env dictates. (Before #194 the control plane pushed
the full resolved map, so the catalog default silently overrode the agent env — a GPU host
could do software encode with no visible error.) Note: the admin Settings page shows a
`resolved` value computed as `catalog-default ← overrides`; it does not see the agent's env,
so on a host whose env differs from the catalog default the displayed value can differ from
what the agent actually runs (effective-config surfacing is a follow-up).

Two classes:

| Class | Behaviour | Knobs (env var) |
|---|---|---|
| **Live** | Applies on the **next session**; no restart. | `QUASAR_ABR`, `QUASAR_ABR_FLOOR_KBPS`, `QUASAR_ABR_FLOOR_RATIO`, `QUASAR_ABR_EWMA_ALPHA`, `QUASAR_ABR_DEADBAND`, `QUASAR_ABR_MAX_UP_STEP`, `QUASAR_ABR_MIN_INTERVAL_MS`, `QUASAR_ABR_MAX_DOWN_STEP`, `QUASAR_ABR_DOWN_DWELL_MS`, `QUASAR_ABR_CLIFF_GUARD_FRAC`, `QUASAR_ABR_LADDER_MAX_BIAS`, `QUASAR_ABR_LADDER_ENGAGE_DWELL`, `QUASAR_ABR_LADDER_RECOVER_DWELL`, `QUASAR_ADAPT_ENCODE_BUDGET_FRAC`, `QUASAR_ADAPT_FPS_STEADY_FRAC`, `QUASAR_ADAPT_GCC_BELOW_FRAC`, `QUASAR_ADAPT_SEND_AT_CAP_FRAC`, `QUASAR_GOP`, `QUASAR_SLICES`, `QUASAR_TARGET_USAGE`, `QUASAR_QUEUE_BUFFERS`, `QUASAR_ZEROCOPY`, `QUASAR_LATENCY_PROBE`, `QUASAR_IDLE_TIMEOUT_SECS`, `QUASAR_APP_BOOT_TIMEOUT_SECS`, `QUASAR_INPUT_BATCH_MS`, `QUASAR_INPUT_CHANNEL_MODE`, `QUASAR_INPUT_TRACE`, `QUASAR_INPUT_CONTROLLER_NUDGE`, `QUASAR_AUDIO_DISABLED`, `QUASAR_AUDIO_NO_CLOCK`, `QUASAR_AUDIO_REQUIRED`, `QUASAR_MIC_DISABLED`, `QUASAR_HOME_ROOT` (storage-config: the per-host `home_root` knob — the effective managed-home root the control plane uses to synthesize `local` mounts; empty or absolute) |
| **Restart** | Read at first session build (`gst::init` is a process-wide `Once`), so a change triggers a **guarded agent restart** — confirmed when the host has live sessions, dropping them. | `QUASAR_ENCODER`, `QUASAR_RENDER_NODE`, `QUASAR_CUDA_DEVICE` |

The authoritative knob catalog (keys, types, defaults, classes) lives in
`control-plane/internal/hostcfg/catalog.go`. Endpoints: `GET /v1/admin/config/catalog`,
`GET`/`PATCH /v1/admin/hosts/{id}/settings` (`protocol/control-api.md`). Wire messages:
`config_update` / `restart` (`protocol/agent-api.md`).

### Console auto-start (`default_app` / `default_user`) — needs an entitlement

Per-host console-mode settings live in `console_config` (`GET`/`PATCH
/v1/admin/hosts/{id}/console-config`, admin UI → host → Console). When
`auto_start_on_display` is on and both `default_app` and `default_user` are set,
a display connecting to that host auto-launches that app as that user.

**The console auto-start goes through the same entitlement gate as any other
launch** (steam-library-discovery Phase 2, migration 0043): the launch runs
`ScheduleAndCreate`, which refuses an app the launching user holds no
entitlement for. So if `default_user` is not entitled to `default_app` — because
an admin revoked it, or because the app was created with `entitle: "none"`, or
because the app was seeded by a raw SQL script that wrote no entitlement row —
auto-start fails with `not entitled to this app` and, after
`consoleBackoffMaxRetries` fast failures, stops retrying until the next capacity
report. This is correct behaviour (the console is a real session for a real
user), but it is not self-evident from the console settings page: grant the
entitlement via `POST /v1/admin/apps/{id}/entitlements`, or leave the app's
default `all` grant in place.

---

<a name="note-launch-resolution"></a>
## Note: launch resolution & tiers

For a control-plane-assigned session the **launch resolution/fps/bitrate** come
from the app's catalog defaults run through the AS-01 tier ladder and the client
connection probe — not from the node-agent's `QUASAR_WIDTH/HEIGHT/FPS` (those are
the standalone-path fallback). The current capping behaviour (a weak probe can
down-tier the launch resolution below the app's configured value) and the proposed
"app config authoritative + admin max-res/fps ceiling + per-app override" model are
tracked in issue #181. Until that lands, setting an app to e.g. 1440p does not
guarantee a 1440p launch.

---

## Session display: internal (render) resolution, external (stream) resolution, interface size

Not env vars: live, per-session, user-changeable knobs, distinct from the
launch resolution above. Two sizes matter here and they are independent axes
(amended 2026-08-16 — see below):

- **Internal resolution** ("render resolution" in the UI) is what the app
  inside the container actually renders/outputs: the app-facing `wl_output`
  logical mode.
- **External resolution** ("stream resolution") is what is encoded and
  streamed to the browser. Until this feature it was pinned for the whole
  session at the launch size. It is now **live-changeable, downward from the
  launch size, on a rung ladder**, independently of internal resolution.

> **Amendment 2026-08-16 (Michael): independent axes, not a clamp.** Internal
> (render) and external (stream) resolution are **independent**: each is
> bounded only by the session's pinned LAUNCH size (16 ≤ dim ≤ launch, even;
> external additionally must be a rung), never by the other. External may sit
> BELOW the current internal size — the encode-side scaler downsamples the
> compositor framebuffer — which is the intended use case: on a degraded
> connection, external steps down (e.g. to 1080p) while the app keeps
> rendering at its internal size (e.g. 4K) and never sees a mode change;
> stepping external back up is a passthrough. This replaces the earlier
> "internal ≤ external" clamp rule (stepping external down no longer clamps
> internal down with it).

**Interface size** is a `wp_fractional_scale_v1` preferred_scale hint sent to
toplevels, unrelated to either resolution. Live QA showed nested KWin (the KDE
image) keeps its own desktop scale pinned at 1 regardless of the parent's
`wp_fractional_scale_v1` / `wl_output` scale hint, so the KDE image does
**not** visibly follow `ui_scale` today; scale-aware native Wayland clients do.

### External (stream) resolution: rung ladder

The external size steps through a **rung ladder**, keyed by the launch
profile's aspect family, filtered to values ≤ the launch (launch = the
session's stream size at connect time, unchanged):

- `16:9`: 3840x2160, 2560x1440, 1920x1080, 1600x900, 1280x720
- `16:10`, `21:9`, `4:3`: equivalent per-family rungs (same construction,
  scaled to each family's aspect)

A session's available rungs are the family's list filtered `≤` the launch
WxH. Custom rungs and a client-aspect-driven family pick are architected for
later (the ladder is code-owned data today, `control-plane/internal/profile/rungs.go`,
migrating to a table is additive).

**Mechanism.** External resize is an **encode-side** change: a mutable scale
stage sits in the ENCODE pipeline, between `interpipesrc` and the encoder.
The compositor, interpipe boundary, and swap machinery are completely
untouched: they keep emitting session-size frames exactly as before. Only
the scale stage's output capsfilter moves (`set_property("caps", WxH)`); the
encoder re-configures on the new input caps via its live
`GstVideoEncoder::set_format` path. An IDR is forced on every step, and
`h264parse config-interval=-1` guarantees SPS/PPS land on that IDR. This
happens with **no WebRTC renegotiation** (no second offer, no ICE restart);
libwebrtc receivers handle a mid-stream coded-size change on an already
negotiated m-line without it.

**Per-encoder support.** External resize needs a live scale stage per
encoder path: VA (`vapostproc`), NVENC (`cudaconvertscale`/`cudascale`), and
openh264 (`videoscale`) all support it. Vulkan (#501): supported for **h264,
h265 and av1** when the image's `gst-wayland-display` fork carries `vulkanscale`
(a `GstBaseTransform` that scales `memory:VulkanImage` NV12/P010 frames,
passthrough at the launch size); the scale stage becomes `vulkanscale !
capsfilter`, the same shape as every other hardware arm, keyed on the
effective encoder (an AV1 session that fell back to the vendor HW path — because
`QUASAR_VULKAN_AV1=0` or the image has no `vulkanav1enc` — keeps that vendor's
arm, not this one).
**h265 was gated off for the first half of 2026-08-22 and is now open.** Live
validation that morning found a mid-stream size grow on
`vulkanh264enc`/`vulkanh265enc` is a GPU-level fault (Xid 31) without a
second vendored patch; with that patch h264 and av1 were proven live, while
`vulkanh265enc` appeared unable to start at all on the devbox (driver
610.57.04, `Video profile format not supported (-1000023003)`). That last
part was a **probe artifact, not a driver or encoder limit**: the element's
src pad template advertises a `main-444` profile its own `H265ProfileMap[]`
cannot map, so a pipeline that leaves `profile` unconstrained can negotiate
an invalid profile IDC. The production encode path always pinned
`video/x-h265,profile=main`, so it was never exposed. With the profile
pinned, the h265 grow/shrink is proven live at full rate, and
`scale_stage::vulkan_resize_validated_for_codec` now passes every codec (see
`docs/reports/2026-08-22-vulkanscale-validation/H265-PROFILE-CAPS.md`). On an
image whose fork pin predates `vulkanscale`, the lever is still refused with
409 `external_resize_unsupported` and metrics report
`external_resize_supported=false`, exactly as before:
`vulkanscale`'s presence is probed at build time (mirroring the `videorate`
presence check for the fps rung), so an older image behaves identically to
today, not a broken new one. **The Vulkan lever must only be enabled on an
image that carries both the fork pin adding `vulkanscale` and the vendored
`vulkan-enc-output-state-on-resize.patch`**: the two land together in the
same image build, because `vulkanscale` present with the patch missing is
exactly the GPU-fault configuration the live validation found. `session_metrics`
always reports the computed `external_resize_supported: bool` for the
session's actual encoder/host, so a client never has to guess from the
encoder name alone.

**Not wired into ABR.** The stream-resolution lever is manual/API only in
this feature. The ABR governor is given a `ResolutionLever` interface
(`available_rungs()` / `set_rung()` / `current()`) so it *could* call the
same mechanism later, but nothing drives it automatically yet; independent
validation of the lever comes first. See `node-agent/src/session/ladder.rs`
for the smoothness-ladder rung this replaces the old deferral note on.

### Flow (both internal and external)

Web session drawer issues `PATCH /v1/sessions/{id}/display` (owner or admin;
body `render_width`/`render_height` both-or-neither for internal, plus
`stream_width`/`stream_height` both-or-neither for external, plus `ui_scale`;
202 on accept). The control plane relays a `session_display_update` message
to the host agent, best-effort (a relay failure never fails the session),
and returns 409 `display_update_rejected` if the agent nacks or does not
ack (or 409 `external_resize_unsupported` for a stream-size change the
encoder can't perform live). The agent sets the `waylanddisplaysrc` element's
`render-size` (`"WxH"`, `"0x0"` = follow the encode size) and `ui-scale`
properties live for internal resolution, and the encode-pipeline scale
stage's capsfilter live for external resolution (forcing an IDR).
Constraints: `16 ≤ dim ≤ stream dim`, even; `1.0 ≤ ui_scale ≤ 3.0`; a
`stream_width`/`stream_height` pair must be a rung of the session's aspect
family, ≤ launch size.

**Ephemeral, not persisted.** Readback is `session_metrics`
(`render_width`/`render_height`/`ui_scale`/`stream_width`/`stream_height`,
present only when non-default) and, on the control-plane side, an
in-memory cache of the last-acked external size surfaced as
`stream.external_width`/`stream.external_height` on `GET
/v1/sessions/{id}` (`stream.width`/`stream.height` there stays the launch
size, unchanged) plus `stream.rungs` (the session's available external
rungs). The control-plane cache is **not** persisted to Postgres, so a
control-plane restart shows the launch size again until the next metrics
sample arrives. A swap re-applies the last-requested internal size to the
new generation before it starts; the external size needs no re-apply
because the encode pipeline itself survives a swap unchanged.

**Older compositor images** (a `gst-wayland-display` build predating the pin
bump that added these properties) leave the agent detecting the missing
properties and the session unchanged, so `session_metrics` never echoes a
size that isn't actually on screen.

**Interface size row is hidden by default in the web UI.** The API, agent, and
compositor plumbing for `ui_scale` is fully live, but the drawer's "Interface
size" row is gated behind `SHOW_INTERFACE_SIZE_CONTROL` in
`web/src/pages/app/SessionDrawer.tsx`, default off, because of the KDE/KWin gap
above. Re-enabling it waits on a KWin-side fix in the KDE image, tracked as a
follow-up in the `quasar-images` repo.

**HUD badge.** The stream view shows the current external size and animates
on change (e.g. "Stream 1280x720"), with an "Adapting…" state while a
resize request is in flight. It shares the drawer's single-in-flight display
PATCH machinery with the internal-resolution and interface-size controls.

### Internal (render) resolution: fullscreen fit + mode ladder

The compositor now scales a fullscreen buffer smaller than its configured
logical size to **fit** the output (aspect-preserved, centred/letterboxed)
instead of drawing it 1:1 top-left. That means a native Wayland game's own
in-game resolution setting works: pick a smaller mode in the game, the
compositor fills the rest of the frame instead of leaving black borders and
an undersized image.

The compositor also advertises a **mode ladder** on the `wl_output` (one
mode per rung ≤ the session's current external size, in the profile's
aspect family), so in-app resolution menus and desktop display settings can
list real choices instead of one fixed mode.

**KDE Display Settings** lists the ladder and applies a chosen mode only
with the **patched KWin shipped in the KDE image** (`quasar-images`); a
stock KWin nested backend does not read the parent's mode ladder. Desktops
built on other images fall back to setting internal resolution from the
drawer control only.

**Steam / gamescope is out of scope.** Gamescope sizes its nested output
from `-W/-H` at launch and cannot resize without a relaunch; games *inside*
gamescope keep their own in-game resolution setting (gamescope scales
them), which already works today. A launch profile pinned through nested
gamescope keeps its launch resolution regardless of the internal-resolution
control; changing it for a Steam session means relaunching.

### Harness

`qses display` / `qses matrix` (or `make session-display`) step a live
session through the external rung ladder and assert the one-offer
invariant, browser `videoWidth` following the step, and the encoder's
`encode_ms` improving at a lower rung; see the validation matrix in
`docs/superpowers/specs/2026-08-16-adaptive-external-resolution-design.md`.

`qses soak` (or `make session-soak SID=latest ARGS='--duration 180'`) is
the longer-form sibling: an on-demand **bad-connection simulation** against
a session someone is already playing. It walks the external size down the
ladder and back up over a fixed duration, samples agent and browser
telemetry throughout, and writes a report (step table, per-transition
boundary analysis, ASCII timeline, optimisation candidates) under
`.diagnostics/soak/`. It never sends `render_*`, restores the launch size
on exit including Ctrl-C, and is **not** wired into ABR — it is a manual
degradation driver for post-hoc optimisation review. A host whose encoder
cannot live-resize (`stream.external_resize_supported=false`, a Vulkan host
without `vulkanscale` in its image, #501) is refused with exit 3 and the
`QUASAR_ENCODER=nvenc`/`va` remedy.

See `protocol/agent-api.md` §session_display_update and
`protocol/control-api.md` §PATCH /v1/sessions/{id}/display.
