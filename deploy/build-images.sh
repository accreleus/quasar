#!/usr/bin/env bash
#
# build-images.sh — the ONE supported way to build the Quasar container image set.
#
#   deploy/build-images.sh                          # runtime + control, validated
#   deploy/build-images.sh runtime --gwd-ref abc123 # test a compositor pin
#   deploy/build-images.sh runtime --git-ref feature/x  # another branch, untouched checkout
#   deploy/build-images.sh all --dry-run            # show the exact docker build lines
#
#   deploy/build-images.sh toolchain                       # build the ~40 min artefact
#   deploy/build-images.sh toolchain --registry ghcr.io/accreleus/quasar --push
#
# IMAGE NAMES NAME A ROLE, NOT AN IMPLEMENTATION (rename 2026-08-26). The old
# names described how an image happened to be built (`quasar-vulkan` = "the one
# with Vulkan in it") rather than what it is for. They are now:
#
#   quasar-vulkan     -> quasar-node-agent
#   quasar-control    -> quasar-control-plane
#   quasar-toolchain  -> quasar-gst-toolchain
#   quasar-dev        -> quasar-agent-dev
#
# Every old name is still written as a LOCAL alias tag alongside the new one, so
# an un-migrated deploy/.env or a hand-typed `docker run` on an existing box keeps
# resolving. Pass --no-legacy-alias to stop writing them. Removal condition is the
# same as the registry aliases in .github/workflows/images.yml: drop them after the
# first release that ships the new names, plus one more.
#   deploy/build-images.sh runtime --toolchain registry    # ~5 min: pull /opt/gst, compile the agent
#
# THE TOOLCHAIN ARTEFACT. Dockerfile.vulkan splits the ~40-minute patched-GStreamer
# build (`toolchain`) from the ~5-minute node-agent build (`build`). The toolchain is
# keyed on deploy/pins.env + deploy/patches/ + the toolchain stage text and tagged with
# that content hash (deploy/toolchain-hash.sh), so it is built once per pin bump and
# reused by quasar-node-agent and quasar-agent-dev alike. `--toolchain registry` makes
# an agent build PULL it; the default `local` builds it in-line as before.
#
# WHY THIS EXISTS
#   Three overlapping build paths existed (dev.sh image, build-image.sh,
#   build-agent-tower.sh) with different target/arg defaults, plus hand-typed
#   `docker build` lines in docs. Dockerfile.vulkan is multi-target and Docker's
#   no---target default is "last stage wins", so a bare build silently produced the
#   fat CUDA image under whatever -t was passed (hit live 2026-07-12). Meanwhile the
#   shipped nv image drifted 6.15GB of build toolchain and lost the pulseaudio daemon
#   without anything noticing (2026-07-26: every Tower session had silent audio).
#
#   This script is the single entrypoint: it always passes an explicit --target, it
#   validates every artifact against deploy/image-contract.json BEFORE promoting
#   :latest or pushing, and it writes a build report recording exactly which pins,
#   base image, and git commit produced each image.
#
#   Spec: docs/design/plans/2026-07-26-image-lineage-consolidation-spec.md
#
# ROLES
#   runtime   quasar-node-agent    Dockerfile.vulkan --target runtime   AMD/Intel + Vulkan
#   updater   quasar-updater       Dockerfile.updater --target runtime  per-host updater
#   dev       quasar-agent-dev     Dockerfile.vulkan --target dev       build/test env only
#   control   quasar-control-plane Dockerfile.control.prod              control plane
#   profiling quasar-profiling     Dockerfile.vulkan --target profiling PROF-02 capture image
#   all                       runtime dev control (NOT profiling)
#   (default when no role is given: runtime control)
#
#   THERE IS NO NVIDIA ROLE (#545, 2026-08-26). `runtime` is the universal agent image:
#   it is CUDA-built like every other lineage, and the one NVIDIA-only library it does
#   not carry (libnvrtc) is fetched by the agent at run time into the driver volume.
#   An NVIDIA host differs by compose overlay, not by image.
#
#   `profiling` (PROF-02, #389) is on-demand and NEVER promoted: no contract validation,
#   no :latest, no push, ever. It is the node-agent rebuilt under [profile.profiling]
#   (release codegen + line tables + frame pointers) plus samply, and it sits outside
#   deploy/image-contract.json on purpose because it is not a shipped artifact. That is
#   not the contract being relaxed — the contract still gates every role that ships.
#   Recipe: docs/profiling-rust.md.
#
# PIN / BUILD-ARG OVERRIDES
#   --base-image REF        --gst-version REF        --gwd-repo URL
#   --gwd-ref SHA           --interpipe-ref SHA      --plugins-rs-ref SHA
#   --rust-version V        --cargo-c-version V      --cuda-pkg-version V
#   --docker-version V      --cuda-enable 0|1        --gst-tests enabled|disabled
#   --build-arg KEY=VALUE   (repeatable escape hatch for anything not listed)
#
#   Every override is checked against the ARGs the Dockerfile actually declares and
#   fails loudly if undeclared — a silently-ignored --build-arg is how an "applied"
#   pin turns out never to have been applied.
#
# BUILDING A DIFFERENT BRANCH OR COMMIT
#   --git-ref REF           build from a throwaway `git worktree` at REF. Your working
#                           checkout is never touched, so you can keep editing while it
#                           builds. Removed on exit, including on failure.
#   --worktree-dir DIR      where to put it. Default is <repo>/.build-tmp/worktree-*,
#                           deliberately NOT /tmp: on the unraid box (Tower) /tmp is a
#                           ramdisk and the operator rule is that everything lives under
#                           the repo at /mnt/user/appdata/quasar.
#
# TAGGING / PUBLISHING
#   --tag-suffix S          extra component on the dated tag
#   --registry HOST/NS      registry prefix, required for --push
#   --push                  push AFTER a passing validation, never before
#   --no-latest             skip the :latest promotion
#   --keep N                dated generations to retain per image (default 2)
#   --no-prune              preserve all existing images (shared-host validation)
#   --no-legacy-alias       do NOT write the pre-rename local alias tags
#                           (quasar-vulkan / quasar-control / quasar-toolchain /
#                           quasar-dev). They are written by default so an
#                           un-migrated deploy/.env keeps resolving.
#
# VALIDATION
#   --no-validate           skip the contract check (also disables --push)
#   --gpu / --no-gpu        force GPU-gated assertions on/off (default: auto-detect)
#   --contract FILE         default deploy/image-contract.json
#   --require-element E     extra device-gated element to assert (repeatable)
#
# BUILD CACHE (buildx --cache-from / --cache-to)
#   --cache SPEC            off | local:<dir> | registry:<ref>     (default: off)
#   --cache-mode rw|ro|wo   read+write / read-only / write-only    (default: rw)
#   --builder NAME          buildx builder to use
#
#   Also settable as QUASAR_BUILD_CACHE / QUASAR_BUILD_CACHE_MODE / QUASAR_BUILDX_BUILDER,
#   which is how a CI workflow passes them without editing this file.
#
#   `off` is the default and is byte-for-byte today's behaviour: no cache flags, the
#   local builder's own layer cache and nothing else. Every other value opts in to an
#   EXTERNAL cache that survives a wiped builder — which is the whole point on a
#   throwaway CI runner, where the builder starts empty every time.
#
#   local:<dir>      cache lives in <dir>/<role>. For a dev box or a self-hosted runner
#                    with a persistent volume.
#   registry:<prefix> cache lives at <prefix>/<image>:buildcache — the SAME refs
#                    .github/workflows/images.yml already writes today, so CI and a
#                    dev box warm each other's cache instead of keeping two:
#                      QUASAR_BUILD_CACHE=registry:ghcr.io/accreleus/quasar
#                    -> ghcr.io/accreleus/quasar/quasar-node-agent:buildcache, …
#                    Requires a registry login with push rights for cache-mode rw|wo;
#                    use --cache-mode ro from a fork PR that cannot push.
#
#   Cache EXPORT (rw/wo) needs a buildx builder with a non-`docker` driver —
#   docker-container or remote. The default `docker` driver silently cannot export,
#   so this script REFUSES rather than run a build whose cache write is a no-op:
#     docker buildx create --name quasar --driver docker-container --use
#
#   Cache scope is per ROLE, never shared between roles. Two roles have different final
#   layers, and a single scope makes the later role evict the earlier one's manifest —
#   the failure mode is a cache that is always warm for exactly one image.
#
# DEPLOY THE AGENT ON THIS HOST
#   --deploy                after a successful `runtime` build, recreate this host's
#                           quasar-node-agent and PROVE the running container is the
#                           image that was just built. Absorbed from the former
#                           deploy/build-images.sh (2026-08-27), which was already a
#                           thin wrapper over this script — the only thing it added was
#                           this verification, and a second entrypoint is exactly the
#                           divergence this script exists to prevent.
#
#                           It asserts, in order: the running container's image id equals
#                           the freshly built one (the 2026-07-14 wrong-tag trap); the
#                           agent process is the image's BAKED binary at
#                           /usr/local/bin/quasar-node-agent, not a bind-mounted
#                           target/release build re-introduced by a compose `command:`
#                           override; and QUASAR_PULSE_IMAGE actually contains a
#                           pulseaudio daemon (a sidecar without one muted every session
#                           on the host with only a WARN — 2026-07-26).
#
#                           Run it ON the host, from the repo root, e.g.
#                             deploy/build-images.sh runtime --deploy
#                             deploy/build-images.sh runtime --deploy --gpu
#
# OTHER
#   --report FILE           JSON build report (default deploy/.build-report.json)
#   --min-free-gb N         pre-flight disk floor (default 20)
#   --prune-cache           drop the docker builder cache first (slow next build)
#   --dry-run               print the docker build commands and exit
#   --no-cache              docker build --no-cache
#   -h|--help
#
# EXIT CODES
#   0 all requested roles built and validated
#   1 a build or a validation failed
#   2 usage error / missing prerequisite
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

