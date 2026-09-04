#!/usr/bin/env bash
#
# validate-image.sh — assert a built Quasar image against its declared contract.
#
#   deploy/validate-image.sh --image quasar-node-agent:latest --role runtime [--gpu]
#
# WHY THIS EXISTS
#   On 2026-07-26 the production NVIDIA agent image was 7.33GB (84% build toolchain) AND
#   was missing the pulseaudio daemon, so every Tower session ran with silent audio.
#   Neither problem was detectable by anything in the repo: that image's Dockerfile stage
#   was a hand-copy of the `runtime` stage's package list, kept in step by discipline
#   alone. (The lineage itself is gone — #545 — but the guarantees are not.)
#   Issues #111 and #272 had BOTH been closed as having solved exactly these problems.
#   This script is the executable form of those guarantees. It runs on every build
#   (deploy/build-images.sh) and gates the :latest promotion and any push.
#
#   Spec: docs/design/plans/2026-07-26-image-lineage-consolidation-spec.md
#   Contract: deploy/image-contract.json  (roles, inheritance, every assertion)
#
# USAGE
#   deploy/validate-image.sh --image REF --role runtime|dev|control [OPTIONS]
#
#   --image REF          image to validate (tag, id, or name@sha256:...). Required.
#   --role NAME          contract role. Required.
#   --pull               docker pull REF before inspecting it. For validating an
#                        artifact that lives in a registry rather than in the local
#                        daemon (CI: the build pushed it, this job did not build it).
#   --require-digest-ref refuse any REF that is not exactly name@sha256:<64 hex>.
#                        A tag is mutable, so resolving one between the build and
#                        this check reopens the race that addressing the artifact by
#                        digest exists to close. Asserted before anything touches
#                        docker, so the refusal is testable without a daemon.
#   --gpu                attach the GPU and additionally assert `required_with_gpu`
#                        elements. Without it, GPU-gated assertions are SKIPPED (and
#                        reported as skipped, never as passed).
#   --no-gpu             force GPU checks off even if a GPU is present.
#   --require-element E  extra device-gated element to assert (repeatable). Use for
#                        host-specific names the contract can't fix, e.g.
#                        --require-element vah264enc on an AMD VCN box.
#   --contract FILE      default deploy/image-contract.json
#   --json OUT           write a machine-readable result document
#   --quiet              only print failures and the verdict
#   -h|--help
#
# EXIT CODES
#   0  every assertion passed (skipped GPU assertions do not fail)
#   1  at least one assertion failed
#   2  usage error / missing prerequisite / could not run the image
#
# NOTES
#   - All in-image assertions run in ONE container pass, not one container per check.
#   - GST_REGISTRY is always pointed at a throwaway path so the stale-registry trap
#     (a registry baked at build time with no GPU) can never mask a missing element.
#   - Never relax an assertion to make a build green. Fix the image.
#   - On an NVIDIA host healed by the Quasar-provisioned driver volume (#468), the
#     probe container is given that volume and the same loader-discovery env the
#     real app containers get, so a GPU element assertion measures the IMAGE and
#     not the host's driver completeness. Auto-detected by volume-name suffix;
#     override with QUASAR_NVIDIA_DRIVER_VOLUME_NAME=<volume>. (Distinct from the
#     runtime knob QUASAR_NVIDIA_DRIVER_VOLUME, which is an on/off switch.)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

IMAGE=""
ROLE=""
CONTRACT="$SCRIPT_DIR/image-contract.json"
JSON_OUT=""
QUIET=0
GPU_MODE="auto"
EXTRA_ELEMENTS=()
DO_PULL=0
REQUIRE_DIGEST_REF=0

die() { printf 'validate-image: %s\n' "$*" >&2; exit 2; }
usage() { sed -n '2,48p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --image)            IMAGE="${2:?--image needs a value}"; shift 2 ;;
    --role)             ROLE="${2:?--role needs a value}"; shift 2 ;;
    --contract)         CONTRACT="${2:?--contract needs a value}"; shift 2 ;;
    --json)             JSON_OUT="${2:?--json needs a value}"; shift 2 ;;
    --require-element)  EXTRA_ELEMENTS+=("${2:?--require-element needs a value}"); shift 2 ;;
    --pull)             DO_PULL=1; shift ;;
    --require-digest-ref) REQUIRE_DIGEST_REF=1; shift ;;
    --gpu)              GPU_MODE="on"; shift ;;
    --no-gpu)           GPU_MODE="off"; shift ;;
    --quiet)            QUIET=1; shift ;;
    -h|--help)          usage; exit 0 ;;
    *) die "unknown argument: $1 (try --help)" ;;
  esac
