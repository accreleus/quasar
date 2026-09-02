#!/usr/bin/env bash
# _qimg_repin.sh — snapshot -> build candidate -> snapshot -> diff. Invoked by
# `qimg repin --host H [--role R] [--deep] [pin flags...]` (scripts/qimg's
# `repin` case execs this with `--host "$H"` [`--role "$ROLE"`] plus the raw
# PASSTHRU argv -- no dispatcher quoting needed at THIS hop, since bash hands
# them over as literal argv elements, not a string. The hop that matters is
# below, where those same tokens get interpolated into a remote-shell COMMAND
# STRING for q_ssh/ssh.)
#
# Never promotes :latest, ever: the candidate is ALWAYS built with
# `--no-latest --tag-suffix repin-$$` (Task 6 review FIX 1) -- a re-pin that
# happens to validate is not automatically an image you want live, so this
# tool must not move production's pointer even on a clean pass. It diffs the
# freshly-built DATED tag (resolved from deploy/.build-report.json, the one
# artifact build-images.sh itself writes with the real image:tag it produced
# -- never guessed/reconstructed locally) against current `:latest`. Going
# live is a separate, deliberate step (`qimg deploy`) for a human to take.
#
# A CONTRACT-VIOLATING candidate is exactly the case this verb exists for: the
# after-snapshot and the diff still run against it (build-images.sh's own
# non-zero exit for that case is caught, not `set -e`-propagated). Only a
# genuine BUILD failure -- no image was ever produced -- has nothing to
# diff; that surfaces as this script's own exit 1 with a distinct message
# BEFORE ever attempting a snapshot against a tag that was never created (a
# validate-image.sh call against a nonexistent tag looks identical to an
# unreachable host -- see the FIX 3 note below -- so distinguishing these
# two cases up front, from build-images.sh's own per-role verdict, matters).
set -euo pipefail
SKILL_DIR="$(cd "$(dirname "$0")/.." && pwd)"
source "$(dirname "$0")/../../_shared/lib.sh"

# Single-quote a token for safe interpolation into a remote shell command
# line (bash 3.2 compatible -- no ${var@Q}). Identical to scripts/qimg's own
# `shquote` (round-trip tested there via execution, including the
# embedded-single-quote + $()/backtick case) -- reused verbatim rather than
# re-derived, since a hand-rolled quoting helper that merely *looks* correct
# on read-through was previously caught only by an executed round trip, not
# by inspection.
shquote() {
  local q="'" bq="'\\''"
  printf '%s' "$q${1//$q/$bq}$q"
}

# `runtime` is the universal agent image (#545). The old default was `nv`, a
# lineage that no longer exists — a bare `qimg repin --host H` re-pinned nothing.
HOST=""; ROLE="runtime"; DEEP=0; PINS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --host) [ $# -ge 2 ] || { echo "repin: --host needs a value" >&2; exit 2; }
            HOST="$2"; shift 2 ;;
    --role) [ $# -ge 2 ] || { echo "repin: --role needs a value" >&2; exit 2; }
            ROLE="$2"; shift 2 ;;
    --deep) DEEP=1; shift ;;
    *)      PINS+=("$1"); shift ;;
  esac
done
[ -n "$HOST" ] || { echo "repin: --host is required" >&2; exit 2; }

DIR="$(q_host_cfg "$HOST" dir)"
[ -n "$DIR" ] || { echo "repin: unknown host '$HOST'" >&2; exit 2; }

# A re-pin builds a candidate image on the host. It never promotes :latest, so
# it is mutating rather than destructive — but it still must not land on a
# production target that nobody named.
q_guard_mutating qimg "$HOST" 1 repin

# ROLE is user-controlled (a CLI token) -- passed to python as argv, never
# interpolated into the python source string, so it can't break out of the
# dict-lookup expression the way embedding it in a "..."-quoted -c string
# would (same class of hazard the shquote fix addresses on the shell side).
TAG="$(python3 -c "
import json, sys
c = json.load(open(sys.argv[1]))
print((c.get('roles', {}).get(sys.argv[2]) or {}).get('tag', ''))
" "$SKILL_DIR/config.json" "$ROLE")"
[ -n "$TAG" ] || { echo "repin: unknown role '$ROLE'" >&2; exit 2; }

