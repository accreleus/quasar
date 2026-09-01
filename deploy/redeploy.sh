#!/usr/bin/env bash
# redeploy.sh — canonical full-stack redeploy for one Quasar test environment.
#
# The repo has two test environments that MUST stay in sync, or you end up
# testing a half-updated stack (e.g. a current web bundle against an old
# control-plane that lacks an endpoint — exactly the #190 host-settings drift).
# This script is the ONE deploy path: it updates ALL components (web SPA,
# control-plane, node-agent) from a single git ref and then VERIFIES the running
# stack actually serves what it just built.
#
# It runs ON the target host, from the repo root. The per-host transport lives in
# the skills that invoke it (`qhost redeploy`), which read the host's profile
# from _shared/hosts.json — no box name appears in this script.
#
# Usage (on the host, from the repo root):
#   deploy/redeploy.sh <profile> [ref] [scope]
#     profile  the host's HARDWARE profile, not its name:
#                va      AMD / Intel VA-API (also the Vulkan encoder on those GPUs)
#                nvidia  NVIDIA — CUDA image lineage, NVENC/Vulkan
#              Local display (DRM/KMS console + host audio) is a separate
#              deployment choice on top of either: set QUASAR_CONSOLE=1 in the
#              environment or in deploy/.env.
#              Host ports come from deploy/.env (CONTROL_PORT / QUASAR_TLS_PORT),
#              the same file compose reads — never from a branch in this script.
#     ref   defaults to origin/main (the sync invariant: both hosts track main).
#           A RELEASE TAG is a first-class value here and is what the public
#           quick start passes (`deploy/redeploy.sh nvidia v0.1.0`, issue #510):
#           no `refs/remotes/origin/<tag>` exists, so the sync step below falls
#           through to `git checkout --detach <tag>`, which is exactly right.
#           Pass a branch/SHA only for deliberate branch testing — and deploy it
#           to BOTH hosts so they never diverge.
#     scope defaults to `all` (web + node-agent + control-plane). The two narrow
#           scopes each rebuild ONE component and force-recreate the
#           control-plane, then run the same verification block:
#             web      rebuild the SPA only. The control-plane is recreated (not
#                      rebuilt) so the dist bind-mount inode is re-picked-up
#                      (#131) and the served bundle is re-verified. Use this for
#                      a web-only change instead of hand-rolling
#                      `compose restart`, which silently serves the stale dist
#                      (#131/#7).
#             control  rebuild the Go control-plane image only (compose builds it
#                      from deploy/Dockerfile.control, which reads nothing from
#                      Dockerfile.vulkan). ~1 minute, versus 40+ for the
#                      GStreamer/Rust image set a `scope=all` rebuild drags in
#                      for a change that touches no Rust and no GStreamer.
#                      The SPA is NOT rebuilt; the served-bundle check reads the
#                      hash already on disk, so it still catches a container that
#                      came back up with a broken dist mount.
#           Neither narrow scope touches the node-agent image or container, so
#           running sessions survive the deploy. Both still WAIT for the
#           control-plane to report healthy, which is what proves an embedded
#           migration finished — the CP-only path is precisely the one that
#           carries migrations, so that wait is not optional.
#
# Exit status is 0 only if every post-deploy verification passes. The final
# line is a machine-readable summary the drift-check (qstack sync) parses:
#   REDEPLOY env=<> scope=<> ref=<> sha=<short> bundle=<index-hash.js> \
#            health=<ok|FAIL> catalog=<code> agent=<registered|MISSING> result=<OK|FAIL>
set -euo pipefail

ENV="${1:-}"
REF="${2:-origin/main}"
SCOPE="${3:-all}"

# A literal `HEAD` means the LOCAL checkout to a caller, but `refs/remotes/origin/HEAD`
# resolves below and silently deploys origin/main, reverting the host mid-run (#538).
# Refuse it: there is no reading of `HEAD` that this script can honour safely.
if [ "$REF" = "HEAD" ]; then
  echo "redeploy.sh: refusing ref 'HEAD' — it resolves to origin/HEAD (= main) and would" >&2
  echo "  revert this host. Pass a branch, a tag, or the commit you mean:" >&2
  echo "    bash deploy/redeploy.sh $ENV \"\$(git rev-parse HEAD)\" ${SCOPE}" >&2
  exit 2
fi

usage() {
  echo "usage: deploy/redeploy.sh <va|nvidia> [ref] [all|web|control]" >&2
  echo "       (host ports come from deploy/.env; QUASAR_CONSOLE=1 adds local display)" >&2
  exit 2
}

case "$SCOPE" in
  all|web|control) ;;
  *) usage ;;
esac

# When this deploy started, as a comparable UTC key (YYYYMMDDhhmmss). The verify
# block compares the control-plane container's StartedAt against it to prove the
# recreate actually happened: for a one-service deploy the whole value is in the
# new binary, and `up -d` without --force-recreate is a silent no-op when the
# built image is byte-identical to the running one. Sampled before any work, so
# whole-second granularity can never make a genuine recreate look old.
DEPLOY_KEY="$(date -u +%Y%m%d%H%M%S)"

# Read a key out of deploy/.env without sourcing it — the file holds operator
# values, not shell, and one stray backtick would otherwise execute here.
env_val() {
  [ -f deploy/.env ] || return 0
  sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" deploy/.env | tail -1 | tr -d '"'"'"'[:space:]'
}

case "$ENV" in
  va|amd|intel)
    ENV=va
    COMPOSE_FILES=(-f deploy/docker-compose.yml)
    NA_IMAGE="${NA_IMAGE:-quasar-node-agent:latest}"
    NA_TARGET="runtime"
    NA_BUILD_ARGS=""
    ;;
  nvidia|nv)
    ENV=nvidia
    COMPOSE_FILES=(-f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml)
    # Same image as a VA host since #545: quasar-node-agent is the universal
    # agent image. It is built CUDA_ENABLE=1 like every other lineage, so it
    # carries the CUDA gst plugins; the one thing it does NOT carry is
    # libnvrtc, which the agent fetches at run time into the driver volume
    # (node-agent/src/cuda_runtime.rs). What differs on NVIDIA is the compose
    # overlay — CDI devices, driver capabilities, encoder defaults — not the
    # image.
    NA_IMAGE="${NA_IMAGE:-quasar-node-agent:latest}"
    NA_TARGET="runtime"
    NA_BUILD_ARGS=""
    ;;
  *)
    usage
    ;;
esac

