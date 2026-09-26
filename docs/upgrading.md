# Upgrading, backing up, and rolling back

This page is for a self-hoster who already has Quasar running and wants to move to a
newer version, or who has hit a problem after an update and wants to go back.

The short version: an install made with the seed updates itself from the console. Its
recovery actor takes a database dump before any update that changes the database, and
Quasar never runs an older control plane against a newer database. The rest of this page
explains how that works, what to do when it does not, and what the other kinds of install
do instead.

## Which kind of install you have

- **An owned install** (installed with the seed: the one-line command, a `docker run` line,
  or a one-service stack in Dockge or Arcane). One small container, the seed, is the only
  thing you declared; Quasar's **recovery actor** on each machine creates, updates and
  recovers the rest. Everything on this page up to "Replacing an install made from the
  Compose files" is about this kind. How to make one: `docs/configuration.md` "Seed" and the
  site's Install pages.
- **An install made from the Compose files before owned installs existed**
  (`deploy/docker-compose.yml` plus `deploy/.env`, with or without `quasar-updater`). It keeps
  running the release it has. It is **not offered** any release that ships owned installs, or
  anything after it, and nothing converts it in place: it is replaced by a fresh seed install
  ("Replacing an install made from the Compose files" below).
- **A source install** (the contributor lane: `deploy/redeploy.sh`, `make redeploy-cp`,
  `make rebuild`). It builds its own images and is never updated from the console
  ("Source installs" below). It is contributor tooling, not a supported install.

A machine is one kind or the other, never both: two installations on one container engine
are not supported.

## Which version to move to

Which releases the console offers is one setting, Admin › Fleet › Releases › **Channel**. It
changes what is *listed*; it never installs anything and never starts a check.

- **`stable`** (the default) — tagged releases with notes. **Prereleases are hidden.**
- **`beta`** — the same tagged releases *and* the prereleases among them.
- **`edge`** — whatever was last published from `release_edge_branch` (default `develop`).
  No version, no notes, a compare link instead. Builds that ship owned installs are published
  under the `o2-<branch>` image tags and no longer move `<branch>`, so a control plane from
  before owned installs, on `edge`, stays on the last build it could run and is never offered
  one it cannot.

A tagged release publishes `platform-release-manifest.v2.json`: the control plane, node agent
and recovery actor by digest, and the **floor**, the oldest node agent and recovery actor the
release still manages ([schema](../scripts/release/platform-release-manifest.md)). Until a
tagged release carries owned installs, an owned install follows the `edge` channel; the stable
and beta channels then list nothing, which is not a fault.

Beta stores nothing of its own: a prerelease is already detected and cached alongside the
stable releases, and beta is the channel that lists it. **What beta costs you:** a prerelease
has not been through a release cut and can carry a migration the next prerelease revises; a
migration is one-way ("The one-way migration rule" below).

**Ordering is by version, not by publication date.** Beta orders its list by SemVer
precedence — `0.2.0-rc.1 < 0.2.0-rc.2 < 0.2.0 < 0.2.1-rc.1`, and `0.3.0-rc.9 < 0.3.0-rc.10` —
so the newest thing on the list is the highest version, never merely the most recently built.

### Switching channel never rolls you back

Suppose the instance runs `0.3.0-rc.1` and you switch back to `stable`. The newest release
stable can see may be `0.2.5`, older than what you run. **No channel offers a build that
orders below the one installed.** Until stable passes you, the Releases list is empty
(*"Nothing newer than this control plane has been detected on the stable channel."*) and each
target reads *"Nothing newer has been detected on this channel."* That is not a failure;
switching back restores the list. The reason is the one-way migration rule: a downgrade is
unrepresentable here, not merely discouraged.

---

## How an owned install is updated

Every machine runs one recovery actor. It is the only thing that creates or replaces that
machine's Quasar containers, it works through the container engine directly, and it keeps its
journal, the machine's secrets and its settings in the `quasar-machine` volume, **the one
volume that must never be deleted**. Nothing reads or writes a Compose file or an `.env`.