die()  { printf '\nbuild-images: %s\n\n' "$*" >&2; exit 2; }
log()  { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
warn() { printf '[%s] WARNING: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
# Print the whole leading comment block, whatever length it is. A hardcoded line
# range silently truncated --help the first time anyone added a section.
usage() { sed -n '2,/^set -euo pipefail$/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; }

# ── Defaults ──────────────────────────────────────────────────────────────────
ROLES=()
declare -a EXTRA_ARGS=()          # KEY=VALUE build args
declare -a REQUIRE_ELEMENTS=()
GIT_REF=""; WORKTREE_DIR=""
TAG_SUFFIX=""; REGISTRY=""; DO_PUSH=0; NO_LATEST=0; NO_PRUNE=0; KEEP=2; LEGACY_ALIAS=1
DO_VALIDATE=1; GPU_FLAG=""; CONTRACT="$SCRIPT_DIR/image-contract.json"
REPORT="$SCRIPT_DIR/.build-report.json"
MIN_FREE_GB=20; PRUNE_CACHE=0; DRY_RUN=0; NO_CACHE=0
DO_DEPLOY=0
# External build cache. Env-first so CI can set it without a flag; the flags below win.
BUILD_CACHE="${QUASAR_BUILD_CACHE:-off}"
BUILD_CACHE_MODE="${QUASAR_BUILD_CACHE_MODE:-rw}"
BUILDX_BUILDER="${QUASAR_BUILDX_BUILDER:-}"
# shellcheck source=deploy/lib/pins.sh
source "$SCRIPT_DIR/lib/pins.sh"
PINS_FILE="$QUASAR_PINS_FILE"
TOOLCHAIN_MODE=local
TOOLCHAIN_REGISTRY="${QUASAR_TOOLCHAIN_REGISTRY:-ghcr.io/accreleus/quasar}"

# The base image comes from pins.env — the same file the Dockerfile ARG defaults are
# asserted against — so this script, redeploy.sh and the Dockerfiles cannot disagree
# about it. It used to be a literal here and a second literal in redeploy.sh; the org
# move updated them in different commits and a redeploy from the in-between state died
# on a 401 for a ref that appears in no actionable error message.
#
# Base image channel (org move 2026-08-08): quasar-images publishes :latest from its
# `stable` branch, plus an immutable :sha-<commit> per build. It does NOT publish a
# :develop channel unless someone dispatches the workflow from `develop`, so :latest is
# the only moving tag that always resolves — a :develop default would die at
# "failed to resolve source metadata ... not found" before compiling anything.
# (Pre-move this was inverted: the old salty2011 package had :develop and no :latest.)
# Override with --base-image or QUASAR_BASE_IMAGE (e.g. to pin a base digest for a release).
BASE_IMAGE_DEFAULT="$(quasar_base_image)" || die "could not read QUASAR_BASE_IMAGE from $PINS_FILE"
BASE_IMAGE_SET=0

# CUDA_ENABLE defaults to 1: the shipped lineage is ALWAYS CUDA-built, so one
# /opt/gst and one agent binary serve both vendors. Verified safe on non-NVIDIA hosts
# (nvcodec registers 0 features without libcuda; the agent has no libcuda DT_NEEDED).
# Setting it to 0 produces an image that cannot serve an NVIDIA host — the contract
# rejects such an image for the runtime role.
add_arg() { EXTRA_ARGS+=("$1"); }

while [ $# -gt 0 ]; do
  case "$1" in
    toolchain|runtime|dev|control|profiling|updater) ROLES+=("$1"); shift ;;
    # `nv` was the NVIDIA lineage, retired by #545. Named explicitly so an old
    # command line or CI job fails with a sentence instead of "unknown argument".
    nv) die "the 'nv' role was retired by #545: quasar-node-agent (role 'runtime') is the universal agent image, and the CUDA userspace it needs is fetched at run time. Build 'runtime'." ;;
    # `profiling` is deliberately NOT in `all`: it is an on-demand diagnostic variant,
    # and a routine `all` build must not silently spend a full extra Rust compile on it.
    all) ROLES+=(runtime dev control updater); shift ;;

    --base-image)       add_arg "QUASAR_BASE_IMAGE=${2:?}"; BASE_IMAGE_SET=1; shift 2 ;;
    --gst-version)      add_arg "GST_VERSION=${2:?}";              shift 2 ;;
    --gwd-repo)         add_arg "GST_WAYLAND_DISPLAY_REPO=${2:?}"; shift 2 ;;
    --gwd-ref)          add_arg "GST_WAYLAND_DISPLAY_REF=${2:?}";  shift 2 ;;
    --interpipe-ref)    add_arg "GST_INTERPIPE_REF=${2:?}";        shift 2 ;;
    --plugins-rs-ref)   add_arg "GST_PLUGINS_RS_REF=${2:?}";       shift 2 ;;
    --rust-version)     add_arg "RUST_VERSION=${2:?}";             shift 2 ;;
    --cargo-c-version)  add_arg "CARGO_C_VERSION=${2:?}";          shift 2 ;;
    --cuda-pkg-version) add_arg "CUDA_PKG_VERSION=${2:?}";         shift 2 ;;
    --docker-version)   add_arg "DOCKER_VERSION=${2:?}";           shift 2 ;;
    --cuda-enable)      add_arg "CUDA_ENABLE=${2:?}";              shift 2 ;;
    --gst-tests)        add_arg "GST_TESTS=${2:?}";                shift 2 ;;
    --build-arg)
        case "${2:?--build-arg needs KEY=VALUE}" in *=*) : ;; *) die "--build-arg expects KEY=VALUE, got '$2'";; esac
        add_arg "$2"; shift 2 ;;

    # --toolchain local|registry (default local).
    #
    #   local     build the toolchain stage in-line, exactly as this script always
    #             did. The right choice on a dev box iterating on a pin, and the
    #             only choice when no artefact for the current pins exists yet.
    #   registry  resolve deploy/toolchain-hash.sh and pass
    #             TOOLCHAIN_IMAGE=<registry>/quasar-gst-toolchain:<hash>, so the ~40 min
    #             GStreamer compile becomes a pull. FAILS LOUDLY when that tag is
    #             not published -- silently falling back to a local build is how you
    #             end up waiting 40 minutes for something you asked to be instant.
    --toolchain)          TOOLCHAIN_MODE="${2:?}";     shift 2 ;;
    --toolchain-registry) TOOLCHAIN_REGISTRY="${2:?}"; shift 2 ;;

    --git-ref)          GIT_REF="${2:?}";       shift 2 ;;
    --worktree-dir)     WORKTREE_DIR="${2:?}";  shift 2 ;;

    --tag-suffix)       TAG_SUFFIX="${2:?}";    shift 2 ;;
    --registry)         REGISTRY="${2:?}";      shift 2 ;;
    --push)             DO_PUSH=1;              shift ;;
    --no-latest)        NO_LATEST=1;            shift ;;
    --no-prune)         NO_PRUNE=1;             shift ;;
    --keep)             KEEP="${2:?}";          shift 2 ;;
    # Retired with the NVIDIA lineage it aliased (#545). Accepted-and-ignored
    # rather than rejected: it may still be in a host's shell history or a cron
    # line, and failing a whole image build over a now-meaningless flag is worse
    # than saying so.
    --compat-vulkan-alias) warn "--compat-vulkan-alias is retired (#545): there is one agent lineage now, so quasar-vulkan is simply the transition alias of quasar-node-agent. Ignoring."; shift ;;
    --no-legacy-alias)  LEGACY_ALIAS=0;         shift ;;

    --no-validate)      DO_VALIDATE=0;          shift ;;
    --gpu)              GPU_FLAG="--gpu";       shift ;;
    --no-gpu)           GPU_FLAG="--no-gpu";    shift ;;
    --contract)         CONTRACT="${2:?}";      shift 2 ;;
    --require-element)  REQUIRE_ELEMENTS+=("${2:?}"); shift 2 ;;

    --cache)            BUILD_CACHE="${2:?}";      shift 2 ;;
    --cache-mode)       BUILD_CACHE_MODE="${2:?}"; shift 2 ;;
    --builder)          BUILDX_BUILDER="${2:?}";   shift 2 ;;

    --report)           REPORT="${2:?}";        shift 2 ;;
    --min-free-gb)      MIN_FREE_GB="${2:?}";   shift 2 ;;
    --prune-cache)      PRUNE_CACHE=1;          shift ;;
    --deploy)           DO_DEPLOY=1;            shift ;;
    --dry-run)          DRY_RUN=1;              shift ;;
    --no-cache)         NO_CACHE=1;             shift ;;
    -h|--help)          usage; exit 0 ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