# The dev overlay: this script ALWAYS builds from a working tree, so it always
# deploys the development shape. The base file is the production deployment
# (published images, no build keys, no source mounts), so without this overlay
# `$DC build quasar-control-plane` below has nothing to build, the SPA bind
# mount is absent, and the healthcheck calls curl in an image that ships wget.
#
# INVARIANT — image flavor and healthcheck binary travel together. The
# dev-built control-plane image (Dockerfile.control, debian-slim: wget, no
# curl) must never run under the base file's curl healthcheck: a healthcheck
# calling a missing binary does not fail loudly, the container just sits
# `unhealthy` forever on "curl: not found" and the node agent, gated on
# `condition: service_healthy`, never starts (seen live on a host handed the
# dev CP image with the base compose alone). This unconditional append is the
# mechanism that keeps the pairing on every chain this script composes; the
# pairing itself is asserted per chain by scripts/dev/test-compose-overlays.sh
# (section 7c).
COMPOSE_FILES+=(-f deploy/overlays/docker-compose.dev.yml)

# Local display is a deployment choice, orthogonal to the GPU vendor.
QUASAR_CONSOLE="${QUASAR_CONSOLE:-$(env_val QUASAR_CONSOLE)}"
if [ "${QUASAR_CONSOLE:-0}" = "1" ]; then
  COMPOSE_FILES+=(-f deploy/overlays/docker-compose.console.yml)
  ENV="$ENV+console"
fi

# Volume adoption (#448). The QUASAR_*_VOLUME name overrides moved out of the
# base compose file into the opt-in deploy/overlays/docker-compose.adopt-volumes.yml
# overlay (Compose v5 rejects an empty `name:` default). A host that was
# already using these vars — a stack migrated off a forked compose file —
# must keep working on this script without edits, so the overlay is added
# automatically whenever the vars are configured. Partial configuration is
# refused rather than silently adopting only some volumes: the un-adopted
# ones would come up empty, which reads as data loss.
ADOPT_PG_VOL="${QUASAR_POSTGRES_VOLUME:-$(env_val QUASAR_POSTGRES_VOLUME)}"
ADOPT_AGENT_VOL="${QUASAR_AGENT_VOLUME:-$(env_val QUASAR_AGENT_VOLUME)}"
ADOPT_CTRL_VOL="${QUASAR_CONTROL_VOLUME:-$(env_val QUASAR_CONTROL_VOLUME)}"
if [ -n "$ADOPT_PG_VOL$ADOPT_AGENT_VOL$ADOPT_CTRL_VOL" ]; then
  if [ -z "$ADOPT_PG_VOL" ] || [ -z "$ADOPT_AGENT_VOL" ] || [ -z "$ADOPT_CTRL_VOL" ]; then
    echo "!! Volume adoption needs all three of QUASAR_POSTGRES_VOLUME," >&2
    echo "!! QUASAR_AGENT_VOLUME and QUASAR_CONTROL_VOLUME set together" >&2
    echo "!! (scripts/dev/migrate-compose-volumes.sh prints the values)." >&2
    exit 1
  fi
  COMPOSE_FILES+=(-f deploy/overlays/docker-compose.adopt-volumes.yml)
  echo "volume adoption: using docker-compose.adopt-volumes.yml ($ADOPT_PG_VOL, $ADOPT_AGENT_VOL, $ADOPT_CTRL_VOL)"
fi

DC="docker compose ${COMPOSE_FILES[*]}"

# Verification probes must hit the ports compose actually published, which are
# whatever deploy/.env sets (a box with busy low ports uses e.g. 18080/18443).
PORT="${PORT:-$(env_val CONTROL_PORT)}"; PORT="${PORT:-8080}"
TLS_PORT="${TLS_PORT:-$(env_val QUASAR_TLS_PORT)}"; TLS_PORT="${TLS_PORT:-8443}"

WEB_IMAGE="${WEB_IMAGE:-node:22}"        # node:22 + npm is the known-good web build combo
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

# ---------------------------------------------------------------------------
step "[$ENV] 1/7 sync repo to $REF"
if [ -n "${QUASAR_REDEPLOY_REEXEC:-}" ]; then
  # Second pass: the first pass already synced to $REF and re-exec'd us. Redoing
  # the fetch/checkout here would just be a slow no-op.
  echo "already synced by the first pass (re-exec at $QUASAR_REDEPLOY_REEXEC)"
else
# --tags is explicit rather than relying on git's auto-follow: a host whose
# remote was configured with a narrowed refspec or remote.origin.tagOpt=--no-tags
# would otherwise never see a release tag, and the checkout below would fail with
# an unhelpful "pathspec did not match" on the one ref the quick start names.
git fetch origin --prune --tags
# Pin the working tree to the ref. For origin/main we fast-forward main; for an
# explicit ref (a branch, a SHA, or a release tag) we detach onto it. Untracked
# test artifacts are left alone.
if [ "$REF" = "origin/main" ]; then
  git checkout main
  git merge --ff-only origin/main
else
  # A branch name that exists only on origin trips git's create-branch dwim
  # ("--detach cannot be used with -b"); detach onto the remote ref directly.
  if git rev-parse --verify --quiet "refs/remotes/origin/$REF" >/dev/null; then
    git checkout --detach "origin/$REF"
  else
    git checkout --detach "$REF"
  fi
fi
git submodule update --init --recursive
fi
SHA="$(git rev-parse --short HEAD)"
echo "now at $SHA ($(git log --oneline -1))"

# THE CHECKOUT ABOVE JUST REWROTE THIS SCRIPT. Re-exec so the rest of the run is
# the version we deployed, not the version we started as.
#
# bash reads a script incrementally by byte offset rather than slurping it, so a
# script that rewrites itself mid-run keeps executing stale content (and, if the
# length changed, can resume at a garbage offset). That made every edit to this
# file take effect one deploy LATE, silently: the run that pulled the change
# still executed the previous version. It went unnoticed for as long as the
# changes were to steps that already existed — it only became visible when a
# whole new step was added (QUASAR_SECRET_KEY provisioning) and simply never ran,
# while the script still printed a confident "result=OK".
#
# The guard makes this exactly one re-exec: the second pass sees the marker, skips
# straight past the sync it has already done, and cannot loop.
if [ -z "${QUASAR_REDEPLOY_REEXEC:-}" ]; then
  echo "re-exec: continuing under the freshly checked-out $0"
  export QUASAR_REDEPLOY_REEXEC="$SHA"
  exec bash "$0" "$@"
fi