done

[ -n "$IMAGE" ] || die "--image is required"
[ -n "$ROLE" ]  || die "--role is required"

# Digest-reference form. Mirrors scripts/release/release-preflight.sh's
# exact_digest_ref(): `repo:tag@sha256:...` is legal Docker but carries a mutable tag
# component, and a candidate must be named by content alone. The optional `:NNNN` is a
# registry port, never a tag. Checked here — before the contract file, jq and docker
# prerequisites — so `--require-digest-ref` is exercisable on a host with no daemon.
DIGEST_REF_RE='^([a-z0-9][a-z0-9.-]*(:[0-9]+)?/)?([a-z0-9]+([._-][a-z0-9]+)*/)*[a-z0-9]+([._-][a-z0-9]+)*@sha256:[0-9a-f]{64}$'
if [ "$REQUIRE_DIGEST_REF" = 1 ] && ! printf '%s' "$IMAGE" | grep -Eq "$DIGEST_REF_RE"; then
  die "--require-digest-ref: --image must be exactly name@sha256:<64 lowercase hex> (no tag component), got: $IMAGE"
fi

[ -f "$CONTRACT" ] || die "contract not found: $CONTRACT"
command -v jq     >/dev/null 2>&1 || die "jq is required on the build host"
command -v docker >/dev/null 2>&1 || die "docker is required"

# The artifact under test may live only in a registry (CI validates what the build
# job pushed, in a separate job with a separate token). Pulling it here rather than
# in the caller keeps "what was validated" and "what was inspected" the same string.
if [ "$DO_PULL" = 1 ]; then
  docker pull --quiet "$IMAGE" >/dev/null \
    || die "could not pull $IMAGE (registry unreachable, not authenticated, or no such digest)"
fi

docker image inspect "$IMAGE" >/dev/null 2>&1 \
  || die "image not found locally: $IMAGE (build or pull it first; --pull does the pull)"

jq -e --arg r "$ROLE" '.roles[$r]' "$CONTRACT" >/dev/null 2>&1 \
  || die "role '$ROLE' is not defined in $CONTRACT"

# ── Resolve the inheritance chain (parent first, child last) ──────────────────
# A role that declares `inherits` must satisfy every assertion of its parent too, so a
# derived image cannot quietly drop something its parent promises. That is the structural
# fix for the 2026-07-26 divergence. No shipped role uses it today (the one that did was
# the NVIDIA lineage, retired by #545); the mechanism stays because the next derived image
# must not have to re-invent it.
CHAIN=()
_cursor="$ROLE"
_guard=0
while [ -n "$_cursor" ] && [ "$_cursor" != "null" ]; do
  # ${x[@]+"${x[@]}"}, not "${x[@]}": bash 3.2 (the /bin/bash macOS ships) treats an
  # empty array's "${x[@]}" as unset under `set -u` and aborts on the FIRST iteration.
  # Invisible on the Linux build hosts, fatal to any local run. Same guard as
  # scripts/verify.sh's GIT_MOUNT.
  CHAIN=("$_cursor" ${CHAIN[@]+"${CHAIN[@]}"})
  _guard=$((_guard + 1))
  [ "$_guard" -le 8 ] || die "inheritance cycle or excessive depth at role '$_cursor'"
  _cursor="$(jq -r --arg r "$_cursor" '.roles[$r].inherits // ""' "$CONTRACT")"
done

CHAIN_JSON="$(printf '%s\n' "${CHAIN[@]}" | jq -Rs 'split("\n")|map(select(length>0))')"