# GPU-gating follows the exact same host-config rule `qimg validate` already
# uses (never hardcode --gpu — a repin snapshot against a non-GPU host must
# not force GPU-gated assertions that host can never satisfy).
if [ "$(q_host_cfg "$HOST" gpu)" = "none" ]; then GPU_FLAG="--no-gpu"; else GPU_FLAG="--gpu"; fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# snapshot <out-doc-path> <image:tag> — one validate-image.sh --json call,
# one docker labels call, and (only when --deep is set) one full
# gst-inspect-1.0 element-inventory call, merged into the role-entry-shaped
# document _qimg_diff.py consumes: {"size_mb", "labels", "contract": <the
# full validate-image.sh --json object>, "deep_elements": [...] (only when
# --deep)}.
#
# FIX 3 (unreachable host / nothing came back): deploy/validate-image.sh
# --json writes its JSON document on BOTH a passing and a FAILING contract
# check -- so if extract_json_object finds no valid object at all in the raw
# capture, that is unambiguous evidence nothing came back (ssh never
# connected, or the image tag genuinely does not exist), not "a degraded but
# legitimate empty result". The old `|| true` guard (still needed, because a
# real contract FAIL is validate-image.sh's normal non-zero exit and must
# NOT abort this script) made that indistinguishable from a clean no-op two
# degraded snapshots would silently diff as "nothing changed". Treat a
# missing JSON object as a hard failure of THIS snapshot: exit 2, name the
# host and the tag, and never let it flow into the diff as if it were valid
# (possibly empty) data.
snapshot() {
  local out="$1" tag="$2"
  local raw="$TMP/$(basename "$out").raw"
  local labels="$TMP/$(basename "$out").labels"
  local deep="$TMP/$(basename "$out").deep"
  # Every value interpolated into the remote command string is shquote'd;
  # none of $DIR/$tag/$ROLE/$GPU_FLAG is user-controlled free text at this
  # point (DIR/TAG come from config.json via q_host_cfg/python, ROLE and
  # GPU_FLAG are validated/derived above, tag is either "$TAG:latest" or a
  # value resolved from the host's own build-report JSON below), but they're
  # quoted anyway rather than trusted as "safe today, so no quoting needed".
  q_ssh "$HOST" "cd $(shquote "$DIR") && bash deploy/validate-image.sh --image $(shquote "$tag") --role $(shquote "$ROLE") $GPU_FLAG --quiet --json /dev/stdout" \
    > "$raw" 2>/dev/null || true
  q_ssh "$HOST" "docker image inspect --format '{{json .Config.Labels}}' $(shquote "$tag")" \
    > "$labels" 2>/dev/null || printf 'null' > "$labels"
  : > "$deep"
  if [ "$DEEP" = 1 ]; then
    # `-e GST_REGISTRY=` forces a fresh element scan rather than trusting a
    # registry baked at image-build time -- the same stale-registry trap the
    # VA encoder path already documents (a registry cached with no GPU
    # present can hide a device-gated element that IS actually there).
    q_ssh "$HOST" "docker run --rm -e GST_REGISTRY=/tmp/qimg-deep-registry --entrypoint gst-inspect-1.0 $(shquote "$tag")" \
      > "$deep" 2>/dev/null || true
  fi
  local rc=0
  python3 -c "
import json, re, sys
raw_path, labels_path, out_path, deep_path, deep_flag, scripts_dir = sys.argv[1:7]
sys.path.insert(0, scripts_dir)
from _qimg_collect import extract_json_object
val = extract_json_object(open(raw_path).read())
if val is None:
    sys.stderr.write('no valid validate-image.sh JSON object was found\n')
    sys.exit(3)
try:
    labels = json.load(open(labels_path))
    if not isinstance(labels, dict):
        labels = {}
except Exception:
    labels = {}
doc = {'size_mb': val.get('size_mb'), 'labels': labels, 'contract': val}
if deep_flag == '1':
    elems = []
    for line in open(deep_path).read().splitlines():
        line = line.strip()
        m = re.match(r'^[^:\s]+:\s+([^:\s]+):', line)
        if m:
            elems.append(m.group(1))
    doc['deep_elements'] = sorted(set(elems))
json.dump(doc, open(out_path, 'w'))
" "$raw" "$labels" "$out" "$deep" "$DEEP" "$SKILL_DIR/scripts" || rc=$?
  if [ "$rc" != 0 ]; then
    if [ "$rc" = 3 ]; then
      echo "repin: no data came back from '$HOST' for '$tag' (validate-image.sh produced no parseable JSON) -- treating this as an unreachable/failed host, not a clean diff" >&2
    else
      echo "repin: snapshot for '$tag' on '$HOST' failed unexpectedly (python exit $rc)" >&2
    fi
    exit 2
  fi
}