# Base image for Dockerfile.vulkan (step 5/7). Read from deploy/pins.env, the same
# file build-images.sh reads and the Dockerfile ARG defaults are asserted against —
# so this script cannot hold a stale copy of the ref while the rest of the repo has
# moved on. It used to be a literal here and a second literal in build-images.sh; the
# org move to accretion-io touched them in different commits, and a redeploy from the
# in-between state died minutes into the build on
#   load metadata for ghcr.io/salty2011/quasar-base:develop ... 401 Unauthorized
# — an error naming a ref the operator cannot find in any one place. Override with
# QUASAR_BASE_IMAGE (a release pins an exact base digest that way).
#
# DELIBERATELY BELOW THE RE-EXEC: everything above this line still runs from the
# PRE-sync copy of the script. Sourcing lib/pins.sh up there would read the old
# checkout's pins — and would hard-fail on a host synced to a commit predating the
# file. Down here the tree is the tree we are deploying.
# shellcheck source=deploy/lib/pins.sh
source deploy/lib/pins.sh
BASE="$(quasar_base_image)"
echo "base image: $BASE"

# ---------------------------------------------------------------------------
step "[$ENV] 2/7 ensure required secrets"
# Three vars are `:?`-required by docker-compose.yml interpolation: without
# them compose refuses to start at step 6/7 with "required variable X is
# missing a value" — AFTER the ~15-minute image build at steps 4-5 (#447). This
# step seeds the two that are safe to auto-generate (POSTGRES_PASSWORD,
# ENROLLMENT_TOKEN) into a fresh deploy/.env, then fails fast, before any
# build, if anything is still missing. BOOTSTRAP_ADMIN_* are deliberately NOT
# handled here — see deploy/.env.example: they are optional, and the first-run
# setup wizard claims the founding admin when they are unset.
#
# The QUASAR_SECRET_KEY block below is the original, most delicate case (an
# existing key can never be regenerated without destroying data) and its
# refusal logic is untouched. The two new vars below are simpler — no data is
# ever sealed under them at generation time — but follow the SAME idempotence
# discipline: an existing uncommented non-empty assignment is left untouched,
# never overwritten.
ENV_FILE="deploy/.env"

# deploy/.env must be the single source of truth for these secrets: Compose
# gives an EXPORTED process variable precedence over the .env file, so an
# inherited POSTGRES_PASSWORD would initialize the database with one value
# while this script persists a different one into .env — and the next run,
# without the export, loses database connectivity with nothing pointing here.
# Refuse the ambiguity outright.
for _secret_var in POSTGRES_PASSWORD ENROLLMENT_TOKEN QUASAR_SECRET_KEY; do
  # printenv, not a value test: an exported EMPTY variable also takes
  # precedence over .env in Compose (and would fail the `:?` interpolation
  # only after the build), so mere presence in the environment is the defect.
  if printenv "$_secret_var" >/dev/null 2>&1; then
    echo "!! $_secret_var is exported in this process's environment (even empty," >&2
    echo "!! Compose prefers it over $ENV_FILE, and the two WILL drift apart" >&2
    echo "!! across runs). Put the value in $ENV_FILE and unset the export." >&2
    exit 1
  fi
done