[ "${#ROLES[@]}" -gt 0 ] || ROLES=(runtime control)
[ "$BASE_IMAGE_SET" = 1 ] || add_arg "QUASAR_BASE_IMAGE=$BASE_IMAGE_DEFAULT"

# De-duplicate, then order so a base image is built before anything that layers on it.
order_of() { case "$1" in toolchain) echo 0 ;; runtime) echo 1 ;; dev) echo 3 ;; profiling) echo 4 ;; control) echo 5 ;; updater) echo 6 ;; esac; }
# Plain while-read rather than `mapfile`: mapfile is bash 4+, and this script should
# stay parseable/runnable on any bash the operator happens to have.
_ordered=()
while read -r r; do [ -n "$r" ] && _ordered+=("$r"); done < <(
  printf '%s\n' "${ROLES[@]}" | awk '!seen[$0]++' \
    | while read -r r; do printf '%s %s\n' "$(order_of "$r")" "$r"; done \
    | sort -n | cut -d' ' -f2
)
ROLES=("${_ordered[@]}")

# --deploy only means anything for the agent image. Refuse rather than silently no-op:
# a `--deploy` that quietly did nothing is how the wrong-tag trap it exists to catch
# would come back.
if [ "$DO_DEPLOY" = 1 ]; then
  case " ${ROLES[*]} " in
    *" runtime "*) : ;;
    *) die "--deploy recreates and verifies this host's quasar-node-agent, so it needs the 'runtime' role in the build (got: ${ROLES[*]})" ;;
  esac
  [ "$DRY_RUN" = 0 ] || die "--deploy and --dry-run are mutually exclusive: there would be no built image to deploy"
fi

# ── Build cache: resolve and validate BEFORE anything is built ────────────────
# Every failure here is a misconfiguration that would otherwise present as "the cache
# just never hits" — a silent no-op is the one outcome this script exists to prevent,
# so each one is a hard refusal with the fix in the message.
case "$BUILD_CACHE_MODE" in
  rw|ro|wo) : ;;
  *) die "--cache-mode expects rw, ro or wo; got '$BUILD_CACHE_MODE'" ;;
esac