# Collect a contract array across the whole chain, de-duplicated.
# NB: pass the chain as --argjson, NOT jq --args — with --args jq treats every
# remaining argument as a positional string INCLUDING the filename, so it reads
# stdin instead of the contract and silently yields nothing.
collect() { # collect <jq-path-expression>
  local expr="$1"
  jq -r --argjson chain "$CHAIN_JSON" \
     "[ .roles as \$roles | \$chain[] | \$roles[.] | ${expr} // [] ] | add // [] | unique | .[]" \
     "$CONTRACT"
}
# Scalar from the most-derived role that defines it.
scalar() { # scalar <jq-path-expression>
  local expr="$1" i out=""
  for (( i=${#CHAIN[@]}-1; i>=0; i-- )); do
    out="$(jq -r --arg r "${CHAIN[$i]}" ".roles[\$r] | ${expr} // empty" "$CONTRACT")"
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
  done
  return 0
}

# ── Decide GPU mode ───────────────────────────────────────────────────────────
GPU_ARGS=()
GPU_ON=0
detect_gpu() {
  if command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
    printf 'nvidia'; return
  fi
  if [ -e /dev/dri/renderD128 ]; then printf 'dri'; return; fi
  printf 'none'
}
case "$GPU_MODE" in
  on)   GPU_KIND="$(detect_gpu)"; [ "$GPU_KIND" = none ] && die "--gpu requested but no GPU detected on this host"; GPU_ON=1 ;;
  off)  GPU_KIND="none" ;;
  auto) GPU_KIND="$(detect_gpu)"; [ "$GPU_KIND" != none ] && GPU_ON=1 ;;
esac
if [ "$GPU_ON" = 1 ]; then
  case "$GPU_KIND" in
    nvidia) GPU_ARGS=(--gpus all -e NVIDIA_DRIVER_CAPABILITIES=all) ;;
    dri)    GPU_ARGS=(--device /dev/dri --security-opt seccomp=unconfined) ;;
  esac
  # A box can have both (Tower: RTX 5090 + AMD iGPU). Add /dev/dri when present.
  if [ "$GPU_KIND" = nvidia ] && [ -e /dev/dri ]; then
    GPU_ARGS+=(--device /dev/dri --security-opt seccomp=unconfined)
  fi
fi

# ── NVIDIA driver volume (#468) ───────────────────────────────────────────────
# On a host whose NVIDIA driver was installed CUDA-only, the node-agent heals the
# gap by provisioning a named docker volume with the userspace graphics libraries
# (nvidia_volume.rs) and pointing app containers at it. The probe container must
# get the SAME treatment, or the Vulkan element assertions measure the HOST's
# driver completeness rather than the IMAGE's contents — which is what made
# `build-images.sh nv` on the factory box refuse to promote :latest even though
# the image was correct.
#
# This does NOT relax any assertion (the never-relax rule): the elements must
# still register. It gives the probe the same runtime environment the real
# workload gets, so the assertion measures the thing it names.
#
# On a host with a complete driver no volume exists, nothing is mounted, and this
# is a no-op — the healthy path is unchanged.
NVIDIA_VOLUME="${QUASAR_NVIDIA_DRIVER_VOLUME_NAME:-}"
if [ "$GPU_ON" = 1 ] && [ "$GPU_KIND" = nvidia ] && [ -z "$NVIDIA_VOLUME" ]; then
  # Compose scopes the volume by project (`<project>_quasar-nvidia-driver`), so
  # match the suffix rather than assuming a project name. An exact unscoped name
  # is also accepted (hand-created volumes, non-compose deployments).
  NVIDIA_VOLUME="$(docker volume ls --format '{{.Name}}' 2>/dev/null \
    | grep -E '(^|_)quasar-nvidia-driver$' | head -1 || true)"
fi
if [ -n "$NVIDIA_VOLUME" ]; then
  if docker volume inspect "$NVIDIA_VOLUME" >/dev/null 2>&1; then
    GPU_ARGS+=(-v "$NVIDIA_VOLUME:/opt/quasar/nvidia-driver:ro"
               -e QV_NVIDIA_DIR=/opt/quasar/nvidia-driver)
    echo "   driver volume: $NVIDIA_VOLUME -> /opt/quasar/nvidia-driver (ro)"
  else
    # An explicitly-named volume that does not exist is an operator error, not a
    # reason to silently probe a bare host and report a confusing FAIL.
    die "QUASAR_NVIDIA_DRIVER_VOLUME_NAME=$NVIDIA_VOLUME does not exist"
  fi
fi

# ── Build the assertion program ───────────────────────────────────────────────
# One TAB-separated instruction per line: <check>\t<id>\t<argument>
PROGRAM=""
emit() { PROGRAM+="$1"$'\t'"$2"$'\t'"$3"$'\n'; }