# The effective value of the LAST uncommented assignment — the one Compose
# actually uses — parsed with Compose's dotenv semantics for the cases that
# matter here: surrounding whitespace trimmed, one pair of matching quotes
# removed (so `KEY="change-me"` equals the placeholder it is, and `KEY=""`
# reads as EMPTY, not as a two-character value), and an unquoted value's
# trailing ` # comment` stripped. `KEY=real` followed by a later `KEY=` (an
# easy editing accident) therefore reads as MISSING before the ~15-minute
# build, not after it. `|| true` on the grep: no-match is an expected answer.
env_file_value() {
  local key="$1" last rhs
  [ -f "$ENV_FILE" ] || return 0
  last="$(grep -E "^[[:space:]]*${key}[[:space:]]*=" "$ENV_FILE" | tail -1 || true)"
  [ -n "$last" ] || return 0
  rhs="${last#*=}"
  rhs="${rhs#"${rhs%%[![:space:]]*}"}"   # ltrim
  rhs="${rhs%"${rhs##*[![:space:]]}"}"   # rtrim
  case "$rhs" in
    \"*\") rhs="${rhs#\"}"; rhs="${rhs%\"}" ;;
    \'*\') rhs="${rhs#\'}"; rhs="${rhs%\'}" ;;
    *)     rhs="${rhs%%[[:space:]]#*}"           # dotenv inline comment
           rhs="${rhs%"${rhs##*[![:space:]]}"}" ;;
  esac
  printf '%s' "$rhs"
}
env_file_has_value() { [ -n "$(env_file_value "$1")" ]; }

# State discovery below must be scoped to THIS deployment's compose project:
# a host running several compose projects would otherwise match some other
# project's quasar-postgres. All chains start with deploy/docker-compose.yml,
# so Compose derives the project name "deploy" unless overridden.
COMPOSE_PROJECT="${COMPOSE_PROJECT_NAME:-$(env_val COMPOSE_PROJECT_NAME)}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-deploy}"
PG_VOLUME_NAME="${ADOPT_PG_VOL:-${COMPOSE_PROJECT}_quasar-postgres-data}"

# --- QUASAR_SECRET_KEY -------------------------------------------------------
# The encrypted secret store (migration 0040) needs a 32-byte master key. Without
# one the control plane still boots and everything unrelated works, but any
# credential stored in the DB is unreadable and the admin UI reports credential
# storage unavailable.
#
# THIS IS STRICTLY IDEMPOTENT AND MUST STAY THAT WAY. Regenerating over an
# existing key does not "reset" anything: every stored secret was sealed under
# the old key and becomes permanently unreadable, and the failure surfaces later
# as ErrKeyMismatch, which reads like a restore-the-key incident rather than
# "the deploy script overwrote it". So: generate only when there is no usable
# key, never overwrite, never touch a key we did not just create.
# An uncommented assignment with a non-empty value. A commented-out line (as
# shipped in .env.example) is deliberately NOT a key.
key_line() { env_file_has_value QUASAR_SECRET_KEY; }
prev_line() { env_file_has_value QUASAR_SECRET_KEY_PREVIOUS; }

if key_line; then
  echo "QUASAR_SECRET_KEY already set in $ENV_FILE — left untouched"
elif prev_line; then
  # A half-finished rotation: predecessors are configured but the primary is
  # not. Generating one would be a guess in two ways — the new key would default
  # to version 1, which collides with a v1 already in PREVIOUS (the parser
  # rejects duplicate versions), and any row sealed under a predecessor is
  # readable, so this is not the "key is lost" case either. A human has to say
  # what they intended.
  echo "!! $ENV_FILE sets QUASAR_SECRET_KEY_PREVIOUS but no QUASAR_SECRET_KEY." >&2
  echo "!! That is a half-finished rotation, not a missing key. Set the primary" >&2
  echo "!! explicitly (e.g. QUASAR_SECRET_KEY=2:<base64>) rather than letting this" >&2
  echo "!! script guess a version that may collide with a predecessor." >&2
  exit 1
else
  # The dangerous case: rows exist but the key does not. Auto-generating here
  # would convert "the key is missing, restore it from your backup" into "the
  # ciphertexts are now unopenable by anything, forever" — and hide that it
  # happened. Refuse instead, and let a human decide.
  # POSTGRES_USER is overridable in .env, so read it from there rather than
  # assuming: a wrong -U makes psql fail, which would look identical to "no
  # rows" and send us down the generate path — the unsafe direction.
  # `|| true` is load-bearing, not defensive noise: on a FRESH host there is no
  # deploy/.env yet, sed exits non-zero on the missing file (GNU sed exits 2,
  # BSD 1), `set -o pipefail` promotes that through `| tail -1`, and `set -e`
  # then kills the whole deploy at step 2/7 — silently, because the 2>/dev/null
  # that was meant to hide the message also hides the cause. That is exactly the
  # bootstrap case this branch exists to handle, so a missing file must read as
  # "no POSTGRES_USER override" and fall through to the default below.
  # (Never reproduced on Tower/hermes: both have always had a .env.)
  PG_USER="$( { sed -nE 's/^[[:space:]]*POSTGRES_USER[[:space:]]*=[[:space:]]*([^[:space:]#]+).*/\1/p' "$ENV_FILE" 2>/dev/null || true; } | tail -1)"
  PG_USER="${PG_USER:-quasar}"
  # NOT `$DC ps`: loading the compose file needs the very `:?`-required vars
  # this step may not have seeded yet (a wiped .env next to a live database is
  # exactly this branch's scenario), so `$DC ps` would fail interpolation, the
  # 2>/dev/null would eat the error, and a running postgres would read as
  # absent — sending us down the generate path, the unsafe direction. Plain
  # docker + compose labels, scoped to THIS project, needs no compose file.
  pg_cid="$(docker ps -q \
    --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" \
    --filter label=com.docker.compose.service=quasar-postgres | head -1 || true)"
  if [ -z "$pg_cid" ] && docker volume inspect "$PG_VOLUME_NAME" >/dev/null 2>&1; then
    # A retained data volume with no running database: instance_secrets cannot
    # be inspected, and generating a key here would be a blind guess — if that
    # volume holds sealed rows, they become permanently unreadable the moment
    # a new key is written and used. Fail closed; a human decides.
    echo "!! $ENV_FILE has no QUASAR_SECRET_KEY, but the postgres data volume" >&2
    echo "!! $PG_VOLUME_NAME already exists and the database is not running, so its" >&2
    echo "!! instance_secrets table cannot be checked. Restore the original" >&2
    echo "!! QUASAR_SECRET_KEY, start the existing stack first, or remove the old" >&2
    echo "!! volume if this is genuinely a from-scratch redeploy." >&2
    exit 1
  fi
  existing_secrets=""
  if [ -n "$pg_cid" ]; then
    # Fail CLOSED on any query error: a wrong POSTGRES_USER, an unreachable
    # database, or a permission problem must not read as "no rows" (the old
    # behaviour — every failure collapsed into the generate path). Only a
    # genuinely missing table (fresh deployment, 0040 not yet applied) may
    # proceed, distinguished from a failed query via to_regclass.
    if ! table_present="$(docker exec "$pg_cid" psql -v ON_ERROR_STOP=1 \
        -U "$PG_USER" -d quasar -tAc \
        "SELECT to_regclass('public.instance_secrets')" 2>/dev/null)"; then
      echo "!! Could not inspect the existing database (psql via container $pg_cid" >&2
      echo "!! failed). Refusing to generate QUASAR_SECRET_KEY while the state of" >&2
      echo "!! instance_secrets is unknown — fix database access and re-run." >&2
      exit 1
    fi
    table_present="${table_present//[[:space:]]/}"
    if [ -n "$table_present" ]; then
      if ! existing_secrets="$(docker exec "$pg_cid" psql -v ON_ERROR_STOP=1 \
          -U "$PG_USER" -d quasar -tAc \
          "SELECT count(*) FROM instance_secrets" 2>/dev/null)"; then
        echo "!! Could not count instance_secrets rows. Refusing to generate" >&2
        echo "!! QUASAR_SECRET_KEY while the state is unknown — fix database" >&2
        echo "!! access and re-run." >&2
        exit 1
      fi
      existing_secrets="${existing_secrets//[[:space:]]/}"
    fi
  fi
  if [ -n "$existing_secrets" ] && [ "$existing_secrets" != "0" ]; then
    echo "!! $ENV_FILE has no QUASAR_SECRET_KEY, but instance_secrets holds $existing_secrets row(s)." >&2
    echo "!! Those were sealed under a key this host no longer has. Generating a new one" >&2
    echo "!! would make them permanently unreadable and mask the loss." >&2
    echo "!! Restore the original QUASAR_SECRET_KEY, or clear the stored secrets and re-enter them." >&2
    exit 1
  fi

  # 32 bytes, base64 — exactly what internal/secrets/keyring.go parses. An
  # optional "N:" version prefix is supported for rotation; new keys are v1, so
  # the prefix is omitted.
  NEW_KEY="$(openssl rand -base64 32)"
  umask 077
  touch "$ENV_FILE"
  {
    echo ""
    echo "# QUASAR_SECRET_KEY — master key for the encrypted secret store (migration 0040)."
    echo "# Generated by deploy/redeploy.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ) because none was set."
    echo "# BACK THIS UP. Losing it makes every stored credential unrecoverable; they"
    echo "# must then be re-entered through the admin UI. Rotating is possible: move the"
    echo "# old value to QUASAR_SECRET_KEY_PREVIOUS and set a new '2:<base64>' here."
    echo "QUASAR_SECRET_KEY=$NEW_KEY"
  } >> "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "generated a new QUASAR_SECRET_KEY into $ENV_FILE (mode 600)"
  echo "*** BACK UP $ENV_FILE — without this key, stored credentials cannot be recovered. ***"
fi

# --- POSTGRES_PASSWORD -------------------------------------------------------
# `:?`-required by docker-compose.yml, interpolated into DATABASE_URL as
# postgres://user:PASS@... — hex only, no characters a URL or compose
# interpolation would need escaping. Idempotent, same discipline as above: an
# uncommented non-empty assignment is left untouched, never regenerated.
pg_password_line() { env_file_has_value POSTGRES_PASSWORD; }

if pg_password_line; then
  echo "POSTGRES_PASSWORD already set in $ENV_FILE — left untouched"
else
  # If an initialized database already exists, a freshly generated password
  # here would not match what the DB was actually initialized with (postgres
  # sets its superuser password ONCE, at first start, from the env var of the
  # moment) — the control plane would then fail every connection with a
  # password mismatch that looks unrelated to this script. Refuse instead of
  # guessing, same as the QUASAR_SECRET_KEY instance_secrets guard above.
  #
  # NOT `$DC ps`: loading the compose file needs the `:?`-required
  # POSTGRES_PASSWORD we just established is missing, so `$DC ps` fails
  # interpolation, 2>/dev/null eats the error, and existing state reads as
  # absent — the guard would be dead code in the one state it exists for.
  # Plain docker + compose labels needs no compose file, and the volume check
  # also catches a STOPPED/REMOVED container whose initialized data volume
  # remains (the container check alone would miss it).
  existing_pg_container="$(docker ps -aq \
    --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" \
    --filter label=com.docker.compose.service=quasar-postgres | head -1 || true)"
  existing_pg_volume=""
  # PG_VOLUME_NAME already resolves to the adopted name when adoption is
  # configured, so one exact-name inspect covers both cases (a label filter
  # could not see an adopted volume created by other tooling anyway).
  if docker volume inspect "$PG_VOLUME_NAME" >/dev/null 2>&1; then
    existing_pg_volume="$PG_VOLUME_NAME"
  fi
  if [ -n "$existing_pg_container" ] || [ -n "$existing_pg_volume" ]; then
    echo "!! $ENV_FILE has no POSTGRES_PASSWORD, but existing postgres state was found" >&2
    echo "!! (container: ${existing_pg_container:-none}, volume: ${existing_pg_volume:-none})." >&2
    echo "!! Generating a new password would not match what that database was" >&2
    echo "!! initialized with, and every connection would start failing." >&2
    echo "!! Restore the original POSTGRES_PASSWORD in $ENV_FILE, or remove the old" >&2
    echo "!! postgres volume if this is genuinely a from-scratch redeploy." >&2
    exit 1
  fi
  NEW_PG_PASSWORD="$(openssl rand -hex 24)"
  umask 077
  touch "$ENV_FILE"
  {
    echo ""
    echo "# POSTGRES_PASSWORD — postgres superuser password, interpolated into DATABASE_URL."
    echo "# Generated by deploy/redeploy.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ) because none was set."
    echo "# Hex-only by design: it goes into a postgres:// URL, where other alphabets"
    echo "# (base64's +/=, etc.) need escaping compose does not do for you."
    echo "POSTGRES_PASSWORD=$NEW_PG_PASSWORD"
  } >> "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "generated a new POSTGRES_PASSWORD into $ENV_FILE (mode 600)"
