# Deploying Quasar

Getting Quasar running on a Linux GPU box, and the reference material for the
things you may have to configure afterwards.

- **[Part 1 — Quick start](#part-1-quick-start)**: from nothing to streaming.
- **[Part 2 — Which compose files do I use?](#part-2-which-compose-files-do-i-use)**
- **[Part 3 — Reference](#part-3-reference)**: TLS, reverse proxies, networking,
  GPU encode, upgrades. All optional reading.

Contributor tooling (the dev container, the harnesses, image builds) is not here
— it is in [`docs/developer-tooling.md`](../docs/developer-tooling.md).

---

# Part 1: Quick start

## Before you start

- A **Linux host** with a GPU. Not macOS, not Windows, not WSL: the node agent
  needs `network_mode: host`, which only Linux provides. Any OS can be the
  *client* — this is about the machine running the games.
- **Docker Engine + Compose v2.20 or newer** (`docker compose version`).
- **Disk for Docker**: ~20 GB to install a release, 40 GB+ to build from source.
- **A GPU**: AMD/Intel (VA-API) works as-is. NVIDIA also needs the driver plus
  `nvidia-container-toolkit` with CDI configured (`sudo nvidia-ctk runtime configure`).
- **The browser and the GPU host on the same LAN**, or on a VPN that joins them.
  The video does not travel through the control plane — see
  [Media reachability](#media-reachability-lan-or-vpn).
- **`git`** installed (it is not there by default on Fedora).

Full detail on every one of these: [Prerequisites in detail](#prerequisites-in-detail).

Two ways in. Pick one:

- **[A — Install a release](#a-install-a-release-recommended)**: pull the
  published images. Nothing compiles. This is what you want.
- **[B — Build from source](#b-build-from-source)**: build everything from a
  git branch. For contributors, and for hosts that track `develop`.

## A. Install a release (recommended)

### 1. Get the tagged tree

You need the repo only for its compose files. A shallow clone of the tag is
enough, and no submodule has to be initialized.

```bash
git clone --depth 1 --branch vX.Y.Z https://github.com/accreleus/quasar.git
cd quasar
```

Replace `vX.Y.Z` with the newest tag from the
[releases page](https://github.com/accreleus/quasar/releases).

> You should see `Cloning into 'quasar'...` and end up in a directory
> containing `deploy/`.

### 2. Write `deploy/.env`

Three values matter. `POSTGRES_PASSWORD` and `ENROLLMENT_TOKEN` are required —
Compose refuses to start without them. `QUASAR_SECRET_KEY` is optional but set
it now: without it, credentials saved through the admin UI cannot be stored.

The two image lines pin the stack to the release. Take the digests from the
release body.

```bash
umask 077
cp deploy/.env.example deploy/.env
cat >> deploy/.env <<EOF

POSTGRES_PASSWORD=$(openssl rand -hex 24)
ENROLLMENT_TOKEN=$(openssl rand -hex 32)
QUASAR_SECRET_KEY=$(openssl rand -base64 32)
QUASAR_HOME_ROOT=/var/lib/quasar/homes

QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@sha256:...
QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@sha256:...
EOF
chmod 600 deploy/.env
```

> No output. `grep -c = deploy/.env` should now be non-zero, and
> `ls -l deploy/.env` should show `-rw-------`.

`QUASAR_HOME_ROOT` can be any persistent path; `/var/lib/quasar/homes` suits a
generic Linux host, but on unraid-style appliance OSes where `/var/lib` is
RAM-backed and lost on reboot, use the persistent data share instead (e.g.
`/mnt/user/appdata/quasar/homes`, or `/mnt/cache/appdata/quasar/homes` to skip
the FUSE overlay) — here and in the `install -d` commands in step 3.

**Back up `deploy/.env` somewhere safe.** `QUASAR_SECRET_KEY` seals every
credential stored through the admin UI; losing it makes them unrecoverable.

### 3. Name this host in the certificate, and create the home directory root

Quasar serves its own self-signed certificate, generated on first boot. It can
only see its own container addresses, so it has to be told the name or address
you will actually type — otherwise the browser rejects it on every visit, and
accepting the warning never clears that (it is a name mismatch, not a trust
failure).

**Reaching it by IP?** Nothing to decide — this fills in your LAN address:

```bash
bash deploy/seed-tls-hosts.sh deploy/.env
sudo install -d -m 0755 /var/lib/quasar/homes
```

> You should see the line it wrote, e.g.
> `QUASAR_TLS_HOSTS=192.168.1.50,myhost` added to `deploy/.env`.

**Have a DNS name for this host?** Put it in `deploy/.env` *before* you bring
the stack up, and the certificate carries it:

```bash
echo 'QUASAR_TLS_HOSTS=play.example.com' >> deploy/.env
sudo install -d -m 0755 /var/lib/quasar/homes
```

Either way you open the stack at that name or address in step 5. (Both work
together: `QUASAR_TLS_HOSTS=play.example.com,192.168.1.50`. `seed-tls-hosts.sh`
never overwrites a value you set yourself, so setting it by hand first is
always safe.)

Adding a name *after* the stack is already running needs the certificate
re-issued — see [Adding a name later](#adding-a-name-later).

### 4. Pull and start

The two image lines you set in step 2 are what make this a pinned release
install — there is no extra overlay to add. AMD/Intel hosts use the base file
alone; NVIDIA hosts add the one hardware overlay.

```bash
# AMD / Intel host
docker compose -f deploy/docker-compose.yml pull
docker compose -f deploy/docker-compose.yml up -d

# NVIDIA host
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml pull
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml up -d
```

> The pull is the long part — the node-agent image is large. `up -d` then
> prints `Container deploy-quasar-postgres-1  Started` and the same for
> `quasar-control-plane` and `quasar-node-agent`.

### 5. Check it came up

```bash
docker compose -f deploy/docker-compose.yml ps
curl http://localhost:8080/health
```

> `ps` should show all three services `healthy`. `curl` should print
> `{"status":"ok","db":"ok"}`. (That one URL stays plaintext deliberately; every
> other HTTP path redirects to HTTPS.)

If you changed `CONTROL_PORT` or `QUASAR_TLS_PORT` in `deploy/.env`, substitute
your values for `:8080` and `:8443` in this and every later command.

Now go to [Claim your admin account](#claim-your-admin-account).

> **Installing `v0.1.0` specifically?** That tag predates three fixes to this
> path and needs two extra steps. See
> [Installing v0.1.0](#installing-v010-extra-steps) at the end of Part 3.

## B. Build from source

Use this when you are changing Quasar, or running a host that tracks `develop`.
It builds the web app and both runtime images locally: roughly **25 minutes and
25 GB of Docker disk** the first time, on an 8 vCPU box.

`deploy/redeploy.sh` does the whole thing — syncs the tree to a git ref,
generates the secrets into `deploy/.env`, seeds the TLS host names, builds the
web app in a `node:22` container (no Node needed on your host), builds the
images, starts the stack, and then verifies that what is serving is what it just
built.

### 1. Clone the repo

```bash
git clone https://github.com/accreleus/quasar.git
cd quasar
```

> You should see `Cloning into 'quasar'...` and end up in a directory
> containing `deploy/`.

### 2. Run the deploy

One command. Pick the line for your GPU vendor.

```bash
bash deploy/redeploy.sh nvidia develop     # NVIDIA host
bash deploy/redeploy.sh va develop         # AMD / Intel host
```

> It prints seven numbered steps (`[nvidia] 1/7 sync repo to develop` …
> `7/7 verify running stack`), and along the way tells you it generated
> secrets (`generated a new QUASAR_SECRET_KEY into deploy/.env (mode 600)`) and
> the TLS host names. The last line is the verdict:
>
> ```
> REDEPLOY env=nvidia scope=all ref=develop sha=<short> bundle=index-<hash>.js health=ok catalog=401 agent=registered result=OK
> ```
>
> `result=OK` means every post-deploy check passed. A non-zero exit or
> `result=FAIL` means it did not — read the failing check above that line.

**Back up `deploy/.env`** — `redeploy.sh` generated `QUASAR_SECRET_KEY` into it,
and losing that key makes stored credentials unrecoverable.

`develop` is the unstable integration branch: it installs whatever merged that
hour. That is the point on a development host, and the reason a real install
should name a release tag instead. A tag works here too
(`bash deploy/redeploy.sh nvidia vX.Y.Z`) and builds that fixed tree from
source.

### 3. Check it came up

```bash
docker compose -f deploy/docker-compose.yml ps
curl http://localhost:8080/health
```

> All three services `healthy`, and `{"status":"ok","db":"ok"}`.

## Claim your admin account

A fresh instance has no accounts at all. Claiming the first one is three steps.

### 1. Open the UI

Go to **`https://<host-ip>:8443`** in a browser.

> The browser shows a certificate warning. That is **expected** — the
> certificate is self-signed. On a network you control, click through it. On one
> you do not fully trust, verify the fingerprint first (see
> [How TLS works by default](#how-tls-works-by-default)). You then land on the
> first-run setup wizard at `/setup`.

`http://<host-ip>:8080` also works but redirects, so use the `https://` URL.

> **Clicking through the warning is enough to get streaming, but not enough for
> everything.** Browsers treat a certificate you bypassed as second-class:
> **microphone passthrough and full in-game keyboard capture need the browser to
> actually *trust* the certificate, not merely to have been told to proceed.**
> Installing it takes a minute per device and is worth doing before you hand the
> URL to anyone — see
> [Trusting the certificate](#trusting-the-certificate-microphone-and-keyboard-capture).

### 2. Get the one-time setup token

The control plane minted a token at boot and wrote it to a file inside the
container — never to the log, so it cannot leak through log forwarding.

```bash
docker compose -f deploy/docker-compose.yml exec quasar-control-plane cat /run/quasar/setup-token
```

> A single 64-character hex string.

### 3. Finish the wizard

Paste the token into the setup wizard along with the email, username and
password you want for the admin account.

> You are signed in as the admin, and the wizard is gone. The token is now
> spent: the claim endpoint returns 409 forever after.

The token is **per boot**. If you restart the control plane before claiming, the
old token is invalidated and a new one is minted — read the file again.

**Scripting the install instead?** Set `BOOTSTRAP_ADMIN_EMAIL`,
`BOOTSTRAP_ADMIN_USERNAME` and `BOOTSTRAP_ADMIN_PASSWORD` (all three) in
`deploy/.env` and the admin is provisioned at boot with no wizard. It is
idempotent — a no-op once any admin exists — so it is safe to leave set.

## Add some apps

Sign in and go to **`/admin`**:

1. **Images** — sync the image catalog and install the app images you want.
2. **Apps** — add the matching apps to the catalog.

Users then launch them from `/app`.

> If you create an app **by hand** rather than from an image, copy the `gpu`,
> `no_new_privileges` and `systempaths_unconfined` values from the image onto
> the app. Without them a desktop-session image fails at launch with
> `software Vulkan renderer detected`.

Nothing streaming? The two usual causes are the host firewall and the network
path — [Host firewall blocking WebRTC media](#host-firewall-blocking-webrtc-media)
and [When the video never arrives](#when-the-video-never-arrives).

---

# Part 2: Which compose files do I use?

Short version, and it is the whole rule:

1. **Always** `deploy/docker-compose.yml`.
2. **On an NVIDIA host, add `docker-compose.nvidia.yml`.** On AMD/Intel, add
   nothing.
3. **Ignore `deploy/overlays/`** unless a section here sends you there.

There is no per-host or per-vendor base file: a host that needs different ports,
a different image or a different render node sets those in `deploy/.env`.

| File | What it adds | Use it when |
|---|---|---|
| `docker-compose.yml` | The whole stack: Postgres, control plane, node agent | **Always** |
| `docker-compose.nvidia.yml` | CDI GPU exposure, driver capabilities, the NVIDIA driver volume | Your GPU is NVIDIA. **AMD/Intel add nothing** |

**Installing a release is not an overlay.** `deploy/docker-compose.yml` *is* the
production deployment: it pulls published images and carries no development
bind mounts. Pinning it to a release means setting `QUASAR_CONTROL_IMAGE` and
`QUASAR_AGENT_IMAGE` in `deploy/.env` to the digests from the release body, and
nothing else. (`docker-compose.release.yml` used to do that substitution and is
retired.)

```bash
# AMD / Intel
docker compose -f deploy/docker-compose.yml up -d

# NVIDIA
docker compose -f deploy/docker-compose.yml \
               -f deploy/docker-compose.nvidia.yml up -d
```

`deploy/redeploy.sh <va|nvidia>` builds the right chain itself, and is the
canonical full redeploy on the source path.

There is one more file at this level, `docker-compose.hardened.yml`. It is not
part of a normal deployment: it is the managed-certificate option covered under
[Advanced: using a real TLS certificate](#advanced-using-a-real-tls-certificate).

**`deploy/overlays/` is everything else** — situational and mostly for
contributors: a laptop-only agentless stack, multi-agent scheduling tests,
profiling, core dumps, and volume adoption. The one operator-facing member is
`docker-compose.console.yml` (console mode: the GPU host drives its own monitor
and speakers). [`deploy/overlays/README.md`](overlays/README.md) has the table.

---

# Part 3: Reference

## Prerequisites in detail

Verified end to end on a clean Fedora 44 box (2026-08-08); any systemd Linux
distribution with Docker should behave the same.

| Requirement | Notes |
|---|---|
| Linux host | `network_mode: host` (required for WebRTC ICE UDP) does not work on Docker Desktop for macOS or Windows |
| `git` | Not installed by default on Fedora; needed to get the source |
| Docker Engine + Compose v2.20+ | Verified against Compose v5.4.0 |
| Disk free for Docker | ~20 GB to install a release, 40 GB+ to build from source (~25 GB of images and build cache, plus headroom). Fedora's installer default 15 GB root LV is not enough for either — growing it (`lvextend` + `xfs_growfs`) was required on the test box. App images are extra: the KDE desktop image alone is ~1.2 GB compressed |
| RAM / CPU | Installing a release is download-bound. Building from source was measured on 8 vCPU / 32 GB; less works, it just takes longer |
| GPU: AMD/Intel (VA-API) or NVIDIA | Optional in principle (`QUASAR_ENCODER=openh264` encodes in software at reduced quality and throughput), expected in practice. NVIDIA hosts also need the driver plus `nvidia-container-toolkit` with CDI configured (`nvidia-ctk runtime configure`) |
| `/dev/uinput` | Virtual input devices (keyboard, mouse, gamepad injection) |
| `/dev/kmsg` | Kernel ring buffer, passed read-only with `CAP_SYSLOG`, so an NVIDIA Xid or amdgpu fault reaches the session trace instead of only the host's `dmesg`. Optional: on a kernel without it, drop the device and the capability from the node-agent service and the `xid_visibility` readiness check reports `skip`, which fails nothing |
| Node.js | **Not** required on the host — the web app builds inside a `node:22` container |
| SELinux | Enforcing is fine; zero denials observed on Fedora 44 |
| Network path between player and host | Same LAN segment, or a VPN that joins them. Media is peer-to-peer between browser and GPU host, so the control plane being reachable is not enough. See [Media reachability](#media-reachability-lan-or-vpn) |
| Host firewall | The control plane's published ports go through Docker's DNAT and bypass the host firewall. The node agent uses host networking, so its WebRTC UDP **is** subject to it, and a default-deny firewall silently drops the video. See [Host firewall blocking WebRTC media](#host-firewall-blocking-webrtc-media) |

Chrome publishes `.local` mDNS hostnames as its WebRTC ICE candidates. The
node-agent image resolves these itself (it ships avahi + nss-mdns and starts a
resolver-only avahi-daemon at container start), so nothing is needed on the
host. If ICE ever stalls at "checking", confirm the resolver is up:
`docker exec <node-agent container> avahi-daemon --check`.

## How TLS works by default

The control plane serves the app over HTTPS on `:8443` and plaintext HTTP on
`:8080`. HTTPS is not cosmetic here: it gives the browser a **secure context**,
which is required for in-game Esc capture (the Keyboard Lock API) and for
gamepad support. The HTTP listener stays up regardless — the node agent enrolls
over it and the healthcheck probes it.

With `QUASAR_TLS=auto` (the default, and what the quick start uses) a
self-signed certificate is generated on first boot and persisted in the
`quasar-control-tls` volume, so it survives restarts and an accepted browser
exception sticks. The startup log prints the certificate path and its SHA-256
fingerprint. To verify before accepting, compare the fingerprint the browser
shows against:

```bash
docker compose -f deploy/docker-compose.yml exec quasar-control-plane \
  sh -c 'openssl x509 -in /var/lib/quasar-control/tls/cert.pem -noout -fingerprint -sha256'
```

**The certificate's names** are `localhost`, the loopback addresses,
`QUASAR_PUBLIC_HOST`, everything in `QUASAR_TLS_HOSTS`, and the control plane's
own interface addresses. That last set does *not* cover your LAN: the control
plane runs in a container, so the only address it can see is its Docker bridge
address. That is why `QUASAR_TLS_HOSTS` exists and why the quick start sets it.
With it unset, `https://<host-lan-ip>:8443` fails with
`ERR_CERT_COMMON_NAME_INVALID` every time and accepting the exception never
clears it.

### Trusting the certificate (microphone and keyboard capture)

**Clicking through the browser's warning is not the same as trusting the
certificate, and the difference is visible in the product.** A bypassed
certificate still counts as a secure context, so the stream itself works
normally — video, audio playback, mouse look, gamepad. What does not work
reliably is anything the browser guards more tightly on an origin it has flagged
as having a broken certificate:

- **Microphone passthrough does not work.** Chrome will not durably store a
  microphone grant for such an origin, so voice either fails outright or
  re-prompts on every visit. Quasar surfaces this as a toast telling you to
  allow the microphone in the address bar — advice that cannot stick while the
  certificate is untrusted.
- **In-game keyboard capture misbehaves.** The Keyboard Lock API (what keeps Esc
  and other reserved keys inside the game instead of leaving fullscreen) can
  refuse to engage, and **it fails silently** — no error, no toast. The symptom
  is Esc dropping you out of the session instead of reaching the game.

Streaming works either way. Trust the certificate and both of the above start
working. Two ways out:

**Preferred: install the certificate on each client device.** Download it from
the control plane — this endpoint is deliberately unauthenticated, because you
need it before you can log in:

```bash
# The certificate, plus its fingerprint in a response header.
curl -k -OJ https://<host-ip>:8443/v1/tls/certificate.pem
curl -kI https://<host-ip>:8443/v1/tls/certificate.pem | grep -i fingerprint
```

**Compare that fingerprint against the one in the control plane's startup log
before you trust it** (`docker compose -f deploy/docker-compose.yml logs
quasar-control-plane | grep -i fingerprint`). Trusting a certificate you fetched
over an untrusted channel without checking it defeats the point.

Then install it, per client OS:

- **macOS** — open the `.pem` in Keychain Access (System keychain), then set it
  to *Always Trust* under Get Info → Trust. Or:
  `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain quasar-control-plane.pem`
- **Windows** — `certutil -addstore -f Root quasar-control-plane.pem` from an
  elevated prompt, or double-click the file → Install Certificate → Local
  Machine → *Trusted Root Certification Authorities*.
- **Linux (system store)** — copy it to `/etc/pki/ca-trust/source/anchors/`
  and run `sudo update-ca-trust` (Fedora/RHEL), or to
  `/usr/local/share/ca-certificates/quasar.crt` and run
  `sudo update-ca-certificates` (Debian/Ubuntu).
- **Chrome on Linux** additionally keeps its own store, so the system step alone
  is not enough:
  ```bash
  certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n quasar -i quasar-control-plane.pem
  ```
- **iOS/Android** — install the profile, then enable full trust for it in the
  OS certificate settings. Both platforms require that second step explicitly.

Restart the browser afterwards. Note that re-issuing the certificate (below)
changes its fingerprint, so every device has to be re-trusted.

**Or remove the problem entirely** by serving a certificate browsers already
trust: [front Quasar with your own reverse
proxy](#advanced-fronting-quasar-with-your-own-reverse-proxy), or
[use a real TLS certificate](#advanced-using-a-real-tls-certificate). If you
have public DNS and a proxy already, that is less work than trusting a
self-signed certificate on every device.

### Adding a name later

The key pair is generated once and reused for about ten years, which is what
makes an accepted browser exception stick. A name added to `QUASAR_TLS_HOSTS`
after first boot is therefore absent from the certificate until you re-issue it.
Add the name to `deploy/.env` first (`seed-tls-hosts.sh` only ever appends when
the value is empty, so it will not do this for you), then:

```bash
docker compose -f deploy/docker-compose.yml exec quasar-control-plane \
  rm -f /var/lib/quasar-control/tls/cert.pem /var/lib/quasar-control/tls/key.pem
docker compose -f deploy/docker-compose.yml up -d --no-deps --force-recreate quasar-control-plane
```

`--no-deps` matters: without it, Compose v5 also recreates Postgres, which can
kill the database mid-migration. The result has a **new fingerprint**, so every
client that trusted the old certificate has to trust the new one again.
`deploy/redeploy.sh` warns (it does not fail) when the served certificate is
missing a name that `QUASAR_TLS_HOSTS` lists, because that re-trust cost makes
re-issuing your decision, not the script's.

## Advanced: fronting Quasar with your own reverse proxy

**Optional.** Skip this unless you already run Caddy, nginx or Traefik and want
Quasar behind it — typically holding a real certificate for a name your internal
DNS resolves.

Point the proxy at this host's control-plane port and **preserve the WebSocket
upgrade for `/v1/signal`**. Without that the page loads and the session never
starts. Make sure the proxy sets `X-Forwarded-Proto` too: the control plane
derives its `wss://` signaling URLs from it.

Then set these in `deploy/.env` and recreate the control plane:

```bash
PUBLIC_BASE_URL=https://play.example.com
QUASAR_ALLOWED_ORIGINS=https://play.example.com
QUASAR_TRUSTED_PROXIES=192.0.2.10/32     # your proxy's IP, as this host sees it
QUASAR_ENV=production                     # optional, see below
```

- `PUBLIC_BASE_URL` is the name your users type. Set it, or a `Host`-rewriting
  proxy makes the control plane advertise its own private listener as the
  signaling address: the browser dials that instead of the proxy and the launch
  hangs forever, with nothing in the log to explain it.
- `QUASAR_ALLOWED_ORIGINS` must list every origin that has to keep working. Once
  a proxy fronts the stack the browser's `Origin` is the proxy's name, and
  same-origin (the default) no longer matches. Keep the LAN URL if you still use
  it: `https://play.example.com,https://192.168.1.50:8443`.
- **`QUASAR_TRUSTED_PROXIES` is the one that is easy to miss.** Without it every
  request looks like it came from the proxy, so every client shares a single
  rate-limit budget. On the first-admin claim endpoint that is a lockout:
  someone burns the shared budget with bad tokens and you can never create the
  first admin. With it, the control plane reads `X-Forwarded-For` — but *only*
  from a peer inside these networks — and limits on real client addresses. Name
  your proxy and nothing else: anyone on a network you list can mint unlimited
  budget. A `/0` is refused at startup; anything wider than `/8` boots with a
  warning.
- `QUASAR_ENV=production` is optional and cheap: it turns the dev-only
  agent-auth flag into a boot refusal rather than something to remember not to
  set.

The control plane keeps serving its self-signed certificate to the proxy, which
is fine on a link you control; set `QUASAR_TLS=off` if you would rather it talk
plaintext. Check your work in the access log: the `remote` field should show
real client addresses, not your proxy's.

## Advanced: using a real TLS certificate

**Optional.** The self-signed default is fine for LAN and VPN use, provided you
[trust it on each client
device](#trusting-the-certificate-microphone-and-keyboard-capture) — untrusted,
the microphone and in-game keyboard capture do not work. Replacing it with a
certificate browsers already trust removes that chore. Two ways.

**Bring your own certificate.** Bind-mount the PEM files into the container and
point the control plane at them:

```bash
QUASAR_TLS_CERT=/etc/quasar/tls/fullchain.pem
QUASAR_TLS_KEY=/etc/quasar/tls/privkey.pem
```

Both must be set. You own the renewal; the control plane reads them at boot, so
recreate the container after a renewal.

**Or let the bundled Caddy manage one.** For a host with no reverse proxy of its
own, `docker-compose.hardened.yml` (with `Caddyfile.hardened`) adds Caddy on
`:443` doing ACME issuance, renewal and HSTS, sets `QUASAR_ENV=production`, and
takes the control plane off its published ports so Caddy is the only listener:

```bash
docker compose -f deploy/docker-compose.yml \
               -f deploy/docker-compose.hardened.yml up -d
```

It needs `QUASAR_PUBLIC_HOST` and `QUASAR_TLS_DIR` in `deploy/.env`, and it
wants `QUASAR_TRUSTED_PROXIES` set to this compose project's own bridge network
— read it off `docker network inspect deploy_default` (e.g. `172.18.0.0/16`),
never the enclosing `172.16.0.0/12`, which would trust every other Docker stack
on the box.

Full variable list: `deploy/.env.example` and
[`docs/configuration.md`](../docs/configuration.md).

## Media reachability: LAN or VPN

The UI, the API and WebRTC signaling all run over TCP, so they work through any
reverse proxy. **The media does not go that way.** Video, audio and input ride a
direct connection between the browser and the node agent on the GPU host. ICE
can only pair addresses the two sides can actually reach, so the control plane
being reachable tells you nothing about whether the stream will come up.

### Start here: one network, or a VPN that acts like one

Put the browser and the GPU host on the same LAN segment and this needs no
configuration at all. To play from somewhere else, join both ends to one private
network first. Tailscale or an ordinary VPN both do this, and either is the
recommended way to reach a Quasar host from outside the house. Quasar runs no
relay of its own, so the browser offers host candidates only, which is all a
shared network needs.

### Multi-host: the client may have no route to the host that won

**Before the second host: the agent link is plaintext only.** A node agent
reaches the control plane over `CONTROL_PLANE_URL`, and the agent's WebSocket
client is built without TLS — a `wss://` value fails to connect. On a single
host that costs nothing, because the agent shares the host's network namespace
and dials `ws://localhost`. A second host puts that link on the wire, carrying
the enrollment token and then the per-node secret in cleartext; whoever reads
them can register a host of their own and be handed sessions. Run the
agent↔control-plane link over a private path you operate — a VPN, Tailscale, or
a dedicated link — and never over a shared network. This is a current
limitation, not a choice you configure.

Several GPU hosts can register to one control plane, and the scheduler places
each session on whichever host has capacity. That opens a case a single host
never has: the client reaches the control plane, browses the library and
launches fine, and then has no route to the machine that got the session,
because that is a different machine on a different network. Nothing about this
is exotic. It is what a two-host deployment looks like the first time the second
host wins a placement.

A relay is the standard answer to that case, and it is on the roadmap alongside
multi-host setup. Until it lands, join both ends to one private network.

### Checking which path a session actually took

Read the **selected candidate pair** in the browser, not a firewall counter.
`chrome://webrtc-internals` reports it, as does `getStats()` on the peer
connection: look at the succeeded candidate pair and its local and remote
candidates' `candidateType` and `protocol`.

This matters because **the node agent advertises ICE-TCP and IPv6 candidates as
well as UDP.** A test that blocks only UDP will still connect directly over TCP,
at full bitrate, and look like a network change that worked when nothing about
the path changed. A firewall drop counter cannot tell those apart. The candidate
pair can.

### Exposing the host to the internet instead

It works, and it is not what we recommend. You would need the HTTPS port (8443
by default) reachable for the UI, the API and signaling, plus the whole inbound
UDP media range reachable from wherever you play — a wide open port range on a
machine whose job is to run games as containers with a GPU attached. A VPN gets
you the same access without that. If you go ahead anyway, read
[HTTPS](#how-tls-works-by-default) and use the hardened reverse-proxy overlay
first. A player behind CGNAT is one more case with no direct route. A relay is
the standard answer to that case, and it is on the roadmap alongside multi-host
setup.

### When the video never arrives

**Symptom.** The launch screen sits there and the video never appears, while
everything else looks healthy: the session is `running`, a host owns it, the GPU
is idle and the agent is online. About two minutes later the node agent logs:

```
WARN quasar_node_agent::session::runner: reaping idle session: idle: WebRTC transport never established
```

The launch screen names this itself: once the session is running and the media
path has not come up within about 12 seconds, it stops reporting a scheduling
reason and says the media connection could not be established. If you still see
"Still looking for a host", the session genuinely has not been placed and this
section is not your problem.

**Checklist, in the order the causes are likely:**

1. **Same network?** Put the browser and the host on the same segment, or on a
   VPN that does. A browser on a different segment offers only mDNS
   (`<uuid>.local`) candidates, which the host cannot resolve across the
   boundary, and there is nothing to fall back on. This
   is the common case behind a reverse proxy: the proxy carries signaling
   perfectly, so nothing looks wrong until media is due.
2. **Firewall passing UDP?** See the next section — common enough on a
   default-hardened distro to have its own readiness check.
3. **Host networking actually in effect?** `network_mode: host` is Linux only.
   Docker Desktop on macOS or Windows cannot share the host network namespace.
4. **Is the host that got the session reachable by this client?** With more than
   one GPU host registered, the session may have been placed somewhere the
   client has no route to. The fix is a VPN that joins them. Confirm which path
   the session took by reading the selected candidate pair.

## Host firewall blocking WebRTC media

A default-deny host firewall silently drops the node agent's WebRTC media. This
is the single most common cause of "session launches, video never arrives" on a
freshly installed Linux server — the control plane and the login page work
perfectly the whole time, so nothing else points at it.

**Why it happens.** The control plane's ports are published through Docker,
which DNATs them straight past the host firewall — no rule needed. The node
agent is different: it runs with host networking (required so WebRTC UDP reaches
the browser at all), so its traffic is ordinary host traffic, filtered by
whatever the firewall's INPUT policy says. A distro that ships default-deny
blocks it out of the box, and every other health surface — the UI, the API,
session launch, even the session reaching `running` — keeps reporting fine,
because none of them go through that path.

**How Quasar tells you.** The node agent's `media_reachability` readiness check
runs a best-effort firewall probe at startup and on reconnect, and when it finds
a filtering posture it logs a `WARN` naming the exact rule to add for the
firewall tool it detected, surfaced in Admin → Hosts. Detection degrades to "no
finding" (not a failure) when no firewall client tool is reachable from inside
the container, which is the common case — so the absence of that warning is not
proof the host is open.

**What needs to be reachable.** Two things, both inbound to the GPU host from
your client devices, scoped to your LAN or VPN subnet:

- **UDP, the ephemeral port range.** This is what the kernel hands out for
  outbound sockets (Linux default `32768-60999`; read yours with
  `cat /proc/sys/net/ipv4/ip_local_port_range`). ICE and RTP both ride on it.
- **UDP/5353, mDNS.** Chrome sends `.local` hostnames as ICE candidates, and
  resolving them needs this. Without it, a filtered UDP range fails closed with
  no fallback.

Scope any rule to your subnet, not `0.0.0.0/0` — opening the whole range to the
internet turns your GPU host into an easy target, and nothing needs it. This is
why the fix is a scoped rule rather than "turn the firewall off".

**Fix, by firewall tool** (substitute `<lan-subnet>`, e.g. `192.168.1.0/24`, and
`<port-range>` with your host's actual range if it differs from `32768-60999`):

- **firewalld** (Fedora/RHEL/CentOS default):
  ```bash
  sudo firewall-cmd --get-default-zone   # confirm <zone>
  sudo firewall-cmd --permanent --zone=<zone> --add-rich-rule='rule family=ipv4 source address=<lan-subnet> port port=<port-range> protocol=udp accept'
  sudo firewall-cmd --permanent --zone=<zone> --add-service=mdns
  sudo firewall-cmd --reload
  ```
  On Fedora Server the default zone allows only ssh, cockpit and dhcpv6, so this
  is very likely the cause if you have not touched the firewall since install.

- **ufw** (Debian/Ubuntu):
  ```bash
  sudo ufw allow proto udp from <lan-subnet> to any port <port-range>
  sudo ufw allow from <lan-subnet> to any port 5353 proto udp
  sudo ufw reload
  ```

- **nftables** (raw): add an INPUT accept rule for UDP `<port-range>` and
  `5353/udp`, scoped to `<lan-subnet>`, ahead of the default-deny rule in your
  base input chain.

- **iptables**:
  ```bash
  sudo iptables -I INPUT -p udp -s <lan-subnet> --dport <port-range> -j ACCEPT
  sudo iptables -I INPUT -p udp -s <lan-subnet> --dport 5353 -j ACCEPT
  ```
  inserted ahead of your existing default-deny rule, then persisted however your
  distro expects.

If the client and the GPU host are on different networks rather than one, a
firewall rule is not the answer on its own — see
[Media reachability](#media-reachability-lan-or-vpn).

## GPU encode

### AMD / Intel (VA-API)

Add to `deploy/.env`:

```
QUASAR_ENCODER=va
LIBGL_ALWAYS_SOFTWARE=
MESA_LOADER_DRIVER_OVERRIDE=
```
`QUASAR_RENDER_NODE` can stay unset — the agent binds to the scheduled GPU. Set
it (e.g. `/dev/dri/renderD128`, or a reboot-stable `/dev/dri/by-path/...`
selector) only to pin a specific GPU on a multi-GPU host.

Verify inside the container:

```bash
docker run --rm --device /dev/dri quasar-agent-dev:latest vainfo
docker run --rm --device /dev/dri \
  -e GST_REGISTRY=/tmp/reg.bin \
  quasar-agent-dev:latest gst-inspect-1.0 vah264enc
```

### NVIDIA and Vulkan

Every image is built with a patched GStreamer carrying the Vulkan encoders, so
there is no separate image to install, and there is no separate NVIDIA image
either — `quasar-node-agent` is what an NVIDIA host runs.

**On NVIDIA hosts Vulkan is the default**: H.264, HEVC and AV1 encode with the
Vulkan encoders. Where a Vulkan encoder is unavailable for a codec, that session
falls back to the NVIDIA hardware encoder instead. That fallback needs the
`cuda*` GStreamer elements, which need `libnvrtc` — CUDA toolkit userspace that
no driver installer carries. The agent fetches it at launch into the driver
volume; if it cannot, or the driver is older than r580, the fallback is simply
unavailable and Vulkan encode is unaffected.

Set `QUASAR_ENCODER=nvenc` in `deploy/.env` to put the whole host back on NVENC.
With `QUASAR_ENCODER` unset the agent auto-detects the GPU vendor: NVIDIA and
AMD default to `vulkan`, Intel to `va`, and a host with no GPU to `openh264`;
`QUASAR_ENCODER=va` keeps an AMD/Intel host on VA-API explicitly.

Per-codec knobs (`QUASAR_VULKAN_H264` / `QUASAR_VULKAN_HEVC` /
`QUASAR_VULKAN_AV1`, all on by default, set to `0` to disable one) and the
headless-decode caveat are in
[`docs/configuration.md`](../docs/configuration.md).

## Services

### `quasar-postgres`

Postgres 16-alpine with a persistent named volume. The control plane migrates
its own schema at startup — no manual migration step.

### `quasar-control-plane`

Serves:

- `/v1/*` — the JSON API
- `/agent/ws` — the node-agent WebSocket
- `/v1/signal` — the authenticated WebRTC signaling relay
- `/health` — liveness (used by the compose healthcheck)
- `/` — the web app

It is the single public ingress unless you put Caddy in front of it with
`docker-compose.hardened.yml`.

### `quasar-node-agent`

Runs the node-agent binary with all GStreamer plugins, using host networking
(**Linux only**) so WebRTC UDP reaches the browser. It reaches the control plane
over `localhost:8080`, and launches game containers and audio sidecars as
sibling containers through the mounted Docker socket.

## Upgrading, backups, and rollback

Before pulling a new version onto a running stack, back up Postgres and read
[`docs/upgrading.md`](../docs/upgrading.md). It covers the backup command, the
upgrade steps, and the fix for the crash loop you get if you roll a control-plane
binary back *below* the database's applied migration version.

### Narrow redeploys (source path)

`deploy/redeploy.sh` takes a third argument that rebuilds one component instead
of everything. Each still force-recreates the control plane and runs the same
post-deploy verification, including the wait for healthy — which is what proves
an embedded migration finished.

| scope | Rebuilds | Typical cost |
|---|---|---|
| `all` (default) | web app + node-agent image + control plane | 25–40 min |
| `web` | the web app | ~2 min |
| `control` | the Go control plane | ~1 min |

```bash
bash deploy/redeploy.sh nvidia my-branch control
```

Neither narrow scope touches the node-agent image or container, so running
sessions survive.

## Stopping and cleanup

```bash
# Stop everything
docker compose -f deploy/docker-compose.yml down

# Also remove volumes — wipes the database and the agent's identity,
# so you will re-enroll and re-claim the admin account
docker compose -f deploy/docker-compose.yml down -v
```

## Installing v0.1.0 (extra steps)

`v0.1.0` predates three fixes to the release path and needs two extra steps,
both applied **before** step 4's `up -d`. Its images also carry the old names:
`quasar-control`, and `quasar-vulkan` (AMD/Intel) or `quasar-nv` (NVIDIA).

Note that you install a release from *that release's own tree*, so the compose
files in play here are `v0.1.0`'s, not the ones described above — `v0.1.0` still
has a `docker-compose.release.yml`, and step 4 for it is the older
`COMPOSE="-f deploy/docker-compose.yml … -f deploy/docker-compose.release.yml"`
form that tag's own README documents.

```bash
# a) The release control image runs as a non-root user, and a fresh Docker
#    named volume is created root-owned, so the control plane cannot create
#    its TLS directory and exits at boot. Pre-create the volume and give it
#    to uid 1000.
docker volume create deploy_quasar-control-tls
docker run --rm -v deploy_quasar-control-tls:/v alpine chown 1000:1000 /v

# b) v0.1.0's release overlay empties the control-plane's volume list (to drop
#    a development bind mount) and takes the TLS volume with it, and the base
#    healthcheck calls wget, which that image does not ship — so the container
#    would stay unhealthy and the node agent would never start. Restore both.
cat > deploy/docker-compose.release-fixups.yml <<'YAML'
services:
  quasar-control-plane:
    volumes:
      - quasar-control-tls:/var/lib/quasar-control
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:8080/health >/dev/null || exit 1"]
YAML
```

Then append `-f deploy/docker-compose.release-fixups.yml` to that tag's own
compose chain. Releases after `v0.1.0` need none of this.

**Image names changed after v0.1.0.** The rename to `quasar-control-plane` /
`quasar-node-agent` lands in the next release, and both names are published for
one transition window, so a pin to either resolves to the same digests. The
third, `quasar-nv`, has no successor: the separate NVIDIA lineage is retired and
an NVIDIA host runs `quasar-node-agent` like every other host.

A release carries an immutable `:X.Y.Z` tag on both runtime images alongside the
digests. Prefer the digest — a tag is a pointer, and the release overlay is
explicitly a pin.

---

## Where the rest lives

- [`docs/configuration.md`](../docs/configuration.md) — every environment
  variable, its default and its accepted values.
- [`docs/upgrading.md`](../docs/upgrading.md) — backups, upgrades, rollback.
- [`docs/developer-tooling.md`](../docs/developer-tooling.md) — contributor
  tooling: the dev container, the acceptance harnesses, image builds
  (`deploy/build-images.sh`), the release gates, and the non-operator compose
  overlays.
- [`deploy/overlays/README.md`](overlays/README.md) — the situational overlays.