while IFS= read -r p; do [ -n "$p" ] && emit pkg_required   "package.$p"        "$p"; done < <(collect '.packages.required')
while IFS= read -r p; do [ -n "$p" ] && emit pkg_forbidden  "package-absent.$p" "$p"; done < <(collect '.packages.forbidden')
while IFS= read -r b; do [ -n "$b" ] && emit bin_required   "binary.$b"         "$b"; done < <(collect '.binaries.required')
while IFS= read -r b; do [ -n "$b" ] && emit bin_forbidden  "binary-absent.$b"  "$b"; done < <(collect '.binaries.forbidden')
while IFS= read -r p; do [ -n "$p" ] && emit path_required  "path.$p"           "$p"; done < <(collect '.paths.required')
while IFS= read -r p; do [ -n "$p" ] && emit path_forbidden "path-absent.$p"    "$p"; done < <(collect '.paths.forbidden')
# NB env assertions are checked HOST-SIDE against the image config, not with `printenv`
# inside a container — see the env_required block further down. A container's environment
# is not the image's: the nvidia container runtime rewrites NVIDIA_VISIBLE_DEVICES to
# `void` after CDI injection, and any `-e` this script passes shadows the baked value. The
# contract asserts what the IMAGE declares.
while IFS= read -r e; do [ -n "$e" ] && emit gst_required   "gst.$e"            "$e"; done < <(collect '.gst_elements.required')

# libcuda.so.1 must NOT be in the image (it is injected at run time by the NVIDIA
# container toolkit). Only meaningful with the driver detached.
if [ "$GPU_ON" = 0 ]; then
  while IFS= read -r l; do [ -n "$l" ] && emit lib_absent "driver-not-baked.$l" "$l"; done \
    < <(collect '.runtime_libs_absent_without_driver')
  # Graceful-degradation assertion: with no driver, NVIDIA elements must not register.
  while IFS= read -r e; do [ -n "$e" ] && emit gst_forbidden "gst-absent-nogpu.$e" "$e"; done \
    < <(collect '.gst_elements.forbidden_without_gpu')
fi

# GPU-gated element assertions.
GPU_SKIPPED=()
while IFS= read -r e; do
  [ -n "$e" ] || continue
  if [ "$GPU_ON" = 1 ]; then emit gst_required "gst-gpu.$e" "$e"; else GPU_SKIPPED+=("$e"); fi
done < <(collect '.gst_elements.required_with_gpu')
for e in "${EXTRA_ELEMENTS[@]:-}"; do
  [ -n "$e" ] || continue
  if [ "$GPU_ON" = 1 ]; then emit gst_required "gst-gpu.$e" "$e"; else GPU_SKIPPED+=("$e"); fi
done

# Agent-binary assertions: the dlopen property that lets ONE binary serve both vendors.
AGENT_PATH="$(scalar '.agent_binary.path')"
if [ -n "$AGENT_PATH" ]; then
  while IFS= read -r t; do
    [ -n "$t" ] && emit agent_no_dt_needed "agent.no-dt-needed.$t" "$AGENT_PATH|$t"
  done < <(collect '.agent_binary.no_dt_needed')
  [ "$(scalar '.agent_binary.no_unresolved_libs')" = "true" ] \
    && emit agent_no_unresolved "agent.no-unresolved-libs" "$AGENT_PATH"
  [ "$(scalar '.agent_binary.must_start')" = "true" ] \
    && emit agent_must_start "agent.starts" "$AGENT_PATH"
fi

# ── Run every in-image assertion in ONE container ─────────────────────────────
# NB `read -r -d ''` rather than RUNNER=$(cat <<'EOF' ...): a heredoc nested inside a
# command substitution does not parse on bash 3.2 (macOS's /bin/bash), so `bash -n`
# would report a bogus syntax error there even though the script runs fine on the
# Linux build hosts. This form parses everywhere.
read -r -d '' RUNNER <<'INNER' || true
set -u
printf '%s' "$QV_PROGRAM_B64" | base64 -d > /tmp/qv-program 2>/dev/null || {
  echo "RESULT\tERROR\tprogram-decode\tcould not decode assertion program"; exit 3; }
export GST_REGISTRY=/tmp/qv-registry.bin
rm -f "$GST_REGISTRY" 2>/dev/null || true