fi

# --- ENROLLMENT_TOKEN ---------------------------------------------------------
# `:?`-required by docker-compose.yml. The node-agent presents this on first
# contact to prove it's allowed to register; unlike POSTGRES_PASSWORD it is not
# baked into any external state at creation time, so there is no "already
# initialized" case to guard — only the standard idempotence rule.
enrollment_token_line() { env_file_has_value ENROLLMENT_TOKEN; }

if enrollment_token_line; then
  echo "ENROLLMENT_TOKEN already set in $ENV_FILE — left untouched"
else
  NEW_ENROLLMENT_TOKEN="$(openssl rand -hex 32)"
  umask 077
  touch "$ENV_FILE"
  {
    echo ""
    echo "# ENROLLMENT_TOKEN — pre-shared token the node-agent presents on first contact"
    echo "# to prove it's allowed to register with the control plane."
    echo "# Generated by deploy/redeploy.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ) because none was set."
    echo "ENROLLMENT_TOKEN=$NEW_ENROLLMENT_TOKEN"
  } >> "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "generated a new ENROLLMENT_TOKEN into $ENV_FILE (mode 600)"
fi

# --- QUASAR_HOME_ROOT (fresh installs only) ----------------------------------
# NOT a `:?`-required var — compose runs fine without it. It is seeded because
# its absence silently costs a feature: with no home root the storage provider
# `auto` used to resolve to the DOCKER VOLUME driver, and library discovery
# could not scan a docker volume (it has no host path the node-agent can
# walk), so a fresh install never produced quick-launch tiles for installed
# games and the library subsystem sat inert with nothing saying so (#472).
# Host-path homes are also the better default on their own merits:
# inspectable, backup-able, and not inside the docker data root (on unraid,
# the size-capped docker.img).
#
# #473 HARD REMOVAL (operator direction 2026-08-25): the docker-volume driver
# no longer exists at all — a rootless host now fails EVERY managed-home
# launch loudly (ErrNoHomeRoot) rather than silently falling back to it. This
# only sharpens why a fresh install gets a root seeded automatically: skipping
# that step here would no longer mean "keeps working the old way", it would
# mean "cannot start a game with save data at all".
#
# ONLY on a genuinely fresh install. Flipping an EXISTING deployment's default
# would not migrate anything — every existing home keeps its recorded ref
# while NEW homes would land on disk, which reads to the user as "my games
# vanished". Existing postgres state (container or data volume) is the same
# fresh-vs-existing signal POSTGRES_PASSWORD's guard above uses.
QUASAR_DEFAULT_HOME_ROOT=/var/lib/quasar/homes

if env_file_has_value QUASAR_HOME_ROOT; then
  echo "QUASAR_HOME_ROOT already set in $ENV_FILE — left untouched"