An update is a **replacement**: the actor pulls the new image by digest, stops the old
container and **keeps** it (renamed `<name>.kept`, its restart policy disabled), starts the new
one and verifies it (running, and healthy if the image has a health check). A verified
container replaces the kept one; one that does not verify is removed and the kept one started
again, with no pull (ADR 0004). An attempt may replace several services, and the recovery actor
always **moves first**: it replaces itself by handing over to a successor, which must verify
itself before the old actor is discarded (ADR 0008). If the actor, the engine or the machine
restarts part-way, the actor settles the attempt on its next start: interrupted before anything
was taken out of service, it ends `failed`/`interrupted` with nothing changed; after, it
continues to verification. Nothing is retried on its own.

- **A GPU host** is replaced through its node agent, which relays the console's request to the
  recovery actor beside it. Replacing the agent **ends that host's sessions.**
- **The control plane** is replaced by the recovery actor on its own machine, over that
  machine's control socket, never through a node agent. The console and API go away for the
  length of the replacement. The control plane writes the request down before it asks, and the
  build that boots reads the actor's verdict back. A new control plane that never becomes
  healthy is put back automatically, unless the release changes the database (below).

### Applying from the console

Admin › Fleet › Releases lists every target and, for an eligible host, offers **Apply**. The
host is cordoned, the attempt sits in **waiting_sessions** showing how many sessions are still
running, and only when the count reaches zero is the apply handed to that host's recovery
actor. The cordon is then restored to whatever it was before. The confirmation's **force**
checkbox skips the wait and names the number of live sessions it ends, because replacing the
agent ends them all either way.

Success is the host registering again on the release's commit, which is why a successful apply
is reported by the *new* agent. A failed apply records the previous digests and shows the
reason. If the new agent never came up, the recovery actor put the previous one back itself and
the history shows "Reverted automatically" beside the failed apply, with the failed container's
last log lines in the failure. A pull that failed leaves the old agent running.

**A host step that ends in `timeout`.** The verdict of a host apply is the recovery actor's, and
it comes home over that host's agent. When no agent comes back to relay it, the attempt can
only expire on its apply deadline (15 minutes). The failed attempt carries the request id; ask
the recovery actor on that host, not the node agent:

```bash
docker exec quasar-recovery cat /var/lib/quasar-machine/journal/<request-id>.json
docker exec quasar-recovery quasar-recovery status
```

The journal is the verdict: the reason, the failed container's last log lines, and the previous
digests. No such file means the actor never admitted the request and nothing on the host was
changed.

### Update Quasar from the console

**Update Quasar** moves the whole instance in one action, in one order that is not
configurable: **the control plane first, then every eligible host, one at a time.** An agent is
never moved past the control plane (ADR 0002).

1. **The whole fleet is cordoned, and drains first only if the release changes the database.**
   A release that runs no migration does not wait: live sessions stream straight through the
   control-plane restart. A release that carries a migration waits for the instance to empty
   (force skips the wait and ends them), because every migration was written assuming no
   session was live while it ran. The confirmation tells you which before you press Update.