# NVIDIA driver volume (#468): mirror EXACTLY what nvidia_volume.rs's
# app_container_args() gives a real app container, so a GPU element assertion
# measures the image rather than the host's driver completeness. Set before the
# registry is built below — gst-inspect probes the loader at scan time, so these
# must already be in the environment.
#
# LD_LIBRARY_PATH is prepended to the IMAGE's own baked value, which is why it is
# computed here rather than passed with -e: `docker run -e` would REPLACE the
# baked value and could break element loading in unrelated ways.
if [ -n "${QV_NVIDIA_DIR:-}" ] && [ -d "$QV_NVIDIA_DIR" ]; then
  export LD_LIBRARY_PATH="$QV_NVIDIA_DIR/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  # cuda/lib64 carries the runtime-provisioned NVRTC (#545). Without it the four
  # cuda* elements never register and the probe reports them missing on an image
  # that is fine. Guarded: the dir only exists once the agent has provisioned it.
  [ -d "$QV_NVIDIA_DIR/cuda/lib64" ] &&
    export LD_LIBRARY_PATH="$QV_NVIDIA_DIR/cuda/lib64:$LD_LIBRARY_PATH"
  # Both of these must UNION with the system dirs, never replace them: an EGL
  # vendor path that lists only the volume loses the container's own glvnd.
  export __EGL_VENDOR_LIBRARY_DIRS="$QV_NVIDIA_DIR/glvnd/egl_vendor.d:/etc/glvnd/egl_vendor.d:/usr/share/glvnd/egl_vendor.d"
  export __EGL_EXTERNAL_PLATFORM_CONFIG_DIRS="$QV_NVIDIA_DIR/egl_external_platform.d:/usr/share/egl/egl_external_platform.d"
  # ADD_DRIVER_FILES, not ICD_FILENAMES: the adding form, so the image's own ICDs survive.
  export VK_ADD_DRIVER_FILES="$QV_NVIDIA_DIR/vulkan/icd.d/nvidia_icd.json"
  # GBM_BACKENDS_PATH REPLACES Mesa's backend dir, so set it only when the volume
  # actually carries a backend — same guard as app_container_args().
  [ -f "$QV_NVIDIA_DIR/gbm/nvidia-drm_gbm.so" ] && export GBM_BACKENDS_PATH="$QV_NVIDIA_DIR/gbm"
fi

emit() { printf 'RESULT\t%s\t%s\t%s\n' "$1" "$2" "$3"; }

has_elem() { gst-inspect-1.0 "$1" >/dev/null 2>&1; }