else
  existing_pg_container="$(docker ps -aq \
    --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" \
    --filter label=com.docker.compose.service=quasar-postgres | head -1 || true)"
  existing_pg_volume=""
  if docker volume inspect "$PG_VOLUME_NAME" >/dev/null 2>&1; then
    existing_pg_volume="$PG_VOLUME_NAME"
  fi
  if [ -n "$existing_pg_container" ] || [ -n "$existing_pg_volume" ]; then
    echo "QUASAR_HOME_ROOT unset and this is an EXISTING deployment — left unset"
    echo "  !! WARNING (#473): the docker-volume fallback this instance may have been"
    echo "  !! relying on was hard-removed. If storage_provider is still 'auto'/unset"
    echo "  !! and no host has a storage root configured, EVERY managed-home launch on"
    echo "  !! this deploy will now fail loudly instead of silently using a volume."
    echo "  !! Set QUASAR_HOME_ROOT (or a per-host root under Admin -> Hosts) before"
    echo "  !! or immediately after this deploy. Existing homes are not migrated by"
    echo "  !! setting it alone — only NEW homes land on the new path."
  else
    umask 077
    touch "$ENV_FILE"
    {
      echo ""
      echo "# QUASAR_HOME_ROOT — host directory holding per-(user, app) managed homes."
      echo "# Seeded by deploy/redeploy.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ) because this was a"
      echo "# fresh install. Host-path homes are the ONLY driver (#473 hard removal,"
      echo "# 2026-08-25): the docker-volume driver this used to fall back to could not"
      echo "# be scanned by library discovery, so quick-launch tiles for installed games"
      echo "# never appeared on it (#472) — it no longer exists at all."
      echo "# Must be the SAME absolute path on the control-plane host and every node-agent"
      echo "# host. On unraid prefer /mnt/cache/appdata/quasar/homes."
      echo "QUASAR_HOME_ROOT=$QUASAR_DEFAULT_HOME_ROOT"
    } >> "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    echo "seeded QUASAR_HOME_ROOT=$QUASAR_DEFAULT_HOME_ROOT into $ENV_FILE (fresh install)"
    # Pre-create the bind-mount source when we can — it lets the homes root exist
    # with sane 0755 ownership rather than the root:root docker would give it at
    # container start.
    #
    # BEST-EFFORT ON PURPOSE. `/var/lib` is not writable by the unprivileged user
    # a normal deploy runs as, and an earlier version of this block ran a bare
    # `mkdir -p` that aborted the ENTIRE deploy under `set -e` on exactly that —
    # a fresh install on the devbox died at step 2/7 with nothing but
    # "Permission denied". The directory is not actually required here: dockerd
    # runs as root and creates a missing bind-mount source itself at container
    # start. So a failure to pre-create it is a note, not an error.
    #
    # The `umask 077` above is for the .env APPEND and must not reach this mkdir:
    # it would create the homes root 0700, so anything walking it from a non-root
    # context (including the agent's own bytes_used measurement) fails with a
    # permission error rather than returning an empty result. Subshell so the
    # umask change cannot leak further either.
    if ( umask 022; mkdir -p "$QUASAR_DEFAULT_HOME_ROOT" 2>/dev/null ); then
      :
    else
      echo "   note: could not pre-create $QUASAR_DEFAULT_HOME_ROOT as $(id -un) —"
      echo "   docker will create it at container start (owned by root). To own it"
      echo "   yourself instead: sudo install -d -m 0755 -o $(id -un) $QUASAR_DEFAULT_HOME_ROOT"
    fi
  fi
fi

# --- fail fast, before any build -------------------------------------------
# Re-check every compose-`:?`-required var by the same rule the seeding above
# used (uncommented, non-empty). This catches every refusal-to-generate path
# above (QUASAR_SECRET_KEY's half-rotation/instance_secrets guards,
# POSTGRES_PASSWORD's already-running guard) in one place, AND catches a
# operator-edited-but-still-empty value, e.g. `POSTGRES_PASSWORD=`. Placed at
# the end of step 2/7 so the ~15-minute image build at steps 4-5 never starts
# on a .env that compose will reject at step 6/7 anyway (#447).
missing=""
key_line || missing="$missing QUASAR_SECRET_KEY"
pg_password_line || missing="$missing POSTGRES_PASSWORD"
enrollment_token_line || missing="$missing ENROLLMENT_TOKEN"
if [ -n "$missing" ]; then
  echo "!! $ENV_FILE is still missing a value for:$missing" >&2
  echo "!! These are required by docker-compose.yml interpolation — compose refuses" >&2
  echo "!! to start without them. Set them in $ENV_FILE and re-run." >&2
  exit 1
fi

# The historical .env.example shipped ACTIVE placeholder assignments
# (`change-me-...`), so a `cp .env.example .env` from an old checkout passes
# every presence check above and deploys with publicly-known credentials —
# the enrollment token is an authentication credential, so anyone reading the
# repo could enroll a rogue node. The example now ships them commented out
# (so the generation path runs), but old copies exist; refuse the values
# themselves.
for _pair in \
  "POSTGRES_PASSWORD:change-me-strong-password" \
  "ENROLLMENT_TOKEN:change-me-enrollment-token"; do
  _k="${_pair%%:*}"; _placeholder="${_pair#*:}"
  if [ "$(env_file_value "$_k")" = "$_placeholder" ]; then
    echo "!! $ENV_FILE sets $_k to the public placeholder value from .env.example." >&2
    echo "!! That is not a secret — anyone with repo access knows it. Delete the line" >&2
    echo "!! (this script will generate a real value) or set one yourself." >&2
    exit 1
  fi
done

# ---------------------------------------------------------------------------
step "[$ENV] 3/7 ensure QUASAR_TLS_HOSTS"
# The default self-signed cert can only ever see the CONTAINER's addresses, so
# without this the cert has no SAN for https://<this-host's-lan-ip>:$TLS_PORT and
# every LAN browser gets a permanent ERR_CERT_COMMON_NAME_INVALID (a name
# mismatch, which trusting the cert does not clear). Idempotent: an operator's
# existing value is never rewritten. Details in the script's header.
bash deploy/seed-tls-hosts.sh "$ENV_FILE"

# ---------------------------------------------------------------------------
bundle_on_disk() {
  [ -f web/dist/index.html ] || return 0
  grep -o 'assets/index-[A-Za-z0-9_-]*\.js' web/dist/index.html | head -1 | sed 's#assets/##'
}

if [ "$SCOPE" = control ]; then
  step "[$ENV] 4/7 skip web SPA build (scope=control)"
  # Read the hash the bind mount ALREADY holds rather than rebuilding it. The
  # served-bundle check below then still fires — it just asserts "the recreated
  # container serves the dist that is on disk" instead of "serves what we just
  # built". That is the assertion that has value here: a CP-only deploy cannot
  # change the bundle, but it CAN come back up with the mount broken (#131).
  BUNDLE="$(bundle_on_disk)"
  if [ -n "$BUNDLE" ]; then
    echo "existing web bundle on disk: $BUNDLE"
  else
    echo "no web/dist/index.html — the SPA has never been built on this host;"
    echo "the served-bundle check below is skipped (run scope=web or scope=all to build it)"
  fi
