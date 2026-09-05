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

# 3. Bring it up. --no-deps so nothing else is touched.
docker compose -f deploy/docker-compose.yml up -d --no-deps quasar-updater

# 4. Verify it discovered the stack it is sitting beside.
docker compose -f deploy/docker-compose.yml exec quasar-node-agent \
  curl -s --unix-socket /run/quasar-updater/updater.sock http://u/v1/self
```

That last command should print the compose project, the working directory, the
`-f` files (**including every overlay you use**) and the namespace allowlist. If
it instead reports that the stack directory is not visible in the container,
`QUASAR_STACK_DIR` is wrong or unset — the updater fails closed rather than
guessing at a compose invocation and recreating the wrong project's containers.

`deploy/redeploy.sh` seeds `QUASAR_STACK_DIR` for you, so a source install that
deploys through it only needs step 1 and step 3.

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

Every result carries the previous digests, so the manual restore is copy-paste:

```bash
docker compose -f deploy/docker-compose.yml exec quasar-node-agent \
  curl -s --unix-socket /run/quasar-updater/updater.sock \
  http://u/v1/results/<request-id>
```

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

## See also

- [`../CHANGELOG.md`](../CHANGELOG.md): what changed in each released version
- [`deploy/README.md`](../deploy/README.md): full deployment guide
- [`docs/configuration.md`](configuration.md): every environment variable
- [`deploy/db-backup-restore-drill.sh`](../deploy/db-backup-restore-drill.sh): the backup/restore rehearsal script referenced above