CACHE_KIND=off; CACHE_TARGET=""
case "$BUILD_CACHE" in
  off|"") CACHE_KIND=off ;;
  local:*)
    CACHE_KIND=local; CACHE_TARGET="${BUILD_CACHE#local:}"
    [ -n "$CACHE_TARGET" ] || die "--cache local:<dir> needs a directory"
    # Relative paths are resolved against the CALLER's cwd, not the build context —
    # the build runs in a subshell that cd's into the context, and a relative dest
    # would land somewhere the caller never looks.
    case "$CACHE_TARGET" in /*) : ;; *) CACHE_TARGET="$PWD/$CACHE_TARGET" ;; esac ;;
  registry:*)
    CACHE_KIND=registry; CACHE_TARGET="${BUILD_CACHE#registry:}"
    [ -n "$CACHE_TARGET" ] || die "--cache registry:<ref> needs an image reference" ;;
  *) die "--cache expects off, local:<dir> or registry:<ref>; got '$BUILD_CACHE'" ;;
esac

command -v docker >/dev/null 2>&1 || die "docker is required"
command -v jq     >/dev/null 2>&1 || die "jq is required"
command -v git    >/dev/null 2>&1 || die "git is required"
[ -f "$CONTRACT" ] || die "contract not found: $CONTRACT"

# The builder must be able to do what was asked of it. `docker buildx build` on the
# default `docker` driver accepts --cache-to, warns, and exports NOTHING; a CI job
# wired that way looks green forever and never gets a cache hit.
if [ "$CACHE_KIND" != off ]; then
  command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1 \
    || die "--cache needs docker buildx (docker >= 23 or the buildx plugin)"
  BUILDER_DRIVER="$(
    if [ -n "$BUILDX_BUILDER" ]; then
      docker buildx inspect "$BUILDX_BUILDER" 2>/dev/null
    else
      docker buildx inspect 2>/dev/null
    fi | awk -F': *' '/^Driver:/ {print $2; exit}'
  )"
  [ -n "$BUILDER_DRIVER" ] || die "cannot inspect buildx builder '${BUILDX_BUILDER:-<current>}' — does it exist?"
  case "$BUILD_CACHE_MODE" in
    rw|wo)
      [ "$BUILDER_DRIVER" != docker ] || die \
"cache EXPORT was requested but buildx builder '${BUILDX_BUILDER:-<current>}' uses the 'docker' driver,
  which cannot export a cache. It would accept --cache-to and write nothing.
  Create a builder that can:
    docker buildx create --name quasar-cache --driver docker-container --use
  then re-run (or pass --builder quasar-cache / QUASAR_BUILDX_BUILDER=quasar-cache).
  To read an existing cache without writing one, use --cache-mode ro." ;;
  esac
  log "build cache: $CACHE_KIND ($BUILD_CACHE_MODE) → $CACHE_TARGET  [builder ${BUILDX_BUILDER:-<current>}, driver $BUILDER_DRIVER]"
fi

# Cache flags for one role. Scope is ALWAYS per-role: two roles sharing one scope
# means each build overwrites the other's manifest and neither is ever warm.
cache_flags_for() { # cache_flags_for <role>  -> prints one flag per line
  local role="$1"
  [ "$CACHE_KIND" = off ] && return 0
  local from_spec="" to_spec=""
  case "$CACHE_KIND" in
    local)
      from_spec="type=local,src=$CACHE_TARGET/$role"
      to_spec="type=local,dest=$CACHE_TARGET/$role,mode=max" ;;
    registry)
      # `<prefix>/<image>:buildcache` — deliberately the SAME refs
      # .github/workflows/images.yml already writes with docker/build-push-action
      # (ghcr.io/<repo>/quasar-node-agent:buildcache and friends). Inventing a second
      # naming scheme would mean CI populates one cache and this script reads
      # another, and neither would ever be warm for the other.
      local cache_ref="$CACHE_TARGET/$(role_image "$role"):buildcache"
      from_spec="type=registry,ref=$cache_ref"
      to_spec="type=registry,ref=$cache_ref,mode=max" ;;
  esac
  # A local cache dir that does not exist yet is normal on the first run; buildx
  # errors on a missing src, so only offer it once there is something to read.
  case "$BUILD_CACHE_MODE" in
    rw|ro)
      if [ "$CACHE_KIND" = local ]; then
        [ -d "$CACHE_TARGET/$role" ] && printf -- '--cache-from\n%s\n' "$from_spec"
      else
        printf -- '--cache-from\n%s\n' "$from_spec"
      fi ;;
  esac
  case "$BUILD_CACHE_MODE" in
    rw|wo)
      [ "$CACHE_KIND" = local ] && mkdir -p "$CACHE_TARGET/$role"
      printf -- '--cache-to\n%s\n' "$to_spec" ;;
  esac
  return 0
}
[ -x "$SCRIPT_DIR/validate-image.sh" ] || chmod +x "$SCRIPT_DIR/validate-image.sh" 2>/dev/null || true

if [ "$DO_PUSH" = 1 ]; then
  [ -n "$REGISTRY" ]   || die "--push requires --registry HOST/NAMESPACE"
  [ "$DO_VALIDATE" = 1 ] || die "--push with --no-validate is refused: an unvalidated image must never be published"
fi

# ── Role table ────────────────────────────────────────────────────────────────
role_dockerfile() { case "$1" in
  toolchain|runtime|dev|profiling) echo "deploy/Dockerfile.vulkan" ;;
  control)                  echo "deploy/Dockerfile.control.prod" ;;
  updater)                  echo "deploy/Dockerfile.updater" ;;
esac; }
role_target() { case "$1" in
  toolchain) echo toolchain ;;
  runtime) echo runtime ;; dev) echo dev ;; profiling) echo profiling ;;
  # Dockerfile.control.prod has a single unnamed final stage — no --target applies.
  control) echo "" ;;
  updater) echo runtime ;;
esac; }
# Images are named for the ROLE they play, not for the technology that happens to
# be inside them (2026-08-26 rename). The NVIDIA lineage that used to sit here was
# never renamed — #545 retired it instead, so the name never had to be minted.
role_image() { case "$1" in
  toolchain) echo quasar-gst-toolchain ;;
  runtime) echo quasar-node-agent ;;
  dev) echo quasar-agent-dev ;; control) echo quasar-control-plane ;;
  profiling) echo quasar-profiling ;;
  updater) echo quasar-updater ;;
esac; }
# The pre-rename name for a role, or nothing. Written as an extra LOCAL tag on the
# same image id so an un-migrated deploy/.env, a hand-typed `docker run`, or a
# runtime host that has not re-read compose keeps resolving through the transition.
# REMOVAL CONDITION: delete this function and its call site after the first release
# that ships the new names, plus one more release. Same window as the registry-side
# aliases in .github/workflows/images.yml — move both together or neither.
role_legacy_image() { case "$1" in
  toolchain) echo quasar-toolchain ;;
  runtime) echo quasar-vulkan ;;
  dev) echo quasar-dev ;; control) echo quasar-control ;;
  *) echo "" ;;
esac; }
# Roles that are shipped artifacts: contract-validated, :latest-promotable, pushable.
# `profiling` is the only one that is not, and it must never become one silently — the
# check is a function rather than an inline test so there is exactly one definition of
# "does this role ship".
role_ships() { case "$1" in profiling) return 1 ;; *) return 0 ;; esac; }
# The toolchain is a BUILD INPUT, not a deployed image: no agent binary, no
# entrypoint, no runtime role, so image-contract.json has nothing to say about it
# and validate-image.sh is skipped for it. It is still pushable -- publishing it is
# the entire point.
role_contract_checked() { case "$1" in toolchain) return 1 ;; *) return 0 ;; esac; }

# Loud refusal rather than a silent per-role skip: someone typing --push with a
# non-shipping role in the list has misunderstood what that image is, and finding out
# afterwards from a registry listing is worse than finding out now.
if [ "$DO_PUSH" = 1 ]; then
  for r in "${ROLES[@]}"; do
    role_ships "$r" || die "role '$r' can never be pushed: it is an unvalidated, non-shipped diagnostic image. Drop it from the role list, or drop --push."
  done
fi

# ── Source tree: working checkout, or a throwaway worktree at --git-ref ────────
# Scratch lives under the repo, never /tmp: on the unraid box /tmp is a ramdisk and
# the operator rule is that every file this project creates stays under
# /mnt/user/appdata/quasar. .build-tmp is gitignored.
# PER-RUN subdirectory, not a shared one. A shared .build-tmp is removed by whichever
# invocation exits first — a `--dry-run` probe run alongside a real 6-minute build deleted
# the directory out from under it and the build died at `mktemp: No such file or
# directory` after the image had already been produced. Concurrent invocations are normal
# (checking a flag while a build runs), so each run owns its own scratch.
BUILD_TMP_ROOT="$REPO_ROOT/.build-tmp"
BUILD_TMP="$BUILD_TMP_ROOT/run-$$"
BUILD_CONTEXT="$REPO_ROOT"
WORKTREE_CREATED=""
cleanup() {
  if [ -n "$WORKTREE_CREATED" ]; then
    log "removing build worktree $WORKTREE_CREATED"
    git -C "$REPO_ROOT" worktree remove --force "$WORKTREE_CREATED" 2>/dev/null \
      || rm -rf "$WORKTREE_CREATED"
    git -C "$REPO_ROOT" worktree prune 2>/dev/null || true
  fi
  rm -rf "$BUILD_TMP" 2>/dev/null || true
  # Only succeeds when no other run still owns a subdirectory — never force it.
  rmdir "$BUILD_TMP_ROOT" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
mkdir -p "$BUILD_TMP"

if [ -n "$GIT_REF" ]; then
  git -C "$REPO_ROOT" rev-parse --verify --quiet "$GIT_REF^{commit}" >/dev/null \
    || die "--git-ref '$GIT_REF' is not a commit/branch/tag in this repo (fetch first?)"
  mkdir -p "$BUILD_TMP"
  WORKTREE_DIR="${WORKTREE_DIR:-$(mktemp -d "$BUILD_TMP/worktree-XXXXXX")}"
  # A detached worktree: no branch is checked out, so this can never move a branch ref
  # or disturb the working tree you are editing.
  log "creating detached build worktree at $GIT_REF → $WORKTREE_DIR"
  git -C "$REPO_ROOT" worktree add --detach --force "$WORKTREE_DIR" "$GIT_REF" >/dev/null
  WORKTREE_CREATED="$WORKTREE_DIR"
  BUILD_CONTEXT="$WORKTREE_DIR"
fi

SRC_SHA="$(git -C "$BUILD_CONTEXT" rev-parse HEAD 2>/dev/null || echo unknown)"
SRC_DIRTY=false
if [ -z "$GIT_REF" ] && ! git -C "$BUILD_CONTEXT" diff --quiet HEAD 2>/dev/null; then
  SRC_DIRTY=true
  warn "working tree is DIRTY — the built image will not correspond to commit $SRC_SHA."
  warn "use --git-ref <ref> to build a clean, reproducible tree."
fi

# Provenance for the image labels (§4). SOURCE_COMMIT is the tree actually built —
# with --git-ref that is the worktree's commit, not the caller's HEAD.
#
# Deliberately NOT folded into the shared EXTRA_ARGS list: EXTRA_ARGS is passed to
# EVERY role's docker build, including the toolchain, which declares neither and
# would die at the undeclared-ARG guard below. Attached per-role instead, only to
# the Dockerfiles that declare them (Dockerfile.vulkan's runtime stage, and
# Dockerfile.control.prod since #107, which needs the same values BOTH as image
# labels and as the -ldflags identity stamps the binary serves).
PROVENANCE_ARGS=("SOURCE_COMMIT=$SRC_SHA" "BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)")

# The control image's org.quasar.schema.version: the highest NNNN_*.up.sql the
# binary embeds. Twin of internal/buildinfo.HighestMigration — the label and the
# served identity must be the same number. "unknown" when the directory cannot be
# read; the edge channel then skips the build rather than guessing (#111).
highest_migration() {
  local dir="$BUILD_CONTEXT/control-plane/migrations" n
  n="$(find "$dir" -maxdepth 1 -name '*.up.sql' -printf '%f\n' 2>/dev/null \
        | sed -n 's/^0*\([0-9][0-9]*\)_.*\.up\.sql$/\1/p' | sort -n | tail -1)"
  printf '%s' "${n:-unknown}"
}
# The SPA bakes in the ref it was built from (web/vite.config.ts) for the
# enroll-host one-liner (#100): the exact tag when the tree sits on one, else the
# commit. Declared only by Dockerfile.control.prod, so attached per role below.
SRC_REF="$(git -C "$BUILD_CONTEXT" describe --tags --exact-match 2>/dev/null || echo "$SRC_SHA")"

# ── Validate every override against the ARGs the Dockerfile actually declares ──
# A --build-arg for an undeclared ARG is silently ignored by Docker. That is exactly
# how a pin can appear to have been applied when it never was.
declared_args() { # declared_args <dockerfile>
  grep -Eo '^[[:space:]]*ARG[[:space:]]+[A-Za-z_][A-Za-z0-9_]*' "$1" \
    | awk '{print $2}' | sort -u
}
check_args_declared() { # check_args_declared <role> <dockerfile-abs> <KEY=VALUE>...
  local role="$1" df="$2"; shift 2
  local bad=() k
  local declared; declared="$(declared_args "$df")"
  for kv in "$@"; do
    [ -n "$kv" ] || continue
    k="${kv%%=*}"
    printf '%s\n' "$declared" | grep -qx "$k" || bad+=("$k")
  done
  if [ "${#bad[@]}" -gt 0 ]; then
    printf '\nbuild-images: these build args are NOT declared as ARG in %s (role %s):\n' "$df" "$role" >&2
    printf '  - %s\n' "${bad[@]}" >&2
    printf '\nDocker would ignore them silently. Declare the ARG in the Dockerfile, or drop the flag.\n' >&2
    printf 'Declared ARGs are:\n' >&2
    printf '%s\n' "$declared" | sed 's/^/  /' >&2
    exit 2
  fi
}

# -- Pins: one file, one declaration, checked both ways ------------------------
#
# deploy/pins.env is the source of truth for every third-party pin. Dockerfile.vulkan
# declares each one exactly ONCE, at global scope (before the first FROM), and stages
# that need to read a pin re-declare it BARE so it inherits that global default.
#
# What this replaced, and why the replacement is stricter rather than looser: until
# 2026-08-20 the four compositor/codec pins were declared TWICE in the Dockerfile --
# build stage and runtime stage -- and a Python guard here asserted the two copies
# agreed and that there were exactly two of them. That guard was load-bearing
# precisely because the duplication existed; DOCKER_VERSION and CUDA_PKG_VERSION were
# duplicated with no guard at all and could drift silently. Removing the duplication
# removes the failure mode, so the guard is now the stronger pair of assertions:
#
#   1. every pin in pins.env is declared EXACTLY ONCE in the Dockerfile, and
#   2. the Dockerfile's default and pins.env's value AGREE.
#
# (1) catches a re-pin that adds a second declaration and reintroduces drift; (2)
# catches a re-pin that edits one file and forgets the other. Neither is relaxable:
# a mismatch means the image's provenance LABELs would describe something other than
# what was built, which is the exact opacity that let Tower and hermes drift under
# one tag in July.
check_pins_agree() { # check_pins_agree <dockerfile-abs> <pins-env-abs>
  python3 - "$1" "$2" <<'PYEOF'
import re
import sys

df_path, pins_path = sys.argv[1], sys.argv[2]

pins = {}
for lineno, raw in enumerate(open(pins_path), 1):
    line = raw.strip()
    if not line or line.startswith('#'):
        continue
    if '=' not in line:
        sys.stderr.write("build-images: %s:%d is neither blank, a comment, nor "
                         "KEY=VALUE: %r\n" % (pins_path, lineno, line))
        sys.exit(2)
    k, v = line.split('=', 1)
    pins[k.strip()] = v.strip()

text = open(df_path).read()
declared = {}
for m in re.finditer(r'^[ \t]*ARG[ \t]+([A-Za-z_][A-Za-z0-9_]*)=(\S+)', text, re.M):
    declared.setdefault(m.group(1), []).append(m.group(2))

bad = False
for name, want in sorted(pins.items()):
    got = declared.get(name, [])
    if not got:
        bad = True
        sys.stderr.write(
            "build-images: %s sets %s but %s never declares it with a default. "
            "Every pin must have exactly one global `ARG %s=<value>` before the "
            "first FROM.\n" % (pins_path, name, df_path, name))
        continue
    if len(got) > 1:
        bad = True
        sys.stderr.write(
            "build-images: ARG %s has %d declarations WITH DEFAULTS in %s (%s). "
            "Pins are declared once, at global scope; a stage that needs to read "
            "one re-declares it BARE (`ARG %s`) and inherits that default. Two "
            "defaults is the drift this check exists to stop -- delete the extra "
            "one, do not relax this.\n"
            % (name, len(got), df_path, ', '.join(got), name))
        continue
    if got[0] != want:
        bad = True
        sys.stderr.write(
            "build-images: pin %s disagrees between the two files:\n"
            "  %s: %s\n  %s: %s\n"
            "pins.env is the source of truth. Edit it, then mirror the value into "
            "the Dockerfile's global ARG block.\n"
            % (name, pins_path, want, df_path, got[0]))

sys.exit(2 if bad else 0)
PYEOF
}

# Dockerfile.control.prod is outside check_pins_agree's remit: it declares exactly ONE
# pin (QUASAR_BASE_IMAGE) and none of the other eight, so the "every pin in pins.env is
# declared here" half of that check cannot apply to it. The half that CAN apply still
# must — the control-plane image's base is the same base, and it is the one that got
# left behind at the old org while the agent lineage moved. Same rule, one key.
check_base_arg_agrees() { # check_base_arg_agrees <dockerfile-abs>
  local df="$1" want got n
  want="$(pin_value QUASAR_BASE_IMAGE)" || return 2
  # Only ARG declarations WITH a default; a bare `ARG QUASAR_BASE_IMAGE` inside a stage
  # inherits the global one and is exactly the pattern we want (it is not a second
  # source of truth), so it must not be counted here.
  n="$(grep -cE '^[[:space:]]*ARG[[:space:]]+QUASAR_BASE_IMAGE=' "$df" || true)"
  [ "$n" = 1 ] || {
    warn "$df has $n global \`ARG QUASAR_BASE_IMAGE=<value>\` declarations; expected exactly 1"
    return 2
  }
  got="$(grep -E '^[[:space:]]*ARG[[:space:]]+QUASAR_BASE_IMAGE=' "$df" | head -1 | cut -d= -f2-)"
  [ "$got" = "$want" ] || {
    warn "base image disagrees between the two files:"
    warn "  $QUASAR_PINS_FILE: $want"
    warn "  $df: $got"
    warn "pins.env is the source of truth. Edit it, then mirror the value into the Dockerfile ARG."
    return 2
  }
}

# ── Pre-flight disk ───────────────────────────────────────────────────────────
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)"
# Prints free GB on the docker root, or NOTHING when it cannot be measured from here.
# On Docker Desktop / OrbStack the daemon's root (/var/lib/docker) lives inside the
# Linux VM and does not exist on the Mac, so `df` fails. Under `set -euo pipefail` a
# failing `df | awk` inside `FREE="$(free_gb)"` used to abort the whole script with the
# only stderr already suppressed — no build, no error, exit 1, and the dirty-tree
# warnings (printed just before) were the last thing on screen. Never let this probe
# be fatal: an unmeasurable disk is a warning + skipped floor, not a silent no-op build.
free_gb() {
  [ -d "$DOCKER_ROOT" ] || return 0
  df -Pk "$DOCKER_ROOT" 2>/dev/null | awk 'NR==2 {printf "%d", $4/1024/1024}' || true
}
FREE="$(free_gb)"
if [ -z "$FREE" ]; then
  warn "cannot measure free space on docker root $DOCKER_ROOT from this host (Docker Desktop /"
  warn "  OrbStack VM?) — skipping the ${MIN_FREE_GB}GB disk floor. Check space in the VM if a build fails."
else
  log "docker root $DOCKER_ROOT — ${FREE}GB free (floor ${MIN_FREE_GB}GB)"
fi
if [ "$PRUNE_CACHE" = 1 ]; then
  log "pruning builder cache (--prune-cache)${BUILDX_BUILDER:+ on builder $BUILDX_BUILDER}"
  # Scoped to the selected builder: each buildx builder owns its own cache, so an
  # unscoped prune from a run using a dedicated builder would wipe the DEFAULT
  # builder's cache and leave the one actually in use untouched.
  [ "$DRY_RUN" = 1 ] || docker buildx prune -af ${BUILDX_BUILDER:+--builder "$BUILDX_BUILDER"} >/dev/null
  FREE="$(free_gb)"; log "after prune: ${FREE:-?}GB free"
fi
if [ -n "$FREE" ] && [ "$FREE" -lt "$MIN_FREE_GB" ] && [ "$DRY_RUN" = 0 ]; then
  die "only ${FREE}GB free on $DOCKER_ROOT (need ${MIN_FREE_GB}GB). Free space or re-run with --prune-cache / --min-free-gb N."
fi

# ── Build ─────────────────────────────────────────────────────────────────────
# -- Toolchain artefact resolution --------------------------------------------
# Dockerfile.vulkan's `build` stage is `FROM ${TOOLCHAIN_IMAGE}`, whose default is
# the LOCAL stage name `toolchain`. Passing a registry reference instead makes
# BuildKit pull the pre-built /opt/gst rather than instantiate the stage, which is
# the difference between a ~5 minute agent-image build and a ~45 minute one.
case "$TOOLCHAIN_MODE" in
  local)
    log "toolchain: building locally (--toolchain registry to reuse a published artefact)" ;;
  registry)
    [ -x "$SCRIPT_DIR/toolchain-hash.sh" ] || chmod +x "$SCRIPT_DIR/toolchain-hash.sh" 2>/dev/null || true
    TOOLCHAIN_TAG="$(bash "$SCRIPT_DIR/toolchain-hash.sh")" \
      || die "could not compute the toolchain hash (deploy/toolchain-hash.sh)"
    TOOLCHAIN_REF="$TOOLCHAIN_REGISTRY/quasar-gst-toolchain:$TOOLCHAIN_TAG"
    log "toolchain: resolving $TOOLCHAIN_REF"
    # Checked HERE rather than let `docker build` fail on the pull, so the error
    # names the artefact and the fix instead of surfacing as a generic manifest
    # error 40 lines into a build log.
    if [ "$DRY_RUN" = 0 ] && ! docker manifest inspect "$TOOLCHAIN_REF" >/dev/null 2>&1; then
      die "no published toolchain for the current pins: $TOOLCHAIN_REF
  The pins in deploy/pins.env (or a patch, or the toolchain stage) changed, so the
  artefact for this hash has never been built. Either:
    build and publish it:  deploy/build-images.sh toolchain --registry $TOOLCHAIN_REGISTRY --push --tag-suffix $TOOLCHAIN_TAG
    or build it in-line:   drop --toolchain registry (costs ~40 min once)"
    fi
    add_arg "TOOLCHAIN_IMAGE=$TOOLCHAIN_REF" ;;
  *) die "--toolchain expects 'local' or 'registry', got '$TOOLCHAIN_MODE'" ;;
esac

TS="$(date -u +%Y%m%d-%H%M)"
GEN_TAG="$TS"; [ -n "$TAG_SUFFIX" ] && GEN_TAG="$TS-$TAG_SUFFIX"

RESULTS_JSON="[]"
OVERALL_RC=0

record() { # record <role> <image> <tag> <id> <size_mb> <verdict> <seconds> <validation_json_or_null>
  RESULTS_JSON="$(jq \
    --arg role "$1" --arg image "$2" --arg tag "$3" --arg id "$4" \
    --argjson size "$5" --arg verdict "$6" --argjson secs "$7" --argjson val "$8" \
    --arg df "$(role_dockerfile "$1")" --arg target "$(role_target "$1")" \
    --argjson args "$(printf '%s\n' "${ROLE_ARGS[@]:-}" | jq -Rs 'split("\n")|map(select(length>0))')" \
    '. + [{role:$role, image:$image, tag:$tag, image_id:$id, size_mb:$size,
           dockerfile:$df, target:$target, build_args:$args,
           build_seconds:$secs, verdict:$verdict, validation:$val}]' \
    <<<"$RESULTS_JSON")"
}

for role in "${ROLES[@]}"; do
  DF_REL="$(role_dockerfile "$role")"
  DF_ABS="$BUILD_CONTEXT/$DF_REL"
  TARGET="$(role_target "$role")"
  IMG="$(role_image "$role")"
  [ -f "$DF_ABS" ] || die "dockerfile missing for role $role: $DF_ABS"

  # SOURCE_COMMIT/BUILT_AT ride along only for the Dockerfile that declares them
  # (see the PROVENANCE_ARGS comment above) — every other role gets EXTRA_ARGS
  # unchanged, identical to before this feature existed.
  # The toolchain image is tagged by its CONTENT, not by wall-clock time: its
  # identity IS the hash of its inputs, that is what makes "is it already
  # published?" answerable with `docker manifest inspect` and what makes the tag
  # meaningful six months from now. A timestamp tag would publish a new artefact
  # on every build and defeat the whole reuse scheme.
  ROLE_TAG="$GEN_TAG"
  if [ "$role" = "toolchain" ]; then
    ROLE_TAG="$(bash "$SCRIPT_DIR/toolchain-hash.sh")" \
      || die "could not compute the toolchain hash (deploy/toolchain-hash.sh)"
    log "toolchain content tag: $ROLE_TAG"
  fi

  ROLE_ARGS=("${EXTRA_ARGS[@]:-}")
  # The updater is the one role not built on the quasar-base family (its base is
  # the upstream docker CLI image), so it declares no QUASAR_BASE_IMAGE ARG and
  # the shared default must not be handed to it. An EXPLICIT --base-image is a
  # misunderstanding worth saying out loud rather than dropping quietly.
  if [ "$role" = updater ]; then
    [ "$BASE_IMAGE_SET" = 1 ] && warn "role 'updater' is not built on quasar-base; --base-image does not apply to it and is ignored for this role."
    _filtered=()
    for kv in "${ROLE_ARGS[@]:-}"; do
      case "$kv" in QUASAR_BASE_IMAGE=*) : ;; *) [ -n "$kv" ] && _filtered+=("$kv") ;; esac
    done
    ROLE_ARGS=("${_filtered[@]:-}")
  fi
  [ "$DF_REL" = "deploy/Dockerfile.vulkan" ] && ROLE_ARGS+=("${PROVENANCE_ARGS[@]}")
  [ "$DF_REL" = "deploy/Dockerfile.updater" ] && ROLE_ARGS+=("${PROVENANCE_ARGS[@]}")
  if [ "$DF_REL" = "deploy/Dockerfile.control.prod" ]; then
    ROLE_ARGS+=("${PROVENANCE_ARGS[@]}")
    ROLE_ARGS+=("SCHEMA_VERSION=$(highest_migration)")
    [ "$SRC_REF" != unknown ] && ROLE_ARGS+=("QUASAR_SOURCE_REF=$SRC_REF")
    # The served semver, and only from a real `vX.Y.Z` tag. A branch build has no
    # version and must say so ("dev") rather than borrow the last tag's number.
    case "$SRC_REF" in
      v[0-9]*) ROLE_ARGS+=("QUASAR_VERSION=${SRC_REF#v}") ;;
    esac
  fi

  [ "$DF_REL" = "deploy/Dockerfile.vulkan" ] && { check_pins_agree "$DF_ABS" "$PINS_FILE" || exit 2; }
  [ "$DF_REL" = "deploy/Dockerfile.control.prod" ] && { check_base_arg_agrees "$DF_ABS" || exit 2; }
  check_args_declared "$role" "$DF_ABS" "${ROLE_ARGS[@]:-}"

  # `docker buildx build` rather than `docker build`. On the default `docker` driver
  # the two are the same builder and the same result; buildx is what makes
  # --cache-from/--cache-to and an alternate builder expressible at all, and #510
  # moves the shipped builds to GitHub Actions where the external cache is the
  # difference between a 5-minute job and a 45-minute one.
  #
  # --load is explicit and unconditional. On the docker driver it is what already
  # happens; on a docker-container driver the image otherwise stays inside the
  # builder and never reaches the local image store — which would break the very
  # next steps here (docker image inspect, validate-image.sh, the :latest tag). A
  # build whose artifact cannot be validated must not be possible to ask for.
  BUILD_CMD=(docker buildx build -f "$DF_REL" --load)
  [ -n "$BUILDX_BUILDER" ] && BUILD_CMD+=(--builder "$BUILDX_BUILDER")
  [ -n "$TARGET" ] && BUILD_CMD+=(--target "$TARGET")
  [ "$NO_CACHE" = 1 ] && BUILD_CMD+=(--no-cache)
  while IFS= read -r f; do [ -n "$f" ] && BUILD_CMD+=("$f"); done < <(cache_flags_for "$role")
  for kv in "${ROLE_ARGS[@]:-}"; do [ -n "$kv" ] && BUILD_CMD+=(--build-arg "$kv"); done
  BUILD_CMD+=(-t "$IMG:$ROLE_TAG" .)

  if [ "$DRY_RUN" = 1 ]; then
    printf '\n# role %s → %s:%s\n( cd %s && %s )\n' \
      "$role" "$IMG" "$ROLE_TAG" "$BUILD_CONTEXT" "$(printf '%q ' "${BUILD_CMD[@]}")"
    continue
  fi

  log "══ building role '$role' → $IMG:$ROLE_TAG  ($DF_REL${TARGET:+ --target $TARGET}) ══"
  START=$(date +%s)
  ( cd "$BUILD_CONTEXT" && "${BUILD_CMD[@]}" ) || {
    warn "BUILD FAILED for role '$role'"
    record "$role" "$IMG" "$ROLE_TAG" "" 0 "build-failed" 0 null
    OVERALL_RC=1
    continue
  }
  ELAPSED=$(( $(date +%s) - START ))
  IMG_ID="$(docker image inspect --format '{{.Id}}' "$IMG:$ROLE_TAG")"
  SIZE_MB=$(( $(docker image inspect --format '{{.Size}}' "$IMG:$ROLE_TAG") / 1024 / 1024 ))
  log "built $IMG:$ROLE_TAG — ${SIZE_MB}MB in ${ELAPSED}s"

  # ── Validate before ANY promotion ───────────────────────────────────────────
  # A non-shipping role has no contract entry by design (PROF-02: the profiling image is
  # deliberately bigger than the shipped one — it carries line tables and samply — so
  # measuring it against the shipped-image assertions would be meaningless, and adding a
  # role to the contract to make that work would be exactly the "relax the contract"
  # move CLAUDE.md forbids). It is therefore never validated, never promoted, never
  # pushed, and its verdict in the build report stays the honest "built".
  ROLE_VALIDATE="$DO_VALIDATE"
  if ! role_contract_checked "$role"; then
    ROLE_VALIDATE=0
    log "role '$role' is a build input, not a deployed image: image-contract.json does not describe it, so no contract check."
  fi
  ROLE_PROMOTE=$([ "$NO_LATEST" = 0 ] && echo 1 || echo 0)
  if ! role_ships "$role"; then
    ROLE_VALIDATE=0
    ROLE_PROMOTE=0
    warn "role '$role' is a non-shipping diagnostic image: no contract check, no :latest, no push."
    warn "  Use it by explicit tag: $IMG:$ROLE_TAG (see docs/profiling-rust.md)."
  fi

  VAL_JSON="null"; VERDICT="built"
  if [ "$ROLE_VALIDATE" = 1 ]; then
    mkdir -p "$BUILD_TMP"
    VAL_FILE="$(mktemp "$BUILD_TMP/validate-XXXXXX.json")"
    VAL_ARGS=(--image "$IMG:$ROLE_TAG" --role "$role" --contract "$CONTRACT" --json "$VAL_FILE")
    [ -n "$GPU_FLAG" ] && VAL_ARGS+=("$GPU_FLAG")
    for e in "${REQUIRE_ELEMENTS[@]:-}"; do [ -n "$e" ] && VAL_ARGS+=(--require-element "$e"); done
    if bash "$SCRIPT_DIR/validate-image.sh" "${VAL_ARGS[@]}"; then
      VERDICT="pass"
    else
      VERDICT="contract-violation"
      OVERALL_RC=1
      warn "role '$role' VIOLATES its contract — $IMG:$ROLE_TAG kept for inspection,"
      warn "  :latest NOT promoted and nothing pushed. Fix the image, do not relax the contract."
    fi
    [ -s "$VAL_FILE" ] && VAL_JSON="$(cat "$VAL_FILE")"
    rm -f "$VAL_FILE"
  fi

  record "$role" "$IMG" "$ROLE_TAG" "$IMG_ID" "$SIZE_MB" "$VERDICT" "$ELAPSED" "$VAL_JSON"

  if [ "$VERDICT" = "contract-violation" ]; then
    continue
  fi

  # ── Promote ─────────────────────────────────────────────────────────────────
  if [ "$ROLE_PROMOTE" = 1 ]; then
    if docker image inspect "$IMG:latest" >/dev/null 2>&1; then
      OLD_ID="$(docker image inspect --format '{{.Id}}' "$IMG:latest")"
      if [ "$OLD_ID" != "$IMG_ID" ]; then
        docker tag "$IMG:latest" "$IMG:prev"
        log "rollback tag $IMG:prev → ${OLD_ID:7:12}"
      fi
    fi
    docker tag "$IMG:$ROLE_TAG" "$IMG:latest"
    log "promoted $IMG:latest → ${IMG_ID:7:12}"
  fi

  # ── Transition alias: the pre-rename name for this role ─────────────────────
  # Deliberately AFTER the :latest promotion. Before #545 an opt-in flag could still
  # re-point `quasar-vulkan` at the NVIDIA image after this ran; with one lineage
  # this is the only writer. See role_legacy_image() for the removal condition.
  LEGACY_IMG="$(role_legacy_image "$role")"
  if [ "$LEGACY_ALIAS" = 1 ] && [ -n "$LEGACY_IMG" ]; then
    docker tag "$IMG:$ROLE_TAG" "$LEGACY_IMG:$ROLE_TAG"
    [ "$ROLE_PROMOTE" = 1 ] && docker tag "$IMG:$ROLE_TAG" "$LEGACY_IMG:latest"
    log "legacy alias $LEGACY_IMG:$ROLE_TAG$([ "$ROLE_PROMOTE" = 1 ] && echo ' + :latest') → $IMG (deprecated name, --no-legacy-alias to skip)"
  fi

  # (--compat-vulkan-alias lived here: it re-pointed quasar-vulkan at the NVIDIA
  # image, for hosts whose deploy/.env still named that tag. With one agent lineage
  # there is nothing to cross-tag — `quasar-vulkan` is the runtime role's own
  # transition alias, written above.)

  if [ "$DO_PUSH" = 1 ] && role_ships "$role"; then
    REMOTE="$REGISTRY/$IMG"
    docker tag "$IMG:$ROLE_TAG" "$REMOTE:$ROLE_TAG"
    docker push "$REMOTE:$ROLE_TAG"
    if [ "$ROLE_PROMOTE" = 1 ]; then
      docker tag "$IMG:$ROLE_TAG" "$REMOTE:latest"
      docker push "$REMOTE:latest"
    fi
    log "pushed $REMOTE:$ROLE_TAG"
    # Mirror the local transition alias into the registry, so a puller pinned to
    # the pre-rename package keeps getting the same bytes. Same removal condition.
    if [ "$LEGACY_ALIAS" = 1 ] && [ -n "$LEGACY_IMG" ]; then
      LEGACY_REMOTE="$REGISTRY/$LEGACY_IMG"
      docker tag "$IMG:$ROLE_TAG" "$LEGACY_REMOTE:$ROLE_TAG"
      docker push "$LEGACY_REMOTE:$ROLE_TAG"
      if [ "$ROLE_PROMOTE" = 1 ]; then
        docker tag "$IMG:$ROLE_TAG" "$LEGACY_REMOTE:latest"
        docker push "$LEGACY_REMOTE:latest"
      fi
      log "pushed DEPRECATED alias $LEGACY_REMOTE:$ROLE_TAG"
    fi
  fi

  # ── Retention ───────────────────────────────────────────────────────────────
  if [ "$NO_PRUNE" = 0 ]; then
    docker images "$IMG" --format '{{.Tag}}' \
      | grep -E '^[0-9]{8}-[0-9]{4}(-.*)?$' | sort -r | tail -n "+$((KEEP + 1))" \
      | xargs -r -I{} docker rmi "$IMG:{}" >/dev/null 2>&1 || true
  fi
done

if [ "$DRY_RUN" = 1 ]; then
  printf '\n(dry run — nothing was built)\n\n'
  exit 0
fi

if [ "$NO_PRUNE" = 0 ]; then
  docker image prune -f >/dev/null 2>&1 || true
fi

# ── Report ────────────────────────────────────────────────────────────────────
jq -n \
  --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg host "$(hostname)" \
  --arg sha "$SRC_SHA" \
  --argjson dirty "$SRC_DIRTY" \
  --arg git_ref "${GIT_REF:-}" \
  --arg gen "$GEN_TAG" \
  --argjson validated "$([ "$DO_VALIDATE" = 1 ] && echo true || echo false)" \
  --arg cache "$BUILD_CACHE" \
  --arg cache_mode "$BUILD_CACHE_MODE" \
  --arg builder "${BUILDX_BUILDER:-}" \
  --argjson results "$RESULTS_JSON" \
  '{built_at:$ts, build_host:$host, source_commit:$sha, source_dirty:$dirty,
    git_ref:(if $git_ref=="" then null else $git_ref end),
    generation:$gen, validated:$validated,
    build_cache:{spec:$cache, mode:$cache_mode,
                 builder:(if $builder=="" then null else $builder end)},
    roles:$results}' > "$REPORT"

printf '\n══ summary ══\n'
jq -r '.roles[] | "  \(.verdict|ascii_upcase)\t\(.role)\t\(.image):\(.tag)\t\(.size_mb)MB\t\(.build_seconds)s"' \
  "$REPORT" | column -t -s $'\t' 2>/dev/null \
  || jq -r '.roles[] | "  \(.verdict) \(.role) \(.image):\(.tag) \(.size_mb)MB"' "$REPORT"
printf '\n  source: %s%s%s\n' "${SRC_SHA:0:12}" \
  "$([ "$SRC_DIRTY" = true ] && echo ' (DIRTY)' || echo '')" \
  "$([ -n "$GIT_REF" ] && echo " via --git-ref $GIT_REF" || echo '')"
printf '  report: %s\n\n' "$REPORT"

# ── --deploy: recreate this host's node-agent and prove it is what we just built ──
# Absorbed from deploy/build-images.sh (2026-08-27). Everything above is the build
# that script already delegated here; this is the part that was genuinely host-specific.
if [ "$DO_DEPLOY" = 1 ]; then
  [ "$OVERALL_RC" = 0 ] || die "--deploy skipped: the build did not pass (rc=$OVERALL_RC). Nothing was recreated."

  AGENT_IMG="$(role_image runtime)"
  NEW_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$AGENT_IMG:latest")"
  log "=== --deploy: $AGENT_IMG:latest = $NEW_IMAGE_ID ==="

  DEPLOY_COMPOSE=(-f "$REPO_ROOT/deploy/docker-compose.yml" -f "$REPO_ROOT/deploy/docker-compose.nvidia.yml")
  docker compose "${DEPLOY_COMPOSE[@]}" up -d --force-recreate --wait quasar-node-agent

  AGENT_CID="$(docker compose "${DEPLOY_COMPOSE[@]}" ps -q quasar-node-agent)"
  [ -n "$AGENT_CID" ] || { log "FAIL: quasar-node-agent container not found after up --force-recreate"; exit 1; }
  RUNNING_ID="$(docker inspect --format '{{.Image}}' "$AGENT_CID")"
  log "running container image id: $RUNNING_ID"

  # The check that would have caught the 2026-07-14 wrong-tag trap: the agent silently
  # ran a stale image while the fix was only built into the other tag.
  if [ "$RUNNING_ID" != "$NEW_IMAGE_ID" ]; then
    log "FAIL: running node-agent image ($RUNNING_ID) != freshly built image ($NEW_IMAGE_ID) — WRONG-TAG TRAP"
    exit 1
  fi
  log "PASS: running node-agent is the freshly built image"

  # The agent runs the BAKED binary (the compose `command:` override that exec'd a
  # workspace-compiled binary was removed 2026-07-26). Assert that, or a stale
  # bind-mounted target/release build could still be what is serving sessions.
  #
  # NB resolve the AGENT process, not PID 1: the service runs with docker `--init`, so
  # PID 1 is /usr/bin/docker-init (tini) and checking it would pass no matter what.
  EXE="$(docker exec "$AGENT_CID" sh -c \
          'for p in $(pgrep -f quasar-node-agent); do readlink -f /proc/$p/exe; done | grep -m1 quasar-node-agent' \
          2>/dev/null || true)"
  [ -n "$EXE" ] || { log "FAIL: no quasar-node-agent process found inside the container"; exit 1; }
  log "agent binary → $EXE"
  case "$EXE" in
    /usr/local/bin/quasar-node-agent)
      log "PASS: agent is running the image's baked binary" ;;
    *)
      log "FAIL: agent is running '$EXE', not the image's baked"
      log "      /usr/local/bin/quasar-node-agent. A compose 'command:' override exec'ing a"
      log "      workspace build has been re-introduced — see the note in"
      log "      deploy/docker-compose.nvidia.yml and the image-lineage spec."
      exit 1 ;;
  esac

  # Audio is the reason the image rework happened: a sidecar image with no `pulseaudio`
  # binary muted every session (streamed AND console-local) with only a WARN in the log.
  PULSE_IMAGE="$(docker exec "$AGENT_CID" printenv QUASAR_PULSE_IMAGE 2>/dev/null || echo '')"
  PULSE_IMAGE="${PULSE_IMAGE:-$AGENT_IMG:latest}"
  if docker run --rm --entrypoint sh "$PULSE_IMAGE" -c 'command -v pulseaudio' >/dev/null 2>&1; then
    log "PASS: QUASAR_PULSE_IMAGE=$PULSE_IMAGE has a pulseaudio daemon"
  else
    log "FAIL: QUASAR_PULSE_IMAGE=$PULSE_IMAGE has NO pulseaudio binary — every session"
    log "      on this host would be silent (streamed and console-local). Point it at an"
    log "      image built from the 'runtime' role."
    exit 1
  fi

  log "--deploy done"
fi

exit "$OVERALL_RC"