else
  step "[$ENV] 4/7 build web SPA ($WEB_IMAGE)"
  # web/dist is a bind mount into the control-plane container (see CLAUDE.md #131).
  docker run --rm -v "$PWD":/w -w /w/web "$WEB_IMAGE" \
    sh -c "npm install --no-audit && npm run build"
  BUNDLE="$(bundle_on_disk)"
  echo "built web bundle: $BUNDLE"
fi

# ---------------------------------------------------------------------------
if [ "$SCOPE" = all ]; then
  step "[$ENV] 5/7 build node-agent image ($NA_IMAGE, target=$NA_TARGET)"
  # ONE target for both environments since #545. Every lineage is built
  # CUDA_ENABLE=1 (it has been the default since 2026-07-26), so the image ships
  # the CUDA gst plugins as well as the patched GStreamer/compositor media
  # lineage, and a self-contained image keeps the baked agent binary, plugins and
  # source revision from drifting independently.
  #
  # HISTORY, so the old shape is not restored by reflex: this script used to
  # build `--target nv --build-arg CUDA_ENABLE=1` on NVIDIA because the runtime
  # target really did default to CUDA_ENABLE=0 — a runtime image then had no
  # `cudaconvert` and any session falling back to NVENC died with "build encode
  # pipeline: cudaconvert not found" (#384, reproduced 2026-07-26 launching Ball
  # at 1080p60, which is av1-first there). Both halves of that have since
  # changed: the CUDA plugins are in every image, and the one remaining
  # NVIDIA-only piece (libnvrtc) is fetched at run time.
  # shellcheck disable=SC2086 # NA_BUILD_ARGS is a deliberate word-split arg list
  docker build -f deploy/Dockerfile.vulkan --target "$NA_TARGET" $NA_BUILD_ARGS \
    --build-arg QUASAR_BASE_IMAGE="$BASE" -t "$NA_IMAGE" .
else
  step "[$ENV] 5/7 skip node-agent build (scope=$SCOPE)"
fi

# ---------------------------------------------------------------------------
step "[$ENV] 6/7 recreate control-plane${SCOPE:+ (scope=$SCOPE)}"
# Control-plane is built by compose (Go: picks up new endpoints + migrations).
# Skipped for scope=web ONLY — there the binary is unchanged and we just need to
# re-mount the freshly-built dist by recreating the container (bind-mount inode
# swap, #131). scope=control is the opposite case: the build IS the deploy.
#
# This build reads deploy/Dockerfile.control (a golang:1.25-alpine compile plus a
# debian-slim runtime) and nothing from Dockerfile.vulkan — which is what makes
# scope=control cheap. Do not "optimise" it by folding the control-plane into the
# vulkan image lineage.
if [ "$SCOPE" != web ]; then
  $DC build quasar-control-plane
fi
# Three separate ups, each force-recreate confined to its named service with
# --no-deps (#453): on Compose v5, `up --force-recreate <svc>` recreates the
# service's DEPENDENCIES too, so the node-agent up below used to recreate
# postgres seconds after the control-plane up created it — under the control
# plane's FIRST-BOOT migration run on a virgin database, killing the
# connection mid-migration and leaving schema_migrations dirty (crash-loop:
# "Dirty database version N. Fix and force version."). Tower/hermes never hit
# it because an already-migrated database's boot migration run is a
# milliseconds no-op; only a virgin database has a window wide enough.
#
# Postgres itself is never force-recreated: a redeploy changes the CP binary
# and the agent image, not the database container, and create-if-missing is
# exactly what a first boot needs.
# --wait: on a VIRGIN box postgres spends its first seconds in initdb, and the
# control-plane up below runs --no-deps (deliberately, #453), which strips the
# compose service_healthy dependency wait along with the recreate cascade. The
# CP's boot-time migration connect then races initdb, crash-loops on
# "connection refused", and the CP --wait below aborts the whole deploy —
# seconds before it would have succeeded (#467, caught by the first-run
# acceptance loop). An already-initialized postgres passes this in
# milliseconds, which is why Tower/hermes never saw it.
$DC up -d --wait --wait-timeout 120 quasar-postgres
# --wait on the control-plane: healthy means /health answers, which means the
# migration run COMPLETED — nothing below may touch the stack before that.
# 300s: a virgin database runs the full migration chain on first boot.
$DC up -d --force-recreate --no-deps --wait --wait-timeout 300 quasar-control-plane
if [ "$SCOPE" = all ]; then
  # Recreate the node-agent from the freshly-built, self-contained Vulkan image.
  # `up -d` can return while a dependency-health wait has left the recreated
  # agent in Docker's Created state (observed repeatedly on Tower). `--wait`
  # makes the deployment contract require the new agent to be running/healthy.
  $DC up -d --force-recreate --no-deps --wait --wait-timeout 60 quasar-node-agent
fi

# ---------------------------------------------------------------------------
step "[$ENV] 7/7 verify running stack"
fail=0

# Give the control-plane a moment to come healthy + the agent to reconnect.
# /health stays on the plain-HTTP listener by design (#376: the agent surface and
# the compose healthcheck are exempt from the HTTPS redirect).
for _ in $(seq 1 30); do
  curl -fsS "http://localhost:$PORT/health" >/dev/null 2>&1 && break
  sleep 1
done

health=FAIL
curl -fsS "http://localhost:$PORT/health" >/dev/null 2>&1 && health=ok
[ "$health" = ok ] || { echo "  FAIL: /health not responding on :$PORT"; fail=1; }

# BROWSER-facing probes must use HTTPS. Since the HTTP->HTTPS redirect shipped
# (develop 20b1d33), plain HTTP answers every browser route with a 308 and an
# empty body — so these two checks reported a bundle mismatch and catalog=308 on
# EVERY redeploy, i.e. `result=FAIL` on a perfectly healthy stack. -k because the
# default cert is the self-signed one generated at first boot.
CURL_TLS="curl -k --connect-timeout 5 --max-time 20"

# Served bundle must match the one on disk (catches the bind-mount/inode +
# stale-container traps: if these differ, the running container is serving an
# old dir or an old build). For scope=all/web that hash is what we just built;
# for scope=control it is what was already there — see step 4/7.
if [ -z "$BUNDLE" ]; then
  echo "  note: no bundle hash on disk (SPA never built here) — served-bundle check skipped"