2. **The control plane is replaced by the recovery actor on its machine.** On a combined or
   control-only machine the recovery actor moves first (it may be one release ahead of a
   control plane it then restores, ADR 0008's A1 exception).
3. **The new build reports the outcome**, and the run, persisted in Postgres, resumes on it.
4. **Each host follows**, cordoned, drained, replaced and uncordoned as a per-host Apply is.
5. **The run stops at the first target that fails**, and says which.
6. **A run that passed a host over ends "succeeded_partial".** **Retry skipped hosts** starts a
   plain fleet apply of the same release once the cause is fixed.

**Hosts that cannot take the release are skipped, not failed**: an offline host, a host below
the floor (it reads "must update before it can be managed" and is offered only an update), one
with no recovery actor answering, one whose preflight checks fail. **Every target is checked
before Update is offered**: its recovery actor answers, no container on its machine looks like a
Quasar service without this installation's labels (an owner conflict, below), there is room
for the pre-update dump when the release migrates, the release's images resolve at the
registry, and for a host its agent's health port is answered by that agent. A failing check
makes the target **Blocked** with the fix named; a check that could not be evaluated warns and
never blocks.

**Cancel stops the run before its next target and never interrupts one in flight.** An admin
tab left open across the control-plane step shows "Quasar was updated" with a Reload button.

### An update that changes the database

A release **migrates** when its schema is above the installed control plane's. Before replacing
the control plane with it:

- **Quasar's own database.** The recovery actor checks the machine has room, then takes a
  **pre-update dump** (`pg_dump --format=custom`) into `dumps/` in machine state, and checks it
  reads back. A dump that fails, does not fit or does not read back refuses the step
  `backup_failed`: the control plane was not replaced and the database was not touched. The
  last three dumps are kept.
- **Your own database.** Quasar never dumps, restores or resets it. The update asks you to
  confirm that you have a current backup of it (the console's checkbox); without it the step is
  refused `backup_unconfirmed` before anything stops.

A migrating update is **never restored automatically**. If the new control plane does not
verify, it is left running (it matches the migrated schema) and the old one stays kept. The
failed attempt names its dump, and its output ends with the one command that goes back, run on
that machine:

```bash
docker exec quasar-recovery quasar-recovery restore --dump <name> --to <version>
docker exec quasar-recovery quasar-recovery restore --to <version>   # your own database, once you restored your backup
docker exec quasar-recovery quasar-recovery restore --list           # the dumps kept here
```

`--to` is the version the control plane was on. The restore checks the dump before touching
anything (checksum, readable, schema matches the version named), holds every control plane off,
loads the dump into a fresh database, starts that version's control plane and waits for it to
report healthy; a restart part-way continues it. Details: `docs/configuration.md` "A migrating
control-plane update, and `restore`".

### Reverting an agent

A host row also offers **Revert** once that host has one succeeded update behind it: an apply
with the digests recorded as `previous_digests` on its last succeeded attempt. On an owned host a
revert replaces the agent first, then the recovery actor, and never goes below the control
plane's floor. **The control plane is never revertible from the console, at any depth**: the
only way back is a restore of a pre-update dump. Agents never move above the control plane's own
release (`release_above_control_plane`).

### Installing updates automatically

**Off by default.** Settings ▸ Platform updates ▸ *Install updates automatically*
(`platform_auto_apply`). With it on, Quasar applies a detected release through exactly the fleet
run **Update Quasar** starts, when release detection next runs (Jobs ▸ **Platform release
detection**; move the job and you move the update hour).

- **A release that changes the database is never installed this way.** It is still listed and
  waits for you.
- **An automatic update is never forced**: it always waits for sessions.
- **A failure stops that release, not the feature.** Quasar does not retry that release
  automatically; a newer one is still installed, and applying the failed one yourself clears the
  block. A run you cancel is not a failure.
- **Where to read what happened.** Jobs ▸ Platform release detection ▸ its latest run: the
  summary carries `auto_apply` — `started` with the run id, or why not (`carries_migration`,
  `no_release`, `not_eligible`, `in_flight`, `failed_before`).

### Developer apply

An admin can apply an arbitrary digest set from an allowlisted registry namespace to one owned
target, without it being published as a release: the product lane's way to test a branch build
on the path users run. It is never offered and never unattended. Setup and rules:
`docs/configuration.md` (`QUASAR_UPDATER_ALLOWED_NAMESPACES`, `QUASAR_PLATFORM_INSECURE_REGISTRIES`).

---

## Back up the database

**Quasar's own database** lives in the `quasar-postgres-data` volume. The recovery actor dumps it
before every update that changes it (above), and keeps the last three. For a backup of your own,
off the machine:

```bash
docker exec quasar-postgres \
  pg_dump --format=custom --no-owner --no-privileges -U quasar quasar \
  > quasar-backup-$(date +%Y%m%d%H%M%S).dump
```

Keep the `.dump` file somewhere off the host: it is a full copy of your accounts, app catalog
and session history. The generated secrets (the database password and `QUASAR_SECRET_KEY`) live
in the `quasar-machine` volume; `QUASAR_SECRET_KEY` protects only values you can enter again (the
cover-artwork key, the release-notification signing secret).

**Your own database** is yours to back up, before any update that changes it.

`deploy/db-backup-restore-drill.sh` is a rehearsal of `pg_dump` and `pg_restore` in a disposable
Postgres, safe to run any time: `bash deploy/db-backup-restore-drill.sh`.

## The one-way migration rule

The control plane runs its database migrations on boot, and they only move forward. **Never run
a control-plane binary that is older than the migration version already applied to its
database**: it cannot start. The database is not damaged; the binary is asking for a migration it
was never built with, and says so in its log with the fix.

On an owned install Quasar enforces this: before a migrating control plane starts, the recovery
actor records the schema it may migrate to, and no control plane whose image declares a lower
schema is ever created, started or put back on that machine again, until a `restore` lowers it.
The only way back from a migration is a restore of a dump taken before it. On a source install
the fix is to redeploy the newer ref that has the migration (`deploy/redeploy.sh <va|nvidia> <ref>`),
not to hand-edit `schema_migrations`.

---

## When the recovery actor does not answer

The console reads a machine whose recovery actor does not answer as `updater_absent` ("No
recovery actor answers on this machine"), and nothing on it can be updated until it does. On
that machine:

```bash
docker ps -a --filter name=quasar-recovery
docker logs --tail 50 quasar-recovery
```

- **It is not there at all.** The seed re-creates it from the last verified recovery-actor
  image within 30 seconds, as long as the seed is running. A seed that is stopped re-creates
  nothing: start it again.
- **It exits at start.** Its last log line names the reason by token (`docs/configuration.md`
  "Recovery actor" lists them).
- **A hand-over was interrupted and no actor starts.** The old actor is kept:
  `docker start quasar-recovery.kept 2>/dev/null || docker start quasar-recovery`.

`docker exec quasar-recovery quasar-recovery status` prints the machine's inventory and one line
for every service that is missing, unhealthy or in the way.

**An owner conflict** is a container that looks like a Quasar service but lacks this
installation's labels: a leftover Compose stack, or a definition a stack manager still holds. The
recovery actor never acts on it, and the target reads **Blocked** naming it. Remove it, and the
stack or manager definition that re-creates it.

## Changing install-time settings, and taking a machine apart

- **Install-time settings** (home root, template root, release trust, app-container defaults) are
  recorded in machine state at the first install and change with `quasar-recovery reconfigure`, a
  verified replacement with the same images and new inputs. The role, node name, database and
  images are fixed at install. `docs/configuration.md` "Changing machine inputs: `reconfigure`".
- **A GPU host** is removed from the console: the host → **Remove host** drains it, then removes
  its agent and recovery actor. Any machine can be taken apart on the machine with the recovery
  image's `uninstall`, which keeps the database, machine state and homes unless `--purge`.
  `docs/configuration.md` "Taking a machine apart".
- Removing or redeploying the **seed** in your stack manager never stops or removes Quasar.

---

## Replacing an install made from the Compose files

An install started from `deploy/docker-compose.yml` and `deploy/.env` is not converted in place.
It keeps running its release, and its console shows no newer release rather than an error: the
releases that ship owned installs publish only the format-2 manifest, which an older control
plane cannot read, and their edge builds use the `o2-` tags it never resolves. So nothing forces
the move, and the old stack keeps working while you prepare it.

Moving over is a **fresh install**. Carrying the old database across is not supported in this
release: the new install starts with an empty database, so accounts, apps and hosts are set up
again. (A restore of a pre-RH-06 dump into a fresh install is planned, #380; if you want your
history, keep the old stack running until it exists.)

1. **Note what you will set up again**: users, the app catalog, each GPU host's node name and
   home root.
2. **Take a database dump of the old stack** and keep it, with the old `deploy/.env`:
   `docker compose -f deploy/docker-compose.yml exec -T quasar-postgres pg_dump -Fc -U quasar quasar > quasar-final.dump`.
3. **Stop the old stack without deleting its volumes** (`docker compose -f deploy/docker-compose.yml down`,
   no `-v`), on the control-plane machine and on every GPU host (an earlier one-line agent install:
   `docker compose --project-directory /opt/quasar-agent down`). A leftover container would be an
   owner conflict and would hold the ports.
4. **Install with the seed**, pointing it at the same home root so saves and installed games
   are found where they are; nothing is copied.
5. **Add each GPU host** from Admin › Fleet › Add host.
6. **Keep the old volumes and `deploy/.env`** until the new install is verified. They are your way
   back: `docker compose -f deploy/docker-compose.yml up -d` restores the old stack as it was.

## Legacy Compose installs

A stack made from the Compose files keeps working on the release it has. It has no recovery
actor, so the console never updates it (its targets read `updater_absent`); it can still be moved
by hand between the releases published **before** owned installs, with those releases' own files.

### Upgrading a registry install

A **registry install** runs published images pinned by digest in `deploy/.env`. It upgrades by
re-pinning the two digests and recreating the two containers they name. Take the digests from
that release's `platform-release-manifest.json` asset (control-plane, then node-agent). The
admin Releases page shows the same commands for a target with no recovery actor.

```bash
# 1. Pin the release's digests — the two lines in deploy/.env.
QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@sha256:<control-plane digest>
QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@sha256:<node-agent digest>

# 2. Pull the pinned images and recreate only those two services.
docker compose -f deploy/docker-compose.yml pull quasar-control-plane quasar-node-agent
docker compose -f deploy/docker-compose.yml up -d --force-recreate --no-deps quasar-control-plane quasar-node-agent
```

- **Repeat every `-f` you deploy with**; a compose invocation that drops an overlay recreates the
  containers without it.
- **Recreating the node agent ends every session on that host**, and **recreating the control
  plane runs migrations**, so the one-way rule applies from that moment.

### One-time: the control-plane TLS volume's ownership

Only for a stack that was built from source before both control-plane images ran as uid 1000,
and only once. A root-owned `quasar-control-tls` volume makes a published control plane
crash-loop with `tls: write TLS key ... permission denied`. Give the volume to uid 1000:

```bash
docker volume ls --format '{{.Name}}' | grep quasar-control-tls
docker run --rm -v <that name>:/t alpine chown -R 1000:1000 /t
docker compose -f deploy/docker-compose.yml up -d --no-deps quasar-control-plane
```

`deploy/redeploy.sh` does this on every deploy that touches the control plane.

## Source installs (the contributor lane)

A source install builds its own images from a checkout. It is never updated from the console
(its targets read `install_mode_source`, with the redeploy command beside them) and a recovery
actor never acts on it. It is contributor tooling, not a supported install.

1. Back up the database (as for a legacy stack: `docker compose ... exec -T quasar-postgres pg_dump ...`).
2. Redeploy. On a host set up with `deploy/redeploy.sh`, one command rebuilds every component
   from the ref you name, brings the stack back up and waits for it to report healthy (which is
   also waiting for any migration):
   ```bash
   deploy/redeploy.sh <va|nvidia> <ref>
   ```
   `va` for an AMD/Intel host, `nvidia` for an NVIDIA host. If only the control plane changed,
   `deploy/redeploy.sh <va|nvidia> <ref> control` (or `make redeploy-cp HOST=<host>`) takes about
   a minute and leaves the agent and its sessions alone.
3. Confirm the stack is healthy and on the version you expect.

A fresh source stack's agent enrolls with a token this control plane minted (Admin › Fleet › Add
host, then `ENROLLMENT_TOKEN` in `deploy/.env`, as `deploy/.env.example` describes): there is no
fleet-wide static enrollment token.

---

## Release notifications

The Releases page shows a banner when an update appears. That only helps someone who is looking
at it. **Fleet ▸ Releases ▸ Notifications** takes a webhook URL and POSTs one message when the
detector finds a release this instance could move to. A webhook rather than email: a URL and a
POST already work with Slack, Discord, ntfy, or a script behind a reverse proxy.

### Wiring one up

1. Get an incoming-webhook URL from wherever you want the message. Slack: *Incoming Webhooks* →
   *Add New Webhook to Workspace*. Discord: channel *Settings* → *Integrations* → *Webhooks* →
   *New Webhook* → *Copy Webhook URL*. ntfy: `https://ntfy.sh/<your-topic>`.
2. Paste it into **Webhook URL** and press **Save URL**.
3. Press **Send test**. A test goes out whether or not notifications are switched on, and it
   records nothing.
4. Press **Turn notifications on**.

**The URL is a credential.** Quasar never shows it in a log line, in the audit record of the
setting change, or in a delivery error.

### What is delivered

One `POST` with a JSON body:

```json
{
  "event": "platform.release.detected",
  "sent_at": "2026-09-08T02:00:11Z",
  "text":    "Quasar 0.2.4 is available. This instance is on 0.2.3 — open Fleet ▸ Releases to apply it.",
  "content": "Quasar 0.2.4 is available. This instance is on 0.2.3 — open Fleet ▸ Releases to apply it.",
  "instance": { "version": "0.2.3", "source_commit": "abc1234", "schema_version": 78, "channel": "stable" },
  "release":  { "id": "…", "channel": "stable", "version": "0.2.4", "source_commit": "def5678",
                "built_at": "…", "schema_version": 79, "prerelease": false,
                "compare_url": null, "notes_excerpt": "### Added\n- …" }
}
```

`text` and `content` are the same sentence under the two field names Slack and Discord read.
`notes_excerpt` is bounded; the full notes are on the Releases page.

### Signing (optional)

A receiver you wrote yourself can ask for proof: store a secret under **Secrets → Release
notification signing secret** (or set `QUASAR_PLATFORM_RELEASE_WEBHOOK_SECRET`). Every delivery
then carries

```
X-Quasar-Timestamp:     1757295611
X-Quasar-Signature-256: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<raw body>")>
X-Quasar-Delivery:      <one per notification, repeated across its retries — dedupe on it>
```

Verify by recomputing over the **raw** body you received and comparing in constant time:

```python
# flask, for illustration
import hashlib, hmac, os, time
secret = os.environ["QUASAR_WEBHOOK_SECRET"].encode()
ts   = request.headers["X-Quasar-Timestamp"]
want = "sha256=" + hmac.new(secret, ts.encode() + b"." + request.get_data(), hashlib.sha256).hexdigest()
assert hmac.compare_digest(want, request.headers["X-Quasar-Signature-256"])
assert abs(time.time() - int(ts)) < 300          # reject a stale replay
```

### Rules worth knowing before you rely on it

- **The same release is announced once**, and a fresh install does not announce its back
  catalogue.
- **A failure retries, then stops**: a few times within the pass, then on each following
  detection pass, up to five passes. The Notifications card shows the last attempt and its
  error; the detection job's run summary carries `notify`, `notify_reason` and
  `notify_status_code`.
- **A failing webhook never fails detection.**
- **`https` only, and public addresses only**: a receiver on the LAN or on localhost is not
  reachable; put a public https endpoint in front of it. `QUASAR_PLATFORM_WEBHOOK_HOSTS` narrows
  the destination further (`docs/configuration.md`).
- Clearing the URL switches notifications off in the same save.

---

## Cutting a release

This is for a maintainer publishing a new Quasar version, not for a self-hoster upgrading one.

`make release VERSION=x.y.z` (`scripts/release/release-cut.sh`) is the one command: on a clean
`main` that matches `origin/main`, it moves `CHANGELOG.md`'s `## Unreleased` section into a dated
`## X.Y.Z — YYYY-MM-DD` section directly above the old one, leaving a fresh empty
`## Unreleased`, commits that (`chore(release): x.y.z`), tags the commit `vX.Y.Z` (annotated),
and pushes both. Pushing the tag triggers the tag-push release lane
(`.github/workflows/images.yml`): it builds and validates the control-plane, node-agent and
recovery-actor images, promotes them, and publishes a GitHub Release whose body is that
version's changelog section, with a `platform-release-manifest.v2.json` asset naming the three
images by digest and the floor ([schema](../scripts/release/platform-release-manifest.md)).
Before it publishes, `scripts/release/check-release-compatibility.sh` refuses a release whose
recovery actor cannot render its images' recipes or reach back to the floor, or whose floor lies
above the previous release. No format-1 `platform-release-manifest.json` is published, so a
control plane from before owned installs is never offered the release. The release notes end
with an "Install or upgrade" section generated from the manifest. Publish the public
documentation from the released tree after this workflow succeeds (`pages.yml` is manually
dispatched).

It refuses — with a one-line reason, before touching anything — unless:

- the repo is on `main`, with a clean working tree that matches `origin/main`
- `VERSION` is strict semver (`X.Y.Z`, an optional `-prerelease` part is allowed for a release
  candidate; no leading `v`, no build metadata) and strictly newer than the newest existing `v*`
  tag
- the `## Unreleased` section is non-empty

It never merges `develop` into `main` for you — that merge, and the operator sign-off it requires
(`CLAUDE.md`, "Git branching & environments"), happens first, by hand. Add `DRY_RUN=1` to see the
changelog diff and the exact git commands it would run without executing any of them:

```bash
make release VERSION=0.2.0 DRY_RUN=1   # preview
make release VERSION=0.2.0             # cut, commit, tag and push v0.2.0
```

A prerelease tag (`v0.2.0-rc.1`) runs the same workflow and publishes a GitHub prerelease instead
of a stable release.

---

## Signing platform releases

Optional, and off on both sides until someone turns it on. A release may publish a detached
signature over its manifest, and a machine may be configured to verify it before applying
anything. What gets installed is still the pinned digest (ADR 0001); the signature answers
whether the digest set came from whoever holds the release key. Format:
`scripts/release/platform-release-signature.md`. Decision record:
`docs/adr/0003-release-signatures.md`.

### Turning it on: the publishing half

Done once, by the maintainer who publishes releases.

1. **Generate a key pair, off CI, on a machine you trust.** Not in the repo — the script refuses
   to write inside the working tree.

   ```bash
   scripts/release/new-release-signing-key.sh \
     --out ~/.config/quasar/release-signing-2026.pem \
     --key-id quasar-release-2026
   ```

   It prints the public key as `quasar-release-2026:<base64>`. Back the private key up; there is
   no recovery from losing it, only a rotation.

2. **Create the CI secret and the label variable**:

   ```bash
   gh secret   set QUASAR_RELEASE_SIGNING_KEY    --repo <owner/name> < ~/.config/quasar/release-signing-2026.pem
   gh variable set QUASAR_RELEASE_SIGNING_KEY_ID --repo <owner/name> --body 'quasar-release-2026'
   ```

3. **Cut a release as usual.** The `release` job signs the manifest right after it validates it,
   verifies its own signature before uploading anything, and attaches
   `platform-release-manifest.v2.json.sig` beside the manifest. With no secret configured the step
   prints one line and does nothing.

4. **Check the release**:

   ```bash
   gh release download vX.Y.Z --pattern 'platform-release-manifest.v2.json*'
   scripts/release/verify-platform-release-manifest.sh \
     --manifest  platform-release-manifest.v2.json \
     --signature platform-release-manifest.v2.json.sig \
     --public-key quasar-release-2026:<base64>
   ```

### Turning it on: the verifying half

Each machine's recovery actor verifies, with the release trust recorded in its machine state.
Give a new machine its settings as seed inputs at install (`QUASAR_UPDATER_SIGNATURE_MODE`,
`QUASAR_UPDATER_TRUSTED_KEYS`; `docs/configuration.md` "Recovery actor"), or change an installed
one on that machine:

```bash
docker exec -it quasar-recovery quasar-recovery reconfigure --yes \
  QUASAR_UPDATER_SIGNATURE_MODE=verify \
  QUASAR_UPDATER_TRUSTED_KEYS=quasar-release-2026:<base64 public key>
```

**Go through `verify` first, not straight to `require` — but do not stop there.** In `verify` a
bad signature is refused and a release that publishes none is not, so a fleet can be configured
before the first signed release exists. **`verify` is a migration rung, not a security
boundary**: a request naming no version, or one never published, reads as unsigned and is
applied. Every unverified apply logs a WARN naming the version. Once every release you intend to
apply is signed, reconfigure to `QUASAR_UPDATER_SIGNATURE_MODE=require`. Under `require`, a
developer apply and a revert to a build this instance can no longer name by release are refused
`signature_missing`.

Two limits of the verifying fetch, both failing closed as `signature_invalid`, never as an
unverified apply: **a machine that reaches the release host only through an HTTPS proxy cannot
run `verify` or `require`** (the fetch has no proxy client), and **a manifest mirror behind a
private certificate authority is refused** (the fetch trusts the public web roots only). Such a
machine runs `off`: the pinned digest and the namespace allowlist.

### Rotating the key

Both sides are lists, which makes this a period rather than a flag day.

1. **Generate the new key** (`--key-id quasar-release-2027`) and add its public half to every
   machine's `QUASAR_UPDATER_TRUSTED_KEYS` *alongside* the old one, with `reconfigure`:
   `QUASAR_UPDATER_TRUSTED_KEYS=quasar-release-2026:<old>,quasar-release-2027:<new>`.
2. **Sign the next releases with both keys**: move the *new* key into the primary secret and the
   *old* one into the previous-key pair; the release job signs with both when both are set.

   ```bash
   gh secret   set QUASAR_RELEASE_SIGNING_KEY             --repo <owner/name> < <new key>
   gh variable set QUASAR_RELEASE_SIGNING_KEY_ID          --repo <owner/name> --body 'quasar-release-2027'
   gh secret   set QUASAR_RELEASE_SIGNING_KEY_PREVIOUS    --repo <owner/name> < <old key>
   gh variable set QUASAR_RELEASE_SIGNING_KEY_ID_PREVIOUS --repo <owner/name> --body 'quasar-release-2026'
   ```

   Signing an existing release's manifest by hand does the same thing —
   `sign-platform-release-manifest.sh … --append <the existing .sig>` — followed by
   `gh release upload <tag> platform-release-manifest.v2.json.sig --clobber`.
3. **Drop the old key** from every machine's `QUASAR_UPDATER_TRUSTED_KEYS` once every machine
   carries the new one and every release you might still apply or revert to is signed by it. Then
   delete `QUASAR_RELEASE_SIGNING_KEY_PREVIOUS` and its label variable.
4. **Destroy the old private key.**

If a key is **compromised**, step 3 comes first and immediately, and any release signed only by
it must be re-signed and its `.sig` asset replaced. Until a machine has the new key, its applies
fail closed with `signature_invalid`.

## See also

- [`../CHANGELOG.md`](../CHANGELOG.md): what changed in each released version
- [`docs/configuration.md`](configuration.md): the seed, the recovery actor and every variable
- [`deploy/README.md`](../deploy/README.md): the Compose files, as contributor tooling
- [`deploy/db-backup-restore-drill.sh`](../deploy/db-backup-restore-drill.sh): the backup/restore rehearsal
