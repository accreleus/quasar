#!/usr/bin/env bash
# scripts/harness/checks/vram-telemetry.sh — is live VRAM telemetry flowing on this
# stack RIGHT NOW? (#383 spec §7.2.)
#
# Standalone, idempotent, READ-ONLY: never launches a session, never mutates
# anything. Safe to run at any time against a live stack.
#
# For every GPU on every ONLINE host (GET /v1/hosts -> GET /v1/hosts/{id}/gpus)
# asserts:
#   - vram_sampled_at is non-null and within the staleness window (default 20s)
#   - vram_mb_free is plausible: 0 < vram_mb_free <= vram_mb_total
#
# If the fields are entirely absent from a GPU's API response (a control plane
# that predates #383), that GPU SKIPs with a clear message — this is the
# EXPECTED state until the server side of #383 merges, not a failure. This is
# the check that would have caught the #383 §0 premise error (the "VRAM
# telemetry" issue #383 claimed already existed, but did not).
#
# Usage (standalone):
#   scripts/harness/checks/vram-telemetry.sh [--stack=hermes|tower] [--staleness=SECS]
#   API=https://localhost:8443 ADMIN_EMAIL=... ADMIN_PASS=... scripts/harness/checks/vram-telemetry.sh
#
# Sourceable: another harness (scripts/harness/run-admission.sh) can `source` this file
# and call `vram_telemetry_check` directly. That reuses whatever harness the
# CALLER already ran `harness_init` on — pass/fail/skip land in the caller's
# own counters instead of running a separate init/report/exit cycle. Expects
# $API and $ADMIN_TOK to already be set by the caller (same names run-admission.sh
# uses for its own login).
#
# Exit (standalone mode only): 0 if every check passed or skipped, 1 if any FAILed.
set -euo pipefail

# ── vram_telemetry_check [staleness_secs] ───────────────────────────────────
# Uses $API / $ADMIN_TOK (already-authenticated caller state) and, unless
# overridden by an explicit argument, $VRAM_STALENESS_SECS (default 20 — #383
# spec §4.3's QUASAR_VRAM_STALENESS_SECS default). Emits pass/fail/skip via
# whatever harness.sh is already sourced in this process; never exits.
vram_telemetry_check() {
  local staleness="${1:-${VRAM_STALENESS_SECS:-20}}"
  local api="${API:?vram_telemetry_check: \$API must be set}"
  local tok="${ADMIN_TOK:?vram_telemetry_check: \$ADMIN_TOK must be set}"

  local hosts_json
  if ! hosts_json=$(curl -sk --connect-timeout 5 --max-time 10 -f "$api/v1/hosts" \
    -H "Authorization: Bearer $tok" 2>/dev/null); then
    skip "GET /v1/hosts unreachable/failed at $api — cannot check vram telemetry"
    return 0
  fi

  local online_ids
  online_ids=$(printf '%s' "$hosts_json" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for h in d.get('items', []):
    if h.get('status') == 'online':
        print(h['id'])
" 2>/dev/null || true)

  if [ -z "$online_ids" ]; then
    skip "no online hosts reported by GET /v1/hosts — nothing to check"
    return 0
  fi

  # Fetch each online host's GPU list into one combined temp file (base64'd,
  # one line per host) so a single python pass can emit every verdict — a
  # per-host curl failure becomes that host's own FAIL line, not a script abort.
  local combined
  combined="$(mktemp "${TMPDIR:-/tmp}/vram-telemetry.XXXXXX")"
  local hid gpus_json
  while IFS= read -r hid; do
    [ -n "$hid" ] || continue
    if gpus_json=$(curl -sk --connect-timeout 5 --max-time 10 -f "$api/v1/hosts/$hid/gpus" \
      -H "Authorization: Bearer $tok" 2>/dev/null); then
      printf 'HOST\t%s\t%s\n' "$hid" "$(printf '%s' "$gpus_json" | base64 | tr -d '\n')" >>"$combined"
    else
      printf 'HOSTERR\t%s\t\n' "$hid" >>"$combined"
    fi
  done <<EOF_HOSTS
$online_ids
EOF_HOSTS

  local verdicts
  verdicts=$(python3 - "$combined" "$staleness" <<'PYEOF'
import sys, json, base64, datetime


def parse_ts(s):
    if not s:
        return None
    s = s.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    if "." in s:
        head, rest = s.split(".", 1)
        frac = ""
        i = 0
        while i < len(rest) and rest[i].isdigit():
            frac += rest[i]
            i += 1
        tail = rest[i:]
        frac = (frac + "000000")[:6]
        s = head + "." + frac + tail
    try:
        return datetime.datetime.fromisoformat(s)
    except Exception:
        return None


combined_file, staleness_s = sys.argv[1], sys.argv[2]
staleness = float(staleness_s)
now = datetime.datetime.now(datetime.timezone.utc)

with open(combined_file, encoding="utf-8") as fh:
    lines = [l.rstrip("\n") for l in fh if l.strip()]

if not lines:
    print("SKIP\tno online hosts had a GPU response to check")

for line in lines:
    parts = line.split("\t")
    kind = parts[0]
    hid = parts[1] if len(parts) > 1 else "?"
    if kind == "HOSTERR":
        print(f"FAIL\thost={hid}: GET /v1/hosts/{hid}/gpus failed or timed out")
        continue
    b64 = parts[2] if len(parts) > 2 else ""
    try:
        raw = base64.b64decode(b64).decode("utf-8", "replace")
        d = json.loads(raw)
    except Exception as e:
        print(f"FAIL\thost={hid}: GET /v1/hosts/{hid}/gpus returned unparsable JSON: {e}")
        continue
    items = d.get("items", [])
    if not items:
        print(f"SKIP\thost={hid}: reports no GPUs")
        continue
    for g in items:
        idx = g.get("gpu_index", "?")
        gid = g.get("gpu_id", "?")
        label = f"host={hid} gpu_index={idx} gpu_id={gid}"

        if "vram_mb_free" not in g or "vram_sampled_at" not in g:
            print(f"SKIP\t{label}: vram telemetry fields absent from the API response (control plane predates #383)")
            continue

        sampled_at_raw = g.get("vram_sampled_at")
        free = g.get("vram_mb_free")
        total = g.get("vram_mb_total")

        if sampled_at_raw is None:
            print(f"FAIL\t{label}: vram_sampled_at is null (never sampled)")
            continue
        ts = parse_ts(sampled_at_raw)
        if ts is None:
            print(f"FAIL\t{label}: vram_sampled_at unparsable: {sampled_at_raw!r}")
            continue
        age = (now - ts).total_seconds()
        if age > staleness:
            print(f"FAIL\t{label}: vram_sampled_at stale (age={age:.1f}s > {staleness:.0f}s window)")
            continue
        if age < -5:
            print(f"FAIL\t{label}: vram_sampled_at is in the future (age={age:.1f}s) — check clock sync")
            continue

        if free is None:
            print(f"FAIL\t{label}: vram_mb_free is null")
            continue
        if total is None or total <= 0:
            print(f"FAIL\t{label}: vram_mb_total missing/non-positive ({total!r}); cannot validate free against it")
            continue
        if not (0 < free <= total):
            print(f"FAIL\t{label}: vram_mb_free={free} implausible (want 0 < free <= vram_mb_total={total})")
            continue

        print(f"PASS\t{label}: fresh (age={age:.1f}s <= {staleness:.0f}s), free={free}MB / total={total}MB")
PYEOF
)
  rm -f "$combined"

  if [ -z "$verdicts" ]; then
    skip "no GPU telemetry verdicts produced (no online hosts with GPUs?)"
    return 0
  fi

  local vline kind msg
  while IFS= read -r vline; do
    [ -n "$vline" ] || continue
    kind="${vline%%$'\t'*}"
    msg="${vline#*$'\t'}"
    case "$kind" in
      PASS) pass "$msg" ;;
      FAIL) fail "$msg" ;;
      SKIP) skip "$msg" ;;
      *) fail "vram-telemetry: unrecognized verdict line: $vline" ;;
    esac
  done <<EOF_VERDICTS