else
  served="$($CURL_TLS -fsS "https://localhost:$TLS_PORT/" 2>/dev/null | grep -o 'assets/index-[A-Za-z0-9_-]*\.js' | head -1 | sed 's#assets/##' || true)"
  if [ "$served" != "$BUNDLE" ]; then
    echo "  FAIL: served bundle '$served' != on-disk bundle '$BUNDLE' (stale container — restart it)"
    fail=1
  else
    echo "  ok: serving on-disk bundle $served"
  fi
fi

# The control-plane container must have been REPLACED by this run, not left
# alone. Every scope force-recreates it, so a StartedAt older than this run means
# the recreate silently did not happen — which for scope=control would report a
# green deploy while the OLD Go binary is still serving (and its migrations still
# unapplied). `date -d` is GNU-only and this also has to run on the BSD-ish
# userlands in the fleet, so the RFC3339 stamp is parsed in-shell instead.
cp_cid="$($DC ps -q quasar-control-plane 2>/dev/null || true)"
if [ -z "$cp_cid" ]; then
  echo "  FAIL: no control-plane container after the deploy"
  fail=1
else
  started_at="$(docker inspect -f '{{.State.StartedAt}}' "$cp_cid" 2>/dev/null || true)"
  # Both stamps are zero-padded UTC, so dropping every non-digit and truncating
  # to whole seconds (YYYYMMDDhhmmss) makes them directly comparable as
  # integers — no `date -d` (GNU-only) and no locale-sensitive string collation.
  started_key="$(printf '%s' "$started_at" | tr -cd '0-9' | cut -c1-14)"
  if [ "${#started_key}" -ne 14 ]; then
    echo "  note: could not read the control-plane StartedAt ('$started_at') — recreate check skipped"
  elif [ "$started_key" -lt "$DEPLOY_KEY" ]; then
    echo "  FAIL: control-plane started at $started_at, BEFORE this deploy began —"
    echo "        the container was not recreated, so the old binary is still running"
    fail=1
  else
    echo "  ok: control-plane container recreated by this run ($started_at)"
  fi
fi

# Catalog endpoint proves the control-plane binary is current: 401 = route
# exists (auth required); 404/200-SPA-fallback = old binary without it.
# NB: no -f here — we WANT the 401 status, and curl -f would exit non-zero on it.
catalog="$($CURL_TLS -sS -o /dev/null -w '%{http_code}' "https://localhost:$TLS_PORT/v1/admin/config/catalog" 2>/dev/null || echo 000)"
if [ "$catalog" = 401 ]; then
  echo "  ok: /v1/admin/config/catalog -> 401 (route present)"
else
  echo "  FAIL: /v1/admin/config/catalog -> $catalog (expected 401; old control-plane?)"
  fail=1
fi

# Every name in QUASAR_TLS_HOSTS must actually be a SAN in the cert being
# served. It can legitimately be missing: the self-signed pair is generated ONCE
# and reused for ~10 years (so an accepted browser exception survives restarts),
# so a name added to .env AFTER first boot is NOT in the cert, including a name
# step 3 just seeded onto a stack that had already booted. Without this check the
# operator only finds out as a permanent ERR_CERT_COMMON_NAME_INVALID that
# re-trusting never clears. WARN, never FAIL: re-issuing is the operator's call
# because it changes the fingerprint and invalidates every trust anchor already
# installed on their clients.
tls_hosts="$(sed -nE 's/^[[:space:]]*QUASAR_TLS_HOSTS[[:space:]]*=[[:space:]]*([^[:space:]#]+).*/\1/p' "$ENV_FILE" 2>/dev/null | tail -1)"
if [ -n "$tls_hosts" ] && command -v openssl >/dev/null 2>&1; then
  cert_sans="$(openssl s_client -connect "localhost:$TLS_PORT" </dev/null 2>/dev/null |
    openssl x509 -noout -ext subjectAltName 2>/dev/null || true)"
  if [ -z "$cert_sans" ]; then
    echo "  note: could not read the served cert's SANs (QUASAR_TLS=off, or an operator cert); skipped"
  else
    missing=""
    for h in $(printf '%s' "$tls_hosts" | tr ',' ' '); do
      printf '%s' "$cert_sans" | grep -qE "(DNS|IP Address):$h(,|$)" || missing="$missing $h"
    done
    if [ -n "$missing" ]; then
      echo "  WARN: served cert has NO SAN for:$missing (QUASAR_TLS_HOSTS lists them)"
      echo "        Those URLs will fail hostname validation in the browser. The cert is"
      echo "        reused from first boot, so re-issue it to pick the names up:"
      echo "          $DC exec quasar-control-plane rm -f /var/lib/quasar-control/tls/cert.pem /var/lib/quasar-control/tls/key.pem"
      echo "          $DC up -d --force-recreate quasar-control-plane"
      echo "        The new cert has a NEW fingerprint: every client that trusted the old"
      echo "        one must accept/trust the new one again."
    else
      echo "  ok: served cert covers QUASAR_TLS_HOSTS ($tls_hosts)"
    fi
  fi
fi

# Agent must re-register after the recreate. Registration lands a few seconds
# after the control-plane container comes up (agent reconnect backoff), so poll
# up to 30s instead of a single grep — a one-shot check raced and false-FAILed
# redeploy-all's hermes leg, aborting the Tower leg.
agent=MISSING
agent_cid="$($DC ps -q quasar-node-agent 2>/dev/null || true)"
if [ -z "$agent_cid" ] || [ "$(docker inspect -f '{{.State.Running}}' "$agent_cid" 2>/dev/null || true)" != true ]; then
  echo "  FAIL: node-agent container is not running"
  fail=1
fi
for _ in $(seq 1 15); do
  if $DC logs --tail 80 quasar-control-plane 2>/dev/null | grep -q 'agent registered'; then
    agent=registered
    break
  fi
  sleep 2
done
if [ "$agent" = registered ]; then
  echo "  ok: node-agent registered"
else
  echo "  WARN: no 'agent registered' in control-plane logs after 30s"
  fail=1
fi

result=OK; [ "$fail" -eq 0 ] || result=FAIL
echo
echo "REDEPLOY env=$ENV scope=$SCOPE ref=$REF sha=$SHA bundle=$BUNDLE health=$health catalog=$catalog agent=$agent result=$result"
exit "$fail"