while IFS="$(printf '\t')" read -r check id arg; do
  [ -n "${check:-}" ] || continue
  case "$check" in
    pkg_required)
      # Fedora renames and merges packages across releases (F43: mesa-libGLES is a
      # virtual Provides satisfied by libglvnd-gles). Assert the CAPABILITY, not the
      # package name, or the contract rots on the next base bump and gets "fixed" by
      # deleting a line that was protecting something real.
      if rpm -q "$arg" >/dev/null 2>&1; then emit PASS "$id" "installed"
      elif prov=$(rpm -q --whatprovides "$arg" 2>/dev/null) && [ -n "$prov" ]; then
        emit PASS "$id" "provided by $(printf '%s' "$prov" | head -1)"
      else emit FAIL "$id" "package not installed"; fi ;;
    pkg_forbidden)
      if rpm -q "$arg" >/dev/null 2>&1; then emit FAIL "$id" "build-time package present in a shipped image"
      else emit PASS "$id" "absent"; fi ;;
    bin_required)
      if command -v "$arg" >/dev/null 2>&1; then emit PASS "$id" "$(command -v "$arg")"
      else emit FAIL "$id" "not on PATH"; fi ;;
    bin_forbidden)
      if command -v "$arg" >/dev/null 2>&1; then emit FAIL "$id" "toolchain binary present: $(command -v "$arg")"
      else emit PASS "$id" "absent"; fi ;;
    path_required)
      if [ -e "$arg" ]; then emit PASS "$id" "present"
      else emit FAIL "$id" "missing"; fi ;;
    path_forbidden)
      if [ -e "$arg" ]; then
        sz=$(du -sh "$arg" 2>/dev/null | cut -f1); [ -n "$sz" ] || sz="?"
        emit FAIL "$id" "present (${sz}) — build artifact in a shipped image"
      else emit PASS "$id" "absent"; fi ;;
    gst_required)
      if has_elem "$arg"; then emit PASS "$id" "registered"
      else emit FAIL "$id" "element not registered"; fi ;;
    gst_forbidden)
      if has_elem "$arg"; then emit FAIL "$id" "element registered with no driver present"
      else emit PASS "$id" "not registered (expected)"; fi ;;
    lib_absent)
      if ldconfig -p 2>/dev/null | grep -q "$arg" || [ -e "/usr/lib64/$arg" ]; then
        emit FAIL "$id" "driver library baked into the image"
      else emit PASS "$id" "not baked (injected at run time)"; fi ;;
    agent_no_dt_needed)
      p=${arg%%|*}; tok=${arg#*|}
      if [ ! -e "$p" ]; then emit FAIL "$id" "agent binary missing at $p"
      elif readelf -d "$p" 2>/dev/null | grep -i 'NEEDED' | grep -qi "$tok"; then
        emit FAIL "$id" "hard-links $tok — binary cannot start on a host without it"
      else emit PASS "$id" "no $tok DT_NEEDED"; fi ;;
    agent_no_unresolved)
      if [ ! -e "$arg" ]; then emit FAIL "$id" "agent binary missing at $arg"
      else
        miss=$(ldd "$arg" 2>/dev/null | grep 'not found' || true)
        if [ -n "$miss" ]; then emit FAIL "$id" "unresolved: $(echo "$miss" | tr '\n' ' ')"
        else emit PASS "$id" "all libraries resolve"; fi
      fi ;;
    agent_must_start)
      if [ ! -e "$arg" ]; then emit FAIL "$id" "agent binary missing at $arg"
      else
        out=$("$arg" --help 2>&1 || true)
        case "$out" in
          *"error while loading shared libraries"*|*"cannot open shared object"*)
            emit FAIL "$id" "dynamic linker failure: $(echo "$out" | head -1)" ;;
          *) emit PASS "$id" "starts (reached argument/config handling)" ;;
        esac
      fi ;;
    *) emit FAIL "$id" "unknown check type: $check" ;;
  esac
done < /tmp/qv-program
INNER

PROGRAM_B64="$(printf '%s' "$PROGRAM" | base64 | tr -d '\n')"

set +e
# ${GPU_ARGS[@]+...}: with --no-gpu the array is empty, and bash 3.2 aborts on a bare
# "${empty[@]}" under `set -u` (see the CHAIN note above).
RAW="$(docker run --rm --network none ${GPU_ARGS[@]+"${GPU_ARGS[@]}"} \
        -e QV_PROGRAM_B64="$PROGRAM_B64" \
        --entrypoint /bin/sh "$IMAGE" -c "$RUNNER" 2>&1)"
RUN_RC=$?
set -e
if [ "$RUN_RC" -ne 0 ] && ! printf '%s' "$RAW" | grep -q '^RESULT'; then
  printf '%s\n' "$RAW" >&2
  die "could not run assertions inside $IMAGE (exit $RUN_RC)"
fi

# ── Host-side assertions: size and image config ───────────────────────────────
HOST_RESULTS=""
hemit() { HOST_RESULTS+="RESULT"$'\t'"$1"$'\t'"$2"$'\t'"$3"$'\n'; }

# Image-declared ENV. Asserted from the image config rather than from inside a running
# container: the nvidia container runtime rewrites NVIDIA_VISIBLE_DEVICES to `void` after
# CDI injection, and this script's own `-e NVIDIA_DRIVER_CAPABILITIES=all` (added with
# --gpu) would shadow the baked value — so a container-side check reports the run, not the
# image, and fails a perfectly correct image.
IMAGE_ENV="$(docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$IMAGE")"
while IFS= read -r spec; do
  [ -n "$spec" ] || continue
  name="${spec%%=*}"
  actual="$(printf '%s\n' "$IMAGE_ENV" | grep -m1 "^${name}=" | cut -d= -f2- || true)"
  if [ "$name" = "$spec" ]; then
    if [ -n "$actual" ]; then hemit PASS "env.$name" "$actual"
    else hemit FAIL "env.$name" "not declared in the image (or empty)"; fi
  else
    want="${spec#*=}"
    if [ "$actual" = "$want" ]; then hemit PASS "env.$name" "$actual"
    else hemit FAIL "env.$name" "image declares '${actual:-<unset>}', contract wants '$want'"; fi
  fi
