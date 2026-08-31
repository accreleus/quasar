#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/uiaudit.sh — automated UI visual-audit evidence capture, extracted
# from the manual #459 audit run. Deterministic machinery only: it captures
# screenshots + DOM metrics, and renders self-contained HTML reports. Reading
# the evidence, comparing it against design_handoff_v3/ mockups, and
# writing findings.json is the agent's job (see
# .claude/skills/quasar-ui-audit/SKILL.md, which wraps these verbs).
#
# Subcommands:
#   capture   Screenshot + metric-scan a set of routes against a live stack.
#             scripts/dx/uiaudit.sh capture --url <base> [--out <dir>]
#               [--routes all|id,id,...] [--widths WxH,WxH,...]
#               [--state-admin <file>] [--state-user <file>]
#               [--key <dev-agent-key>]
#             If --state-admin/--state-user are omitted, sessions are minted
#             automatically via scripts/dx/agentcreds.sh against --url (needs
#             QUASAR_DEV_AGENT_AUTH=1 on that stack and a dev key — pass --key
#             or set $QUASAR_DEV_AGENT_KEY; see docs/configuration.md).
#
#   report    Evidence dir (+ optional findings.json) -> self-contained HTML.
#             scripts/dx/uiaudit.sh report --evidence <dir> [--findings <file>]
#               [--out <report.html>]
#             No --findings = coverage-only report (every captured surface
#             listed, flagged only from metrics, no editorializing).
#
#   ab        Two evidence dirs (before/after) -> self-contained A/B HTML.
#             scripts/dx/uiaudit.sh ab --before <dir> --after <dir>
#               [--out <report.html>]
#
#   make-audit / make-ab
#             The `make ui-audit` / `make ui-audit-ab` entry points. They take
#             $URL/$OUT/$ROUTES/$KEY and $BEFORE/$AFTER/$OUT from the
#             ENVIRONMENT and call capture+report / ab. Not for hand use — they
#             exist so the Makefile interpolates no caller-settable value into a
#             recipe line (#550).
#
# Evidence defaults to $DX_UIAUDIT_DIR/<stamp> (see scripts/dx/common.sh);
# never committed (.gitignore covers .uiaudit/). Never echoes the dev-agent
# key or any storage-state contents.
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET_NAME=uiaudit
UIAUDIT_SCRIPTS_DIR="$DX_DIR/uiaudit"

usage() { sed -n '3,41p' "$0" | sed 's/^# \{0,1\}//'; }

if [ $# -lt 1 ]; then
  usage
  dx_guard "$TARGET_NAME" "missing subcommand (capture|report|ab)"
fi

SUBCOMMAND="$1"; shift

# ── Shared: bootstrap Playwright the same way scripts/dx/validate.sh does ────
bootstrap_playwright() {
  local validate_dir="$DX_ROOT/scripts/validate"
  if [ ! -d "$validate_dir/node_modules" ]; then
    dx_info "installing scripts/validate node deps (npm ci — first run, caches thereafter)"
    ( cd "$validate_dir" && npm ci )
  fi

  local browsers_cache="${PLAYWRIGHT_BROWSERS_PATH:-$validate_dir/.cache/playwright}"
  export PLAYWRIGHT_BROWSERS_PATH="$browsers_cache"
  local chromium_exe
  chromium_exe="$(cd "$validate_dir" && node -e "process.stdout.write(require('playwright').chromium.executablePath())" 2>/dev/null || true)"
  if [ -z "$chromium_exe" ] || [ ! -x "$chromium_exe" ]; then
    dx_info "installing Playwright chromium into $browsers_cache (missing/mismatched executable)"
    ( cd "$validate_dir" && npx playwright install chromium )
  fi
}

case "$SUBCOMMAND" in
  capture)
    dx_require_local "$TARGET_NAME"

    URL=""
    OUT=""
    ROUTES="all"
    WIDTHS=""
    STATE_ADMIN=""
    STATE_USER=""
    KEY="${QUASAR_DEV_AGENT_KEY:-}"

    while [ $# -gt 0 ]; do
      case "$1" in
        --url) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--url requires a value"; URL="$2"; shift 2 ;;
        --out) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--out requires a value"; OUT="$2"; shift 2 ;;
        --routes) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--routes requires a value"; ROUTES="$2"; shift 2 ;;
        --widths) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--widths requires a value"; WIDTHS="$2"; shift 2 ;;
        --state-admin) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--state-admin requires a value"; STATE_ADMIN="$2"; shift 2 ;;
        --state-user) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--state-user requires a value"; STATE_USER="$2"; shift 2 ;;
        --key) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--key requires a value"; KEY="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) dx_guard "$TARGET_NAME" "unknown arg '$1' — see: scripts/dx/uiaudit.sh capture --help" ;;
      esac
    done

    [ -n "$URL" ] || dx_guard "$TARGET_NAME" "--url is required"
    URL="${URL%/}"

    STAMP="$(dx_timestamp)"
    if [ -z "$OUT" ]; then
      OUT="$DX_UIAUDIT_DIR/${STAMP}"
    fi
    mkdir -p "$OUT"

    STATE_DIR="$OUT/.state"
    mkdir -p "$STATE_DIR"
    chmod 700 "$STATE_DIR" 2>/dev/null || true

    # Determine, from the route manifest, whether admin/user sessions are
    # actually needed for the requested routes — no point minting a session
    # (and requiring a key) for an unauth-only run.
    NEEDS_ADMIN=0
    NEEDS_USER=0
    if [ "$ROUTES" = "all" ]; then
      NEEDS_ADMIN=1
      NEEDS_USER=1
    else
      if dx_have python3; then
        NEEDED_ROLES="$(python3 - "$UIAUDIT_SCRIPTS_DIR/routes.json" "$ROUTES" <<'PY'