quoted_pins=""
if [ "${#PINS[@]}" -gt 0 ]; then
  for tok in "${PINS[@]}"; do
    quoted_pins="$quoted_pins $(shquote "$tok")"
  done
fi

echo "── snapshot current $TAG:latest ──" >&2
snapshot "$TMP/before.json" "$TAG:latest"

# FIX 1: --no-latest + a dated --tag-suffix so a re-pin evaluation NEVER
# moves production's :latest pointer, whether the candidate passes its
# contract or not. The exact tag actually produced is read back from
# deploy/.build-report.json below -- never assumed/reconstructed locally
# (the dated-tag timestamp is build-images.sh's own clock, not this
# script's).
BUILD_TAG_SUFFIX="repin-$$"
echo "── build candidate ($ROLE --no-latest --tag-suffix $BUILD_TAG_SUFFIX$quoted_pins) ──" >&2
BUILD_RC=0
q_ssh "$HOST" "cd $(shquote "$DIR") && bash deploy/build-images.sh $(shquote "$ROLE") --no-latest --tag-suffix $(shquote "$BUILD_TAG_SUFFIX")$quoted_pins" \
  || BUILD_RC=$?
# build-images.sh's own exit codes (see its header): 0 pass, 1 build-or-
# contract failure, 2 usage/infra error. Deliberately NOT `set -e`-propagated
# here -- a contract-violating (but successfully BUILT) candidate is exactly
# the case this verb exists to surface, and the diff below is how it gets
# surfaced. Only a bare usage/infra error (2) has no build-report worth
# trusting (build-images.sh dies via its own `die()` before ever reaching
# the loop that writes deploy/.build-report.json, so any report present on
# the host at this point would be a STALE one from a previous run) --that
# case exits immediately here, matching build-images.sh's own exit code.
if [ "$BUILD_RC" -eq 2 ]; then
  echo "repin: build-images.sh reported a usage/infra error (exit 2) for role '$ROLE' on '$HOST' -- see the build output above; nothing was built, nothing to diff" >&2
  exit 2
fi

# Resolve the real candidate image:tag from the build report build-images.sh
# itself just wrote (FIX 1) -- never guess a tag string locally. Reuses
# _qimg_collect.py's own parse_build_report rather than a second parser.
REPORT_RAW="$TMP/build-report.raw"
REPORT_RC=0
q_ssh "$HOST" "cd $(shquote "$DIR") && cat deploy/.build-report.json" \
  > "$REPORT_RAW" 2>/dev/null || REPORT_RC=$?
if [ "$REPORT_RC" != 0 ]; then
  echo "repin: could not read deploy/.build-report.json from '$HOST' after the build (ssh exit $REPORT_RC)" >&2
  exit 2
fi

python3 -c "
import sys
sys.path.insert(0, sys.argv[2])
from _qimg_collect import parse_build_report
report = parse_build_report(open(sys.argv[1]).read())
role = sys.argv[3]
verdict, tag = '', ''
if report is not None:
    for r in (report.get('roles') or []):
        if isinstance(r, dict) and r.get('role') == role:
            verdict = r.get('verdict') or ''
            image, rtag = r.get('image') or '', r.get('tag') or ''
            if image and rtag:
                tag = image + ':' + rtag
            break
print(verdict)
print(tag)
" "$REPORT_RAW" "$SKILL_DIR/scripts" "$ROLE" > "$TMP/resolve.out"
VERDICT="$(sed -n '1p' "$TMP/resolve.out")"
CANDIDATE_TAG="$(sed -n '2p' "$TMP/resolve.out")"

if [ -z "$VERDICT" ]; then
  echo "repin: build report from '$HOST' has no entry for role '$ROLE' after the build -- cannot resolve the candidate tag" >&2
  exit 2
fi

if [ "$VERDICT" = "build-failed" ]; then
  echo "repin: candidate build FAILED for role '$ROLE' on '$HOST' -- no image was produced, nothing to diff" >&2
  exit 1
fi

[ -n "$CANDIDATE_TAG" ] || { echo "repin: build report entry for role '$ROLE' has no image/tag -- cannot snapshot the candidate" >&2; exit 2; }

echo "── snapshot candidate ($CANDIDATE_TAG, verdict=$VERDICT) ──" >&2
snapshot "$TMP/after.json" "$CANDIDATE_TAG"

echo "── diff ──" >&2
python3 "$SKILL_DIR/scripts/_qimg_diff.py" --before "$TMP/before.json" --after "$TMP/after.json"