done < <(collect '.env.required')

# Entrypoint env guards (#94): unlike the checks above, this measures RUNTIME behavior of
# the real ENTRYPOINT, not baked image config — so it gets its own `docker run`s (with
# `env` as the command) instead of joining the bypassed --entrypoint /bin/sh assertion
# program. Empty-but-set must come out unset (the entrypoint's own `[ -z ] && unset`
# guard); a real value must survive, so a deliberate bounded trace path still works.
while IFS= read -r name; do
  [ -n "$name" ] || continue
  empty_out="$(docker run --rm --network none -e "${name}=" "$IMAGE" env 2>&1)" || true
  if printf '%s\n' "$empty_out" | grep -q "^${name}="; then
    hemit FAIL "entrypoint-guard.$name.empty" "still present when set empty: $(printf '%s\n' "$empty_out" | grep "^${name}=")"
  else
    hemit PASS "entrypoint-guard.$name.empty" "absent from \`env\` (guard fired)"
  fi

  want="/tmp/qv-guard-$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')"
  set_out="$(docker run --rm --network none -e "${name}=${want}" "$IMAGE" env 2>&1)" || true
  if printf '%s\n' "$set_out" | grep -qF "${name}=${want}"; then
    hemit PASS "entrypoint-guard.$name.set" "survives: ${want}"
  else
    hemit FAIL "entrypoint-guard.$name.set" "expected ${name}=${want}, got: $(printf '%s\n' "$set_out" | grep "^${name}=" || echo '<absent>')"
  fi
done < <(collect '.entrypoint_env_guards.unset_when_empty')

# Image-declared provenance labels (§4 of the image-lineage spec). Host-side, like
# env: these are image config, not container state.
IMAGE_LABELS="$(docker image inspect --format '{{range $k, $v := .Config.Labels}}{{$k}}={{$v}}{{"\n"}}{{end}}' "$IMAGE")"
while IFS= read -r spec; do
  [ -n "$spec" ] || continue
  name="${spec%%=*}"
  actual="$(printf '%s\n' "$IMAGE_LABELS" | grep -m1 "^${name}=" | cut -d= -f2- || true)"
  if [ "$name" = "$spec" ]; then
    if [ -n "$actual" ]; then hemit PASS "label.$name" "$actual"
    else hemit FAIL "label.$name" "not declared on the image"; fi
  else
    want="${spec#*=}"
    if [ "$actual" = "$want" ]; then hemit PASS "label.$name" "$actual"
    else hemit FAIL "label.$name" "image declares '${actual:-<unset>}', contract wants '$want'"; fi
  fi
done < <(collect '.labels.required')

SIZE_BYTES="$(docker image inspect --format '{{.Size}}' "$IMAGE")"
SIZE_MB=$(( SIZE_BYTES / 1024 / 1024 ))
SIZE_MAX="$(scalar '.size_max_mb')"
if [ -n "$SIZE_MAX" ]; then
  if [ "$SIZE_MB" -le "$SIZE_MAX" ]; then hemit PASS "size" "${SIZE_MB}MB <= ${SIZE_MAX}MB"
  else hemit FAIL "size" "${SIZE_MB}MB exceeds the ${SIZE_MAX}MB ceiling"; fi
fi

ENTRY_WANT="$(scalar '.image_config.entrypoint_or_cmd_contains')"
if [ -n "$ENTRY_WANT" ]; then
  CFG="$(docker image inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' "$IMAGE")"
  if printf '%s' "$CFG" | grep -q "$ENTRY_WANT"; then hemit PASS "image.entrypoint" "$CFG"
  else hemit FAIL "image.entrypoint" "neither Entrypoint nor Cmd mentions '$ENTRY_WANT': $CFG"; fi
fi

if [ "$(scalar '.image_config.healthcheck_required')" = "true" ]; then
  HC="$(docker image inspect --format '{{json .Config.Healthcheck}}' "$IMAGE")"
  if [ "$HC" != "null" ] && [ -n "$HC" ]; then hemit PASS "image.healthcheck" "declared"
  else hemit FAIL "image.healthcheck" "no HEALTHCHECK declared"; fi
fi