import json, sys
routes_path, wanted = sys.argv[1], sys.argv[2].split(',')
with open(routes_path, encoding='utf-8') as f:
    routes = json.load(f)['routes']
wanted = set(w.strip() for w in wanted if w.strip())
roles = set()
for r in routes:
    if r['id'] in wanted:
        roles.add(r['role'])
print(' '.join(sorted(roles)))
PY
)"
        dx_word_in admin "$NEEDED_ROLES" && NEEDS_ADMIN=1
        dx_word_in user "$NEEDED_ROLES" && NEEDS_USER=1
      else
        # No python3 to introspect routes.json — mint both, harmless if unused.
        NEEDS_ADMIN=1
        NEEDS_USER=1
      fi
    fi

    if [ -z "$STATE_ADMIN" ] && [ "$NEEDS_ADMIN" = "1" ]; then
      STATE_ADMIN="$STATE_DIR/admin.json"
      dx_info "minting admin dev-agent session against $URL"
      if ! bash "$DX_DIR/agentcreds.sh" --role admin --url "$URL" ${KEY:+--key "$KEY"} \
          --storage-state "$STATE_ADMIN" >/dev/null; then
        dx_fail agent-creds "failed to mint an admin session — see stderr above"
        dx_result "$TARGET_NAME"
      fi
    fi
    if [ -z "$STATE_USER" ] && [ "$NEEDS_USER" = "1" ]; then
      STATE_USER="$STATE_DIR/user.json"
      dx_info "minting user dev-agent session against $URL"
      if ! bash "$DX_DIR/agentcreds.sh" --role user --url "$URL" ${KEY:+--key "$KEY"} \
          --storage-state "$STATE_USER" >/dev/null; then
        dx_fail agent-creds "failed to mint a user session — see stderr above"
        dx_result "$TARGET_NAME"
      fi
    fi
    dx_pass agent-creds "session state ready (admin=${STATE_ADMIN:-n/a} user=${STATE_USER:-n/a})"

    bootstrap_playwright

    SHA="$(git -C "$DX_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"

    NODE_ARGS=(--url "$URL" --out "$OUT" --routes "$ROUTES" --stamp "$STAMP" --sha "$SHA")
    [ -n "$STATE_ADMIN" ] && NODE_ARGS+=(--state-admin "$STATE_ADMIN")
    [ -n "$STATE_USER" ] && NODE_ARGS+=(--state-user "$STATE_USER")
    [ -n "$WIDTHS" ] && NODE_ARGS+=(--widths "$WIDTHS")

    dx_info "capturing routes=$ROUTES against $URL -> $OUT"
    if node "$UIAUDIT_SCRIPTS_DIR/capture.mjs" "${NODE_ARGS[@]}"; then
      dx_pass capture "evidence written to $OUT"
    else
      dx_fail capture "capture.mjs reported failures — see $OUT/manifest.json"
    fi
    dx_result "$TARGET_NAME" "evidence=$OUT"
    ;;

  report)
    dx_require_local "$TARGET_NAME"

    EVIDENCE=""
    FINDINGS=""
    OUT=""

    while [ $# -gt 0 ]; do
      case "$1" in
        --evidence) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--evidence requires a value"; EVIDENCE="$2"; shift 2 ;;
        --findings) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--findings requires a value"; FINDINGS="$2"; shift 2 ;;
        --out) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--out requires a value"; OUT="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) dx_guard "$TARGET_NAME" "unknown arg '$1' — see: scripts/dx/uiaudit.sh report --help" ;;
      esac
    done

    [ -n "$EVIDENCE" ] || dx_guard "$TARGET_NAME" "--evidence is required"
    [ -d "$EVIDENCE" ] || dx_guard "$TARGET_NAME" "--evidence dir '$EVIDENCE' does not exist"
    [ -n "$OUT" ] || OUT="$EVIDENCE/report.html"

    NODE_ARGS=(--evidence "$EVIDENCE" --out "$OUT")
    [ -n "$FINDINGS" ] && NODE_ARGS+=(--findings "$FINDINGS")

    if node "$UIAUDIT_SCRIPTS_DIR/report.mjs" "${NODE_ARGS[@]}"; then
      dx_pass report "wrote $OUT"
    else
      dx_fail report "report.mjs failed"
    fi
    dx_result "$TARGET_NAME" "report=$OUT"
    ;;

  ab)
    dx_require_local "$TARGET_NAME"

    BEFORE=""
    AFTER=""
    OUT=""

    while [ $# -gt 0 ]; do
      case "$1" in
        --before) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--before requires a value"; BEFORE="$2"; shift 2 ;;
        --after) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--after requires a value"; AFTER="$2"; shift 2 ;;
        --out) [ $# -ge 2 ] || dx_guard "$TARGET_NAME" "--out requires a value"; OUT="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) dx_guard "$TARGET_NAME" "unknown arg '$1' — see: scripts/dx/uiaudit.sh ab --help" ;;
      esac
    done

    [ -n "$BEFORE" ] || dx_guard "$TARGET_NAME" "--before is required"
    [ -n "$AFTER" ] || dx_guard "$TARGET_NAME" "--after is required"
    [ -d "$BEFORE" ] || dx_guard "$TARGET_NAME" "--before dir '$BEFORE' does not exist"
    [ -d "$AFTER" ] || dx_guard "$TARGET_NAME" "--after dir '$AFTER' does not exist"
    [ -n "$OUT" ] || OUT="$DX_UIAUDIT_DIR/ab-$(dx_timestamp).html"

    if node "$UIAUDIT_SCRIPTS_DIR/ab.mjs" --before "$BEFORE" --after "$AFTER" --out "$OUT"; then
      dx_pass ab "wrote $OUT"
    else
      dx_warn ab "ab.mjs reported regressions or failed — see $OUT"
    fi
    dx_result "$TARGET_NAME" "report=$OUT"
    ;;

  # ── The two `make` entry points ────────────────────────────────────────────
  # `make ui-audit` used to build the capture+report command line itself, with
  # URL/OUT/ROUTES/KEY interpolated into the recipe (#550) — where a `"` or a
  # backtick in any of them was live. The knobs now arrive as ENVIRONMENT
  # variables and become elements of a bash array, which no shell re-parses, so
  # a value may contain anything (a path with a space included) without being
  # code. There is no separate `make-routes`: `make ui-audit-routes` is the same
  # verb with ROUTES set.
  make-audit)
    OUT="${OUT:-}"
    [ -n "$OUT" ] || OUT="$DX_UIAUDIT_DIR/$(dx_timestamp)"
    CAPTURE_ARGV=(capture --url "${URL:-}" --out "$OUT" --routes "${ROUTES:-all}")
    if [ -n "${KEY:-}" ]; then CAPTURE_ARGV+=(--key "$KEY"); fi
    bash "$0" "${CAPTURE_ARGV[@]}"
    bash "$0" report --evidence "$OUT"
    ;;

  make-ab)
    AB_ARGV=(ab --before "${BEFORE:-}" --after "${AFTER:-}")
    if [ -n "${OUT:-}" ]; then AB_ARGV+=(--out "$OUT"); fi
    bash "$0" "${AB_ARGV[@]}"
    ;;

  -h|--help)
    usage; exit 0 ;;

  *)
    dx_guard "$TARGET_NAME" "unknown subcommand '$SUBCOMMAND' (capture|report|ab)" ;;
esac
