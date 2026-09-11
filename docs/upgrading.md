# Upgrading, backing up, and rolling back

This page is for a self-hoster who already has Quasar running and wants to
move to a newer version, or who has hit a problem after an upgrade and wants
to go back. Read it before you run `git pull` or `git checkout` on a live
stack.

The short version: back up Postgres before you upgrade, and never run an
older control-plane binary against a newer database. The rest of this page
explains why and gives the exact commands.

## Which version to move to

Quasar has tagged releases (`v0.1.0` onward), and moving between two tags is
the upgrade this page is written for. A tag is a fixed tree with a
[`CHANGELOG.md`](../CHANGELOG.md) entry describing what changed and a version
number you can quote in a bug report; `develop` is the unstable integration
branch and changes under you. Released versions are listed on the
[releases page](https://github.com/accreleus/quasar/releases).

Read the changelog section for the version you are moving to before you start.
Then follow the steps below, passing the new tag wherever a `<ref>` appears.
The tag is the same argument the quick start uses, so an upgrade is the install
command with a newer version in it.

### Upgrading past v0.1.0: the platform images were renamed

The images are now named for the role they play rather than the technology
inside them: `quasar-control` is now `quasar-control-plane` and `quasar-vulkan`
is now `quasar-node-agent`. `quasar-nv` is unchanged (it is being retired
separately, #545), and the dev/toolchain images — `quasar-dev` →
`quasar-agent-dev`, `quasar-toolchain` → `quasar-gst-toolchain` — are build-time
only and are not deployed.

**No action is required to upgrade.** Both names are published for a transition
window and resolve to the same digests, and a local build writes the old name as
an alias tag alongside the new one. Only pin the new names when you next edit
`deploy/.env`; a `QUASAR_NODE_IMAGE=quasar-vulkan:latest` or a digest-pinned
`QUASAR_CONTROL_IMAGE` under the old package keeps working meanwhile.

**The env var for the agent image has a new name too.** `QUASAR_AGENT_IMAGE` is
now the primary name, matching `QUASAR_CONTROL_IMAGE`; `QUASAR_NODE_IMAGE`
remains honoured as an alias, so nothing breaks, and `QUASAR_AGENT_IMAGE` wins
when both are set.

**`docker-compose.release.yml` is retired.** `deploy/docker-compose.yml` is now
itself the production deployment, so pinning a release is setting those two
image vars in `deploy/.env` and nothing more. If your compose chain names the
release overlay, drop that `-f` — the pins move into `deploy/.env`:

```bash
# before
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml \
               -f deploy/docker-compose.release.yml up -d
# after — QUASAR_CONTROL_IMAGE / QUASAR_AGENT_IMAGE now live in deploy/.env
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml up -d
```

The overlay also defaulted `QUASAR_AUDIO_REQUIRED=1`; set it in `deploy/.env` to
keep that behaviour.

**Switching an existing source-built stack onto pinned images needs the TLS
volume recreated.** The source image runs as root and the published image runs
as the non-root `quasar` user, so a `quasar-control-tls` volume that a
source-built stack already populated is owned by root, and the published image
cannot write to it. The control plane then exits at boot, repeatedly:

```
fatal: tls: write TLS key "/var/lib/quasar-control/tls/key.pem" (mount a writable volume): open ...: permission denied
```

A **fresh** install is unaffected — `Dockerfile.control.prod` owns the mount
point, and Docker copies that ownership onto a new named volume. It is only the
switch that bites, because Docker never re-initializes a volume that already has
content. Recreate it:

```bash
docker compose -f deploy/docker-compose.yml down
docker volume rm deploy_quasar-control-tls
docker compose -f deploy/docker-compose.yml up -d
```

That re-issues the self-signed certificate with a **new fingerprint**, so every
client that trusted the old one has to trust the new one again. (Verified on the
gpu-test host, 2026-08-27: the failure above, then a clean boot after removing
the volume.) Building from source instead? `deploy/redeploy.sh` handles
this for you — it adds `deploy/overlays/docker-compose.dev.yml`, which carries
the build keys and the SPA bind mount the base file no longer has. The old
names are dropped one release after the release that introduces the new ones.

---

## Back up Postgres before you upgrade

Every Quasar upgrade that touches the control plane can bring a database
migration with it. A migration changes the schema in place; there is no
built-in "undo" for it once it has run. If an upgrade goes wrong, the fastest
safe way back is restoring a backup taken before you started, not trying to
hand-edit the schema.

Quasar ships a script that proves the backup/restore path actually works,
`deploy/db-backup-restore-drill.sh`. It does not touch your production
volume: it stands up a disposable Postgres and control-plane binary in a
throwaway Docker Compose project, seeds real rows across every application
table (users, apps, hosts, GPUs, sessions, admin activity, entitlements),
takes a `pg_dump --format=custom` backup, restores it into a second fresh
database, and asserts that the schema, the migration version, and every
seeded row all match after restore. It is a rehearsal, not the button you
press on your own stack, but it demonstrates the two commands your own
backup and restore should use: `pg_dump` and `pg_restore`.

To back up your own running stack before an upgrade, dump the
`quasar-postgres` service's database the same way the rehearsal script does:

```bash
docker compose -f deploy/docker-compose.yml exec -T quasar-postgres \
  pg_dump --format=custom --no-owner --no-privileges -U quasar quasar \
  > quasar-backup-$(date +%Y%m%d%H%M%S).dump
```

Check the username and database name against your `deploy/.env` if you
changed the defaults. Keep the resulting `.dump` file somewhere off the host
(it is a full copy of your account, app catalog, and session history).

To restore it later, into a stopped stack with a running `quasar-postgres`
container:

```bash
docker compose -f deploy/docker-compose.yml exec -T quasar-postgres \
  pg_restore --exit-on-error --clean --if-exists --no-owner --no-privileges \
  -U quasar -d quasar < quasar-backup-20260101120000.dump
```

If you want to prove this works on your own hardware before you rely on it,
run the rehearsal script itself:

```bash
bash deploy/db-backup-restore-drill.sh
```

It builds and tears down its own disposable Postgres and Compose project, so
it is safe to run any time and leaves your real stack untouched.

---

## A normal upgrade, start to finish

1. Back up Postgres (previous section).
2. Fetch the new code. `redeploy.sh` in step 3 fetches and checks out the ref
   itself, so this step only matters if you want to read the changelog or a
   diff first:
   ```bash
   git fetch origin --tags
   git submodule update --init protocol
   ```
3. Redeploy. On a host already set up with `deploy/redeploy.sh`, this is a
   single command that rebuilds every component from the ref you name,
   brings the stack back up, and waits for it to report healthy:
   ```bash
   deploy/redeploy.sh <va|nvidia> <ref>
   ```
   Use `va` for an AMD/Intel host, `nvidia` for an NVIDIA host, and pass the
   ref you want. For a normal upgrade that is the release tag you are moving
   to (`v0.1.1`, say); a branch or a commit works too, for deliberate branch
   testing. The control-plane container will not report healthy
   until any pending migration has finished running, so a script that waits
   for health is also waiting for the migration.
4. If only the control-plane (Go) code changed, and not the node-agent or
   web SPA, the narrow `control` scope is faster (about a minute instead of
   the full multi-service rebuild):
   ```bash
   deploy/redeploy.sh <va|nvidia> <ref> control
   ```
   or, if you manage the host through this repo's `make` targets:
   ```bash
   make redeploy-cp HOST=<host>
   ```
5. Confirm the stack is healthy and the version you expect is running before
   you consider the upgrade done.

---

## Upgrading a registry install

The section above rebuilds a host from source. A **registry install** — one
running published images, pinned by digest in `deploy/.env` (`deploy/README.md`
install path A) — upgrades by re-pinning those two digests and recreating the
two containers they name. There is no build and no `git` step: the images
already exist.

Take the digests from the release's `platform-release-manifest.json` asset,
which lists exactly two components, control-plane then node-agent. The admin
Releases page shows the same commands filled in for the release it is offering.

```bash
# 1. Pin the release's digests — the two lines in deploy/.env.
QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@sha256:<control-plane digest>
QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@sha256:<node-agent digest>

# 2. Pull the pinned images and recreate only those two services.
docker compose -f deploy/docker-compose.yml pull quasar-control-plane quasar-node-agent
docker compose -f deploy/docker-compose.yml up -d --force-recreate --no-deps quasar-control-plane quasar-node-agent
```

Notes that matter:

- **Repeat every `-f` you deploy with.** An NVIDIA host that came up with
  `-f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml` must pass
  both files here too; a compose invocation that drops an overlay recreates the
  containers without it.
- **Recreating the node agent kills every session on that host.** Drain it
  first if that matters.
- **Recreating the control plane runs migrations**, so the one-way rule below
  applies from that moment: the digest you just replaced is no longer a safe
  thing to pin back if the new one migrated the database.
- Nothing else is touched: `--no-deps` leaves Postgres, the updater and the
  session containers alone.

An install with the updater beside it (see "The updater") does this for you
from the admin UI once the apply half ships; the commands above are what it
runs, and stay the manual path for a host without one.

---

## The one-way migration rule

Quasar's control-plane binary runs its database migrations automatically on
boot (`golang-migrate`). That is what makes step 3 above a single command.
It also means migrations only ever move forward: a binary applies every
migration up to the newest one it was built with, and it has no code path
that removes a migration to match an older binary.

This creates one hard rule: **never run a control-plane binary that is older
than the migration version already applied to its database.**

A migration is a one-way door once its "up" step has run against your
database. Rolling the *binary* back with `git checkout <older-ref>` does not
roll the *database* back. If you do this, on the next boot the binary looks
at the database, sees a migration version it does not recognize, and cannot
proceed. The database is not damaged. The binary is asking for a migration
it was never built with.

This is easy to hit by accident: you upgrade, hit an unrelated problem, and
reach for the obvious `git checkout <previous commit>` to "go back." If that
previous commit predates a migration that already ran, the control-plane
will not start.

### What the failure looks like, and what it means now

Older versions of Quasar printed the raw error from the migration library
here, which named a version number but not a cause or a fix. As of this
change, the control-plane instead reports the cause and the fix directly:
it explains that the running binary is older than the database schema, most
likely from a rollback to an older commit or release, and names the
migration version at fault. It also repeats the fix from the next section
in the error text itself, so a self-hoster reading their own logs does not
need to already know this page exists.

### The fix: redeploy the version that has the migration

The database is fine. Bring back a binary that has the migration it is
looking for. In most cases that means redeploying the newer ref or commit
you were previously running, using the exact same commands as a normal
upgrade:

```bash
deploy/redeploy.sh <va|nvidia> <newer-ref>
```

or, if the change that introduced the migration touched only the
control-plane:

```bash
make redeploy-cp HOST=<host>
```

Do not try to "fix" this by rolling the database back yourself
(`schema_migrations` hand edits, restoring an old backup over a newer
database, or running a migration's `down` step by hand). Restoring your
pre-upgrade backup is only the right move if you have decided you actually
want to abandon the upgrade and everything that happened after it; if you
only want the control-plane running again, redeploying forward is faster
and loses nothing.

This is exactly why the backup step at the top of this page matters: if
going back to a known-good binary is not an option (for example, you need
to stay on the newer code but the migration itself is the problem), your
pre-upgrade backup is the way to get a clean, working database again on
the older version.

---

## One-time: the control-plane TLS volume's ownership

**Only if your stack was built from source before this change, and only once.**

Both control-plane images now run as uid 1000. The source-built image used to run
as root, so on a stack that ever ran it the `quasar-control-tls` volume — which
holds the TLS pair and the artwork cache — is owned by root. Applying a published
release to such a stack starts a uid-1000 container that cannot write it, and the
control plane crash-loops:

```
fatal: tls: write TLS key /var/lib/quasar-control/tls/key.pem: permission denied
```

Nothing is damaged and nothing is lost. Give the volume to uid 1000:

```bash
# The volume name is <compose project>_quasar-control-tls. The project is the
# stack directory's name unless COMPOSE_PROJECT_NAME says otherwise, so this
# lists the real one rather than assuming it:
docker volume ls --format '{{.Name}}' | grep quasar-control-tls

docker run --rm -v <that name>:/t alpine chown -R 1000:1000 /t
docker compose -f deploy/docker-compose.yml up -d --no-deps quasar-control-plane
```

`deploy/redeploy.sh` does this for you on every deploy that touches the control
plane, so a source install that deploys through it needs nothing here. The
enrolled-host stack (`deploy/enroll-host.sh`) has no control-plane volume at all
— it runs only the agent and the updater — so nothing there needs it either.

---

## The updater

`quasar-updater` is the per-host actor that applies a platform release: it pulls
the pinned digests and recreates the containers they replace, because a
container cannot recreate itself. Nothing on this page requires it — a manual
upgrade is still the two `.env` vars plus `docker compose up -d` — but the
admin-facing "apply this release to this host" path goes through it.

### Adding it to an existing install

One time, on each host:

```bash
# 1. Get the compose file that declares the service.
git -C /path/to/quasar pull            # source install
#   ...or re-download deploy/docker-compose.yml for a registry install.

# 2. Tell it where the stack lives. This must be the stack directory's
#    absolute HOST path: the updater rebuilds its compose invocation from its
#    own container labels, and those record host paths.
echo "QUASAR_STACK_DIR=$(cd deploy && pwd)" >> deploy/.env
#    A registry install also names the image (a tag, see below):
echo "QUASAR_UPDATER_IMAGE=ghcr.io/accreleus/quasar/quasar-updater:latest" >> deploy/.env

# 3. Bring it up, AND recreate the two containers that talk to it. The compose
#    file mounts the updater's socket volume into the control plane and the
#    node agent; a container created before the volume existed does not have
#    that mount until it is recreated, and until then the console reports the
#    updater as not installed for that target even though it is running.
docker compose -f deploy/docker-compose.yml up -d quasar-updater quasar-control-plane quasar-node-agent

# 4. Verify it discovered the stack it is sitting beside.
docker compose -f deploy/docker-compose.yml exec quasar-node-agent \
  curl -s --unix-socket /run/quasar-updater/updater.sock http://u/v1/self
```

That last command should print the compose project, the working directory, the
`-f` files (**including every overlay you use**) and the namespace allowlist.
The console checks the same things for you: Admin › Fleet › Releases shows each
target's pre-update checks, and a control plane or agent created before the
socket volume existed reads as **Blocked** with the recreate command beside
it, rather than as "no updater". If the command
instead reports that the stack directory is not visible in the container,
`QUASAR_STACK_DIR` is wrong or unset — the updater fails closed rather than
guessing at a compose invocation and recreating the wrong project's containers.

`deploy/redeploy.sh` seeds `QUASAR_STACK_DIR` for you, so a source install that
deploys through it only needs step 1 and step 3.

### The agent's health port

Since #152 the node agent **refuses to start** when it cannot bind its health
address, instead of letting whatever already owns the port answer its health
checks. The agent runs with host networking, so the default `127.0.0.1:9091` is
shared with everything on the machine. The host's readiness card (Hosts tab ›
Updates › "agent health port free") and the Releases page's pre-update checks
both report who answers that address, so a squatter shows up before an update
rather than as a host that is down after one; an update that does hit it is
put back on the previous agent by the updater (ADR 0004), with the
`health-bind-failed` line in the failure. To move the agent off a busy port,
set `QUASAR_HEALTH_ADDR=127.0.0.1:9191` (any free loopback port; the image's
`HEALTHCHECK` follows it) or `QUASAR_HEALTH_ADDR=` (empty disables the
endpoint) in `deploy/.env`, then `docker compose up -d quasar-node-agent`.

### Updating the updater itself

**The updater is not part of a platform release.** It is what applies one, so it
is not one of the images an apply moves by digest, and it is not in the release
manifest. Its image is therefore named by a tag, and it updates by hand:

```bash
docker compose -f deploy/docker-compose.yml pull quasar-updater
docker compose -f deploy/docker-compose.yml up -d --no-deps quasar-updater
```

Safe at any time: it holds no state beyond the result files in its volume, and
an apply in flight is a detached `docker compose` invocation that finishes
regardless.

### What an apply does, and what it costs

Recreating the **node agent kills every session on that host.** The
`quasar-sess-*` / `quasar-pulse-*` sibling containers survive the recreate and
are then swept by the new agent's startup orphan sweep, so nothing is orphaned
and the apply is safe to retry — but the sessions are gone. Draining the host
first is the control plane's job.

Recreating the **control plane** runs the one-way migrations above. If the new
container never starts, the updater restores `.env` from `.env.prev` and brings
the previous digest back itself — never having started, it cannot have migrated
anything. If it starts and then fails, the updater leaves it failed and records
the previous digests in the result, because a started container may already have
migrated and the rule at the top of this section then applies.

### Release channels: stable, beta, edge

Which releases the console offers is one setting, Admin › Fleet › Releases ›
**Channel**. It changes what is *listed*; it never installs anything and never
starts a check.

- **`stable`** (the default) — tagged releases with notes. **Prereleases are
  hidden**, which is the point of the channel: `v0.3.0-rc.1` is published and
  installable by hand, but stable will not offer it.
- **`beta`** — the same tagged releases *and* the prereleases among them. This is
  how you follow release candidates from the console: you get the rc as soon as
  it is published, with its notes and its pinned digests, applied through exactly
  the same path a stable release takes. Nothing else about an apply changes.
- **`edge`** — whatever was last published from `release_edge_branch` (default
  `develop`). No version, no notes, a compare link instead.

Beta stores nothing of its own: a prerelease is already detected and cached
alongside the stable releases, and beta is the channel that lists it. So
switching to or from beta re-detects nothing and writes nothing — the next read
simply selects a different set.

**What beta costs you.** A prerelease is a build that has not been through a
release cut. It can carry a migration that the next prerelease revises, and a
migration is one-way (see "The one-way migration rule" above). Run beta on a
host you can afford to have ahead of stable, and back Postgres up before an
apply, exactly as you would for any upgrade.

**Ordering is by version, not by publication date.** Release candidates are cut
from the development branch while patches are cut from the release branch, so a
`0.3.0-rc.1` can be *built before* the `0.2.5` that is *below* it. Beta orders
its list by SemVer precedence — `0.2.0-rc.1 < 0.2.0-rc.2 < 0.2.0 < 0.2.1-rc.1`,
and `0.3.0-rc.9 < 0.3.0-rc.10` — so the newest thing on the list is the highest
version, never merely the most recently built.

#### Switching back to stable: you wait, you are never rolled back

This is the one rule to read before turning beta on.

Suppose the instance is running `0.3.0-rc.1` and you switch the channel back to
`stable`. Stable hides prereleases, so the newest release it can see may be
`0.2.5` — *older than what you are running*. Quasar does not offer it. **No
channel offers a build that orders below the one installed.**

What you see instead:

- **`0.3.0` has shipped** → it is listed, and Update Quasar moves you onto it
  normally. The switch is complete.
- **`0.3.0` has not shipped yet** → the Releases list is empty. In place of the
  list you get *"Nothing newer than this control plane has been detected on the
  stable channel."*, and each target on the Fleet update card reads *"Nothing
  newer has been detected on this channel."* — the same fact said once for the
  list and once per target. The instance stays on `0.3.0-rc.1` until stable
  passes it. This is not a failure state and needs no action; switching back to
  beta immediately restores the full list.

The reason is the one-way migration rule. `0.3.0-rc.1` may have applied a
migration that `0.2.5` does not embed, and a control plane booted below the
database's applied version **crash-loops** with no console left to fix it from.
Rather than checking that per release and sometimes offering a downgrade, the
console offers none: a downgrade is unrepresentable here, not merely
discouraged.

If you genuinely need to go backwards, that is a manual redeploy, and it is
subject to the same rule — run the down migrations and reset
`schema_migrations` first, or you will land in the crash-loop described above.

### Applying from the console

Admin › Fleet › Releases lists every target and, for an eligible host, offers
**Apply**. What that does, in order: the host is cordoned through the same drain
as `POST /v1/hosts/{id}/drain`, the attempt sits in **waiting_sessions** showing
how many sessions are still running, and only when the count reaches zero is the
apply handed to that host's updater. The cordon is then restored to whatever it
was before — a host an admin had already cordoned stays cordoned, and one that
was serving goes back to serving, whether the apply succeeded or failed.

The confirmation's **force** checkbox skips the wait and names the number of
live sessions it ends, because recreating the agent kills them all either way
(above); with force off, nothing is lost. Force never stops sessions itself —
the recreate does.

Success is the host registering again on the release's commit, which is why a
successful apply is reported by the *new* agent and not by the one that carried
it out. A failed apply records the previous digests and shows the reason. If
the new agent container never came up — it exited, or never became healthy —
the updater puts the previous digest back itself and the history shows
"Reverted automatically" beside the failed apply, with the failed container's
last log lines in the failure (ADR 0004). A pull that failed leaves the old
agent running. The Apply history section below the targets is the durable
record, and `GET /v1/admin/platform/attempts` is the same data.

Every result carries the previous digests, so the manual restore is copy-paste:

```bash
docker compose -f deploy/docker-compose.yml exec quasar-node-agent \
  curl -s --unix-socket /run/quasar-updater/updater.sock \
  http://u/v1/results/<request-id>
```

### Update Quasar from the console

Admin › Fleet › Releases offers **Update Quasar** when a newer release is
listed and nothing else is in flight. It moves the whole instance in one
action, in one order that is not configurable: **the control plane first, then
every eligible host, one at a time.** An agent is never moved past the control
plane (ADR 0002), which is why the order exists and why there is no "hosts
only" button.

What happens, step by step:

1. **The whole fleet is cordoned, and drains first only if the release changes
   the database.** Every host goes out of scheduling for the run — each one is
   going to be recreated, so a session started mid-run is one the run would end
   at that host's step. The cordons are released when the run finishes, and a
   host an admin had already cordoned stays cordoned.

   Whether the run also *waits* for those sessions to end depends on the
   release:

   - **A release that runs no migration does not wait.** Live sessions stream
     straight through the control-plane restart: the agent holds its running
     sessions across the outage and the browser keeps the media path it already
     has, so the control plane's absence costs the stream nothing. Measured on a
     real session: 1080p60 decoding at 60 fps throughout a 73-second
     control-plane outage, still `running` afterwards.
   - **A release that carries a migration waits for the instance to empty.** The
     run cordons every host and sits in **waiting_sessions**, showing the
     instance-wide count, until it reaches zero; force skips the wait and ends
     them. The reason is not the restart — it is that the held session's row is
     read back by a binary that has just migrated the database under it. Every
     migration Quasar has ever shipped was written on the assumption that no
     session was live while it ran, and one of them (0027) moved the signalling
     token out of the `sessions` table entirely. Nothing checks a migration for
     whether a live session survives it, so the update does not gamble on it.

   You can tell the two apart before you press Update: the confirmation says
   either that live sessions keep streaming, or that this release changes the
   database and the update waits.
2. **The control plane updates itself** through the updater sitting beside it,
   over that host's local socket — never through a node agent. It pulls, then
   recreates its own container, so **the API and the console go away for
   roughly twenty seconds.**
3. **The new build reports the outcome.** The control plane cannot report its
   own success — carrying the update out kills the process holding the request
   — so the request id is written down before the socket call and the binary
   that boots reads it back. Its own liveness on the release's commit is the
   evidence; the run and its cancel flag are in Postgres, so the run resumes
   on the new build.
4. **Each host follows, in the fleet list's order.** Each is cordoned, drained,
   updated and uncordoned exactly as a per-host Apply is (above).
5. **The run stops at the first target that fails**, and says which. Targets
   behind it are already updated; targets ahead of it were never started. A host
   whose new agent did not come up is put back on its previous digest by its
   updater before the run stops there (ADR 0004).
6. **A run that passed a host over ends "succeeded_partial", not "succeeded".**
   The banner says which hosts were skipped and why, and **Retry skipped hosts**
   starts a plain fleet apply of the same release once the cause is fixed —
   the updated targets are already on it and are skipped, so only the hosts
   left behind move. An automatic update picks them up on its next pass by
   itself.

**Force** applies to every target in the run, the control plane included, and
the confirmation names how many hosts it will take sessions from. Without it
the run waits for each host's own sessions in turn — and, on a release that
carries a migration, for the whole instance to empty before the control-plane
step as well — which means a run can sit for as long as someone is playing.

**A source-built control plane is not offered an update from here.** The
registry image is a different build with a different uid, and swapping one for
the other leaves a control plane that starts and then cannot write its own
state — a crash-loop with no console left to fix it from. The control plane
learns its own install mode from the updater beside it; a source install, or
one it cannot determine, makes the target ineligible and refuses the run
outright, since nothing moves before the control plane.

**Hosts that cannot take the release are skipped, not failed** — an offline
host, a source-built host, one with no updater, one whose pre-update checks
fail. The run lists them under "Not updated" with the reason and finishes
`succeeded_partial` (step 6). "Nothing was eligible" is a legitimate outcome,
not an error; a fleet where every host is already on the release is a plain
`succeeded`.

**Every target is checked before Update is offered.** Beside each target the
Releases page shows its pre-update checks: the updater is reachable (and the
console tells "socket volume not mounted — recreate the container" apart from
"updater not running"), the updater sees the stack directory, the container was
started with the same compose files the updater will recreate it with, the
release's images resolve at the registry, and — for a host — its agent's health
port is answered by that agent. A failing check makes the target **Blocked**
with the fix named; Update is refused while the control plane is blocked, and a
blocked host is skipped and named. A check that could not be evaluated (an agent
that predates the checks) warns and never blocks. A host's own checks are on
the Hosts tab under **Updates**.

**Cancel stops the run before its next target and never interrupts one in
flight.** A pull or a recreate that has already started finishes; interrupting
a recreate is how a stack is left with no container at all. The flag is
persisted, so a cancel pressed while the control plane is restarting is
honoured by the build that boots.

**An admin tab left open across the control-plane step** shows "Quasar was
updated" with a Reload button once the served build differs from the one the
page was loaded from. The page keeps polling through the restart and says the
control plane is restarting rather than showing an error.

### Reverting an agent

A host row also offers **Revert** once that host has one succeeded update behind
it. A revert is an apply with an older digest set. Same drain, same message,
same states, aimed at the digests recorded as `previous_digests` on that host's
last succeeded attempt. It is not a version picker. The only thing on offer is
the build the host was demonstrably running a moment ago, and the confirmation
names that digest.

**The control plane is never revertible from the console, at any depth.** It
carries migrations, and rolling it below the database's applied version is the
crash-loop described under the one-way migration rule above. Agents carry no
migrations, so they can move back, but never above the control plane's own
release: a revert whose target orders above it is refused with
`host_not_eligible` / `release_above_control_plane`. That is only reachable if
the control plane was moved backwards by hand.

If this instance can no longer name the build being restored, because the digest
predates its release records, the revert still runs. That digest ran on this
host under this control plane or an older one, so it cannot be above it. What
changes is the evidence. With a known release, the revert succeeds when the host
registers on that release's commit. Without one, the updater's own `succeeded`
result resolves it, as does a register reporting any commit other than the one
it was reverted from. A revert is itself recorded as an attempt
(`kind: revert`), so it can be reverted in turn.

---

## Installing updates automatically

**Off by default.** Settings ▸ Platform updates ▸ *Install updates automatically*
(`platform_auto_apply`). With it on, Quasar applies a detected release without waiting for
you — the control plane first, then every eligible host, through exactly the fleet run the
**Update Quasar** button starts. It is a trigger on that machinery, not a second path.

**A release that changes the database is never installed this way.** That is the one case
where the control-plane step still empties the whole instance before it runs (see "What an
apply does, and what it costs"), and ending every live session with nobody watching is not
something to do on a schedule. Such a release is still detected, still listed, still
banners; it waits for you to press the button. Everything else rides through the
control-plane step with sessions still streaming, which is what makes automatic updates
tolerable at all — a host's own sessions still end when that host is updated, as always.

**There is no separate schedule to configure.** An automatic update happens when release
detection next runs, so *that* job's schedule is the window: Jobs ▸ **Platform release
detection**. Move the job and you move the update hour; run it now ("Check now") and an
eligible release is applied now. One schedule, already yours, that cannot disagree with
itself.

**An automatic update is never forced.** "Update now" — the checkbox that ends live
sessions — is an operator agreeing to lose them, and there is no operator here. An
automatic run always waits.

**A failure stops that release, not the feature.** If an automatic run fails on a host, the
run stops there and restores its cordons exactly as a manual one does, and Quasar will not
retry *that release* automatically. A newer release is still installed, and applying the
failed one yourself clears the block — the rule is that the **most recent** run on a release
decides, so any run you start yourself, whatever its outcome, resets it. One flaky host does
not end automatic updates for the instance.

**A run you cancel is not a failure**, so the same release is tried again on the next pass.
Cancelling says "not now", not "never"; if you want it left alone, turn the setting off.

**An automatic run is refused outright if the release would change the database**, even
though such a release is never chosen in the first place. The check happens twice on purpose:
once when the pass picks a release, and again at the control-plane step, because that step
re-reads the release and a row it cannot read is treated as one that migrates. A refusal
shows as a failed run whose message says so, and nothing is drained.

**Where to read what happened.** Jobs ▸ Platform release detection ▸ its latest run. The
summary carries `auto_apply` — `started` with the run id, or why not: `carries_migration`,
`no_release`, `not_eligible` (with the reason), `in_flight`, `failed_before`. A run started
this way is marked in Fleet ▸ Releases, so a fleet update you did not start explains itself.

## Release notifications

The Releases page shows a banner when an update appears. That only helps
someone who is looking at it. **Fleet ▸ Releases ▸ Notifications** takes a
webhook URL and POSTs one message when the detector finds a release this
instance could move to.

A webhook rather than email on purpose: email would need SMTP credentials, a
sender identity and somewhere for bounces to go before it delivered anything,
while a URL and a POST already work with Slack, Discord, ntfy, or a script
behind a reverse proxy.

### Wiring one up

1. Get an incoming-webhook URL from wherever you want the message. Slack:
   *Incoming Webhooks* → *Add New Webhook to Workspace*. Discord: channel
   *Settings* → *Integrations* → *Webhooks* → *New Webhook* → *Copy Webhook
   URL*. ntfy: `https://ntfy.sh/<your-topic>`.
2. Paste it into **Webhook URL** and press **Save URL**.
3. Press **Send test**. A test goes out whether or not notifications are
   switched on, and it records nothing — it can never use up the one
   notification a real release gets.
4. Press **Turn notifications on**.

**The URL is a credential.** On Slack, Discord and ntfy, knowing the URL is
what authorizes posting to that channel. Quasar treats it as one: it never
appears in a log line, in the audit record of the setting change, or in a
delivery error. Treat it the same way — it is admin-readable in the console
because an admin has to be able to see what they configured.

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

`text` and `content` are the same sentence under the two field names Slack and
Discord read, which is why those two render this body with no adapter in
between. Everything structured sits beside them for a receiver that wants it.
`notes_excerpt` is bounded — the full notes are on the Releases page.

### Signing (optional)

Slack, Discord and ntfy authenticate by URL and need nothing more. A receiver
you wrote yourself can ask for proof: store a secret under **Secrets → Release
notification signing secret** (or set
`QUASAR_PLATFORM_RELEASE_WEBHOOK_SECRET`). Every delivery then carries

```
X-Quasar-Timestamp:     1757295611
X-Quasar-Signature-256: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<raw body>")>
X-Quasar-Delivery:      <one per notification, repeated across its retries — dedupe on it>
```

Verify by recomputing over the **raw** body you received and comparing in
constant time. The timestamp is inside the signed material, so a captured
delivery cannot be replayed under a new one.

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

- **The same release is announced once.** Detection runs weekly and whenever
  someone presses *Check now*; the record of what has been announced is keyed
  on the release, so re-detection is silent.
- **A fresh install does not announce its back catalogue.** The trigger is the
  same "an update is available" the banner uses, so at most one message goes
  out, about the release you could actually move to.
- **A failure retries, then stops.** A refused delivery is retried a few times
  within the pass, then on each following detection pass, up to five passes.
  After that the release is left un-announced rather than retried forever. The
  Notifications card shows the last attempt and its error; the detection job's
  run summary in **Fleet ▸ Jobs** carries `notify`, `notify_reason` and
  `notify_status_code` for every pass.
- **A failing webhook never fails detection.** The banner, the release list and
  the apply button are unaffected by anything the receiver does.
- **`https` only, and public addresses only.** Delivery refuses plain `http`, a
  URL carrying credentials, and any host that resolves to a loopback, private
  or link-local address — the same containment the image-digest resolver uses,
  so this cannot become a probe of your own network. **A receiver on the LAN or
  on localhost is therefore not reachable**; put a public https endpoint (a
  tunnel, a reverse proxy) in front of it. `QUASAR_PLATFORM_WEBHOOK_HOSTS`
  narrows the destination further if you want it pinned
  (`docs/configuration.md`).
- Clearing the URL switches notifications off in the same save. There is no
  state where notifications are on with nowhere to send.

---

## Cutting a release

This is for a maintainer publishing a new Quasar version, not for a
self-hoster upgrading one — see "Which version to move to" above for that.

`make release VERSION=x.y.z` (`scripts/release/release-cut.sh`) is the one
command: on a clean `main` that matches `origin/main`, it moves
`CHANGELOG.md`'s `## Unreleased` section into a dated `## X.Y.Z — YYYY-MM-DD`
section directly above the old one, leaving a fresh empty `## Unreleased` in
its place, commits that (`chore(release): x.y.z`), tags the commit `vX.Y.Z`
(annotated), and pushes both. Pushing the tag is what triggers the tag-push
release lane (`.github/workflows/images.yml`, #108): it builds and validates
the images, then publishes them, a GitHub Release whose body is that
version's changelog section, and a `platform-release-manifest.json` asset.
Publication waits for the separately versioned updater image too, because the
release notes link its tag. The manifest itself contains control-plane and
node-agent only. Publish the public documentation from the released tree after
this workflow succeeds (`pages.yml` is manually dispatched).

It refuses — with a one-line reason, before touching anything — unless:

- the repo is on `main`, with a clean working tree that matches `origin/main`
- `VERSION` is strict semver (`X.Y.Z`, an optional `-prerelease` part is
  allowed for a release candidate; no leading `v`, no build metadata) and
  strictly newer than the newest existing `v*` tag
- the `## Unreleased` section is non-empty

It never merges `develop` into `main` for you — that merge, and the operator
sign-off it requires (`CLAUDE.md`, "Git branching & environments"), happens
first, by hand. Add `DRY_RUN=1` to see the changelog diff and the exact git
commands it would run without executing any of them:

```bash
make release VERSION=0.2.0 DRY_RUN=1   # preview
make release VERSION=0.2.0             # cut, commit, tag and push v0.2.0
```

A prerelease tag (`v0.2.0-rc.1`) runs the same workflow and publishes a GitHub
prerelease instead of a stable release — useful for exercising the publish
lane before cutting the real version.

---

## Signing platform releases

Optional, and off on both sides until someone turns it on. A release may publish
a detached signature over its manifest, and a host may be configured to verify
it before applying anything. Neither half changes what a release *is*: what gets
installed is still the pinned digest (ADR 0001). The signature answers a
different question — whether the digest set came from whoever holds the release
key. Format: `scripts/release/platform-release-signature.md`. Decision record:
`docs/adr/0003-release-signatures.md`.

**What is signed is the manifest.** It names every component digest, so the
signature covers the images through them. There is no per-image signature.

### Turning it on: the publishing half

Done once, by the maintainer who publishes releases.

1. **Generate a key pair, off CI, on a machine you trust.** Not in the repo —
   the script refuses to write inside the working tree.

   ```bash
   scripts/release/new-release-signing-key.sh \
     --out ~/.config/quasar/release-signing-2026.pem \
     --key-id quasar-release-2026
   ```

   It prints the public key as `quasar-release-2026:<base64>` and the exact
   commands for step 2. Back the private key up somewhere you could restore a
   release from; there is no recovery from losing it, only a rotation.

2. **Create the CI secret and the label variable.** The *secret* holds the
   private key, the *variable* holds its label:

   ```bash
   gh secret   set QUASAR_RELEASE_SIGNING_KEY    --repo <owner/name> < ~/.config/quasar/release-signing-2026.pem
   gh variable set QUASAR_RELEASE_SIGNING_KEY_ID --repo <owner/name> --body 'quasar-release-2026'
   ```

   Both are required together: the release job fails loudly if the key is set
   and the label is not, rather than publishing a signature nobody can name.

3. **Cut a release as usual.** The `release` job signs the manifest right after
   it validates it, verifies its own signature with the public half of the key
   before uploading anything, and attaches
   `platform-release-manifest.json.sig` beside the manifest. With no secret
   configured the step prints one line and does nothing.

4. **Check the release.** The asset should be there, and:

   ```bash
   gh release download vX.Y.Z --pattern 'platform-release-manifest.json*'
   scripts/release/verify-platform-release-manifest.sh \
     --manifest  platform-release-manifest.json \
     --signature platform-release-manifest.json.sig \
     --public-key quasar-release-2026:<base64>
   ```

### Turning it on: the verifying half

Per host, in that stack's `deploy/.env`, then
`docker compose up -d quasar-updater`.

```bash
QUASAR_UPDATER_SIGNATURE_MODE=verify
QUASAR_UPDATER_TRUSTED_KEYS=quasar-release-2026:<base64 public key>
```

A stack whose `docker-compose.yml` predates this feature does not pass those
variables through, so the updater would come back up in `off` and say so in its
log. Take the current `quasar-updater` service block from
`deploy/docker-compose.yml`, or add the four `QUASAR_UPDATER_SIGNATURE_MODE` /
`_TRUSTED_KEYS` / `_MANIFEST_BASE_URL` / `_MANIFEST_TIMEOUT_S` lines to its
`environment:`.

**Go through `verify` first, not straight to `require` — but do not stop there.**
In `verify` a bad signature is refused and a release that publishes none is not,
so a fleet can be configured before the first signed release exists, and again
after it, with nothing breaking in between.

Be clear about what that costs while you sit in it. **`verify` is a migration
rung, not a security boundary.** The apply request chooses which version's
signature the updater looks for, so a request naming no version, or one that was
never published, reads as "unsigned" and is applied — no network request, no
refusal. Anything able to drive an apply can therefore walk straight past
`verify`. It catches a *signed* release that has been tampered with in transit,
and nothing else. Every unverified apply logs a WARN naming the version, so
`docker logs quasar-updater | grep UNVERIFIED` tells you whether a host is still
relying on that leniency. Once every release you intend to apply is signed:

```bash
QUASAR_UPDATER_SIGNATURE_MODE=require
```

Confirm what a host is actually doing:

```bash
curl --unix-socket /run/quasar-updater/updater.sock http://u/v1/self | jq '{signature_mode, trusted_key_ids, manifest_source}'
```

The full mode/outcome matrix, and the two things `require` refuses that `verify`
does not, are in `docs/configuration.md` "Release signature verification". The
important one: **under `require`, a revert to a build this instance can no
longer name by release is refused**, because there is no published manifest to
have signed it. Drop that host to `verify` for the revert, or use the manual
recipe.

### Rotating the key

Both sides are lists, which is what makes this a period rather than a flag day.
Never a same-day swap.

1. **Generate the new key** (`--key-id quasar-release-2027`) and add its public
   half to `QUASAR_UPDATER_TRUSTED_KEYS` on every host, *alongside* the old one:

   ```bash
   QUASAR_UPDATER_TRUSTED_KEYS=quasar-release-2026:<old>,quasar-release-2027:<new>
   ```

   Recreate each `quasar-updater` and confirm both labels in `/v1/self`. Nothing
   has changed about which releases verify; the fleet has simply widened.

2. **Sign the next releases with both keys.** Move the *new* key into the
   primary secret and the *old* one into the previous-key pair; the release job
   signs with both when both are set, and the asset carries one entry per key:

   ```bash
   gh secret   set QUASAR_RELEASE_SIGNING_KEY             --repo <owner/name> < <new key>
   gh variable set QUASAR_RELEASE_SIGNING_KEY_ID          --repo <owner/name> --body 'quasar-release-2027'
   gh secret   set QUASAR_RELEASE_SIGNING_KEY_PREVIOUS    --repo <owner/name> < <old key>
   gh variable set QUASAR_RELEASE_SIGNING_KEY_ID_PREVIOUS --repo <owner/name> --body 'quasar-release-2026'
   ```

   A dual-signed release verifies on a host that trusts either key, so a host
   that has not been updated yet is not stranded. Signing an existing release's
   manifest by hand does the same thing —
   `sign-platform-release-manifest.sh … --append <the existing .sig>` — followed
   by `gh release upload <tag> platform-release-manifest.json.sig --clobber`.

3. **Drop the old key** from `QUASAR_UPDATER_TRUSTED_KEYS` on every host, once
   every host carries the new one and every release you might still want to
   apply or revert to is signed by it. Then delete
   `QUASAR_RELEASE_SIGNING_KEY_PREVIOUS` and its label variable.

4. **Destroy the old private key.**

If a key is **compromised** rather than rotated on schedule, step 3 comes first
and immediately — remove it from every host — and any release signed only by it
must be re-signed with the new key and its `.sig` asset replaced (the workflow
uploads with `--clobber`, and `gh release upload` by hand does the same). Until
a host has the new key, its applies fail closed with `signature_invalid`, which
is the correct outcome.

## See also

- [`../CHANGELOG.md`](../CHANGELOG.md): what changed in each released version
- [`deploy/README.md`](../deploy/README.md): full deployment guide
- [`docs/configuration.md`](configuration.md): every environment variable
- [`deploy/db-backup-restore-drill.sh`](../deploy/db-backup-restore-drill.sh): the backup/restore rehearsal script referenced above