if [ "$(scalar '.image_config.must_not_run_as_root')" = "true" ]; then
  U="$(docker image inspect --format '{{.Config.User}}' "$IMAGE")"
  if [ -n "$U" ] && [ "$U" != "root" ] && [ "$U" != "0" ]; then hemit PASS "image.user" "runs as '$U'"
  else hemit FAIL "image.user" "runs as root (User='${U:-<empty>}')"; fi
fi

if [ "$(scalar '._deployment_ban')" = "not_a_runtime_role" ]; then
  hemit PASS "role.deployment-ban" "declared build/test-only — build-images.sh will refuse runtime tags"
fi

# ── Report ────────────────────────────────────────────────────────────────────
ALL="$(printf '%s\n%s' "$RAW" "$HOST_RESULTS" | grep '^RESULT' || true)"
PASS_N=$(printf '%s\n' "$ALL" | grep -c $'^RESULT\tPASS\t' || true)
FAIL_N=$(printf '%s\n' "$ALL" | grep -c $'^RESULT\tFAIL\t' || true)
PASS_N=${PASS_N:-0}; FAIL_N=${FAIL_N:-0}

if [ "$QUIET" = 0 ]; then
  printf '\n── contract: role %s (chain: %s) · image %s ──\n' "$ROLE" "$(IFS=' → '; echo "${CHAIN[*]}")" "$IMAGE"
  printf '%s\n' "$ALL" | awk -F'\t' 'NF>=4 { printf "  %-4s %-46s %s\n", $2, $3, $4 }'
  if [ "${#GPU_SKIPPED[@]}" -gt 0 ]; then
    printf '\n  SKIP %-46s %s\n' "gpu-gated assertions" \
      "no GPU attached — not asserted: ${GPU_SKIPPED[*]}"
    printf '       %-46s %s\n' "" "re-run with --gpu on a GPU host to close this gap"
  fi
else
  printf '%s\n' "$ALL" | awk -F'\t' '$2=="FAIL" && NF>=4 { printf "  FAIL %-46s %s\n", $3, $4 }'
fi

if [ -n "$JSON_OUT" ]; then
  {
    printf '{\n'
    printf '  "image": %s,\n'  "$(jq -Rn --arg v "$IMAGE" '$v')"
    printf '  "image_id": %s,\n' "$(jq -Rn --arg v "$(docker image inspect --format '{{.Id}}' "$IMAGE")" '$v')"
    printf '  "role": %s,\n'   "$(jq -Rn --arg v "$ROLE" '$v')"
    printf '  "chain": %s,\n'  "$(printf '%s\n' "${CHAIN[@]}" | jq -Rs 'split("\n")|map(select(length>0))')"
    printf '  "size_mb": %s,\n' "$SIZE_MB"
    printf '  "size_max_mb": %s,\n' "${SIZE_MAX:-0}"
    printf '  "gpu_attached": %s,\n' "$([ "$GPU_ON" = 1 ] && echo true || echo false)"
    printf '  "gpu_skipped_elements": %s,\n' "$(printf '%s\n' "${GPU_SKIPPED[@]:-}" | jq -Rs 'split("\n")|map(select(length>0))')"
    printf '  "passed": %s,\n' "$PASS_N"
    printf '  "failed": %s,\n' "$FAIL_N"
    printf '  "verdict": "%s",\n' "$([ "$FAIL_N" -eq 0 ] && echo PASS || echo FAIL)"
    printf '  "assertions": %s\n' "$(printf '%s\n' "$ALL" | awk -F'\t' 'NF>=4 {printf "%s\t%s\t%s\n",$2,$3,$4}' \
        | jq -Rs 'split("\n")|map(select(length>0))|map(split("\t"))|map({status:.[0],id:.[1],detail:.[2]})')"
    printf '}\n'
  } > "$JSON_OUT"
  [ "$QUIET" = 0 ] && printf '\n  report → %s\n' "$JSON_OUT"
fi

printf '\n  %s — %d passed, %d failed%s\n\n' \
  "$([ "$FAIL_N" -eq 0 ] && echo 'CONTRACT SATISFIED' || echo 'CONTRACT VIOLATED')" \
  "$PASS_N" "$FAIL_N" \
  "$([ "${#GPU_SKIPPED[@]}" -gt 0 ] && echo ", ${#GPU_SKIPPED[@]} gpu-gated skipped" || echo "")"

[ "$FAIL_N" -eq 0 ]