$verdicts
EOF_VERDICTS
}

# ── Standalone execution (not sourced) ──────────────────────────────────────
# `${BASH_SOURCE[0]}" = "$0"` is the standard "am I being run, not sourced" test.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
  # shellcheck source=scripts/harness/lib/harness.sh
  source "$ROOT/scripts/harness/lib/harness.sh"

  STACK="hermes"
  VRAM_STALENESS_SECS="${VRAM_STALENESS_SECS:-20}"
  for a in "$@"; do
    case "$a" in
      --stack=*) STACK="${a#*=}" ;;
      --staleness=*) VRAM_STALENESS_SECS="${a#*=}" ;;
      -h | --help)
        sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
      *)
        echo "unknown arg: $a" >&2
        exit 2
        ;;
    esac
  done
  case "$STACK" in
    hermes) DEFAULT_PORT=8080; DEFAULT_TLS_PORT=8443 ;;
    tower) DEFAULT_PORT=18080; DEFAULT_TLS_PORT=18443 ;;
    *)
      echo "unknown --stack=$STACK (hermes|tower)" >&2
      exit 2
      ;;
  esac

  ENV_FILE="$ROOT/deploy/.env"
  if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck source=/dev/null
    . "$ENV_FILE"
    set +a
  fi
  # Browser-facing routes are HTTPS-only since the HTTP->HTTPS redirect shipped
  # (develop 20b1d33): plain HTTP answers /v1/* with a 308 and an empty body, so a
  # harness defaulting to http:// sees "login returned no token" and skips against
  # a perfectly healthy stack. Default to the TLS listener; curl uses -k because
  # the default cert is the self-signed one generated at first boot. Only /health
  # and the agent surface stay on plain HTTP (#376).
  API="${API:-https://localhost:${QUASAR_TLS_PORT:-$DEFAULT_TLS_PORT}}"
  ADMIN_EMAIL="${ADMIN_EMAIL:-${BOOTSTRAP_ADMIN_EMAIL:-}}"
  ADMIN_PASS="${ADMIN_PASS:-${BOOTSTRAP_ADMIN_PASSWORD:-}}"

  harness_init "vram-telemetry"
  require curl python3

  harness_note "api" "$API"
  harness_note "stack" "$STACK"
  harness_note "staleness_secs" "$VRAM_STALENESS_SECS"

  if [ -z "$ADMIN_EMAIL" ] || [ -z "$ADMIN_PASS" ]; then
    skip "admin credentials unavailable (no $ENV_FILE, or BOOTSTRAP_ADMIN_* unset) — cannot log in to check vram telemetry"
    harness_report
  fi

  if ! LOGIN_JSON=$(curl -sk --connect-timeout 5 --max-time 10 -X POST "$API/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" 2>/dev/null); then
    skip "control-plane unreachable at $API (login request failed/timed out) — stack likely down"
    harness_report
  fi

  ADMIN_TOK=$(printf '%s' "$LOGIN_JSON" | python3 -c "
import sys, json
try:
    print(json.load(sys.stdin).get('access_token', ''))
except Exception:
    print('')
" 2>/dev/null || echo "")
  if [ -z "$ADMIN_TOK" ]; then
    skip "admin login did not return an access token — stack may be unhealthy or credentials wrong"
    harness_report
  fi

  vram_telemetry_check "$VRAM_STALENESS_SECS"
  harness_report
fi
