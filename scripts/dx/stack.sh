#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/stack.sh — operate a stack, local or remote.
#
#   scripts/dx/stack.sh <up|down|restart|rebuild|redeploy-cp|status|health|logs|logs-follow>
#
# HOST selects the target:
#   HOST unset / HOST=local   the agentless local stack (docker-compose.local.yml,
#                             project-scoped to $QUASAR_INSTANCE)
#   HOST=<role-or-host>       a fleet box resolved from .claude/skills/_shared/
#                             hosts.json (role name, e.g. gpu-test, or host name)
#
# Remote rules (enforced in common.sh):
#   * only status/health/logs/logs-follow/rebuild/redeploy-cp/up/down/restart may
#     target it
#   * every MUTATING verb (up/down/restart/rebuild/redeploy-cp) refuses unless
#     HOST was typed explicitly, and prints which canonical script it delegates to
#
# This script never reimplements deploy/build-images.sh or deploy/redeploy.sh.
# Those are the canonical, contract-validating paths; `rebuild` and `redeploy-cp`
# call them.
#
#   rebuild      the whole stack: build-images.sh (GStreamer + Rust image set,
#                40+ minutes) then redeploy.sh at scope=all.
#   redeploy-cp  the Go control-plane ONLY: redeploy.sh at scope=control, which
#                compose-builds deploy/Dockerfile.control (~1 minute) and
#                force-recreates that one service. No image build, and the
#                node-agent container is never touched, so running sessions
#                survive. This is the right verb for a control-plane-only change
#                — reaching for `rebuild` there costs 40 minutes to rebuild Rust
#                and GStreamer that did not change.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

VERB="${1:-}"
[ -n "$VERB" ] || dx_guard stack "usage: stack.sh <up|down|restart|rebuild|redeploy-cp|status|health|logs|logs-follow>"

case "$VERB" in
  up|down|restart|rebuild|redeploy-cp|status|health|logs|logs-follow) ;;
  *) dx_guard stack "unknown verb '$VERB'" ;;
esac

dx_require_host_scope "$VERB"

LOG_LINES="${N:-200}"
LOG_SVC="${S:-}"

# Both reach a REMOTE command completely unquoted — `remote_compose` splices its
# arguments in with a bare `$*`, and logs-follow inlines the same string into an
# `exec ssh`. With no quoting layer to break out of, a `;` or `$(...)` in S= or
# N= needs no escape at all to run as the fleet ssh account. Validate here, the
# one place both are assigned, rather than at each of the three splices.
dx_require_safe "$VERB" "N" "$LOG_LINES" "$DX_RE_UINT" "N is a line count."
[ -z "$LOG_SVC" ] || dx_require_safe "$VERB" "S" "$LOG_SVC" "$DX_RE_NAME" \
  "S is a compose SERVICE name (quasar-control-plane, quasar-node-agent, quasar-postgres)."

# ─────────────────────────────────────────────────────────────────────────────
# LOCAL
# ─────────────────────────────────────────────────────────────────────────────
local_require_docker() {
  if ! dx_have docker || ! docker info >/dev/null 2>&1; then
    dx_fail docker "daemon not reachable — start Docker and retry"
    dx_result "$VERB"
  fi
}

# Map a container's State+Health onto healthy | degraded | stopped | failed.
local_status() {
  local json state health name exit_code any=0
  json="$(dx_local_compose ps --all --format json 2>/dev/null || true)"
  if [ -z "$json" ]; then
    dx_warn stack "no containers for project $QUASAR_INSTANCE — the stack is down"
    return 0
  fi
  # `docker compose ps --format json` emits one JSON object per line.
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    any=1
    name="$(printf '%s' "$line" | sed -n 's/.*"Name":"\([^"]*\)".*/\1/p')"
    state="$(printf '%s' "$line" | sed -n 's/.*"State":"\([^"]*\)".*/\1/p')"
    health="$(printf '%s' "$line" | sed -n 's/.*"Health":"\([^"]*\)".*/\1/p')"
    exit_code="$(printf '%s' "$line" | sed -n 's/.*"ExitCode":\([0-9-]*\).*/\1/p')"
    case "$state" in
      running)
        case "$health" in
          healthy|"") dx_pass "$name" "running${health:+ ($health)}" ;;
          starting)   dx_warn "$name" "running but health=starting" ;;
          *)          dx_warn "$name" "running but health=$health (degraded)" ;;
        esac
        ;;
      restarting) dx_warn "$name" "restarting — check: make logs S=$name" ;;
      exited|dead)
        if [ "${exit_code:-0}" = "0" ]; then
          dx_warn "$name" "stopped (exit 0)"
        else
          dx_fail "$name" "failed (exit ${exit_code:-?}) — check: make logs S=$name"
        fi
        ;;
      created|paused) dx_warn "$name" "state=$state (not serving)" ;;
      *)              dx_warn "$name" "state=${state:-unknown}" ;;
    esac
  done <<< "$json"
  [ "$any" -eq 1 ] || dx_warn stack "no containers for project $QUASAR_INSTANCE — the stack is down"
}

local_health() {
  local url="http://127.0.0.1:${DX_CP_PORT}/health"
  if ! dx_have curl; then
    dx_warn health "curl not on PATH — cannot probe $url"
    return 0
  fi
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$url" 2>/dev/null || true)"
  if [ "$code" = "200" ]; then
    dx_pass health "control-plane $url -> 200"
  elif [ -z "$code" ] || [ "$code" = "000" ]; then
    dx_fail health "control-plane $url unreachable — is the stack up? (make up)"
  else
    dx_fail health "control-plane $url -> $code"
  fi
}

run_local() {
  case "$VERB" in
    up)
      local_require_docker
      dx_info "project=$QUASAR_INSTANCE cp=$DX_CP_PORT tls=$DX_CP_TLS_PORT pg=$DX_PG_PORT"
      if [ ! -d "$DX_ROOT/web/dist" ]; then
        dx_warn web "web/dist does not exist — the SPA will 404; run: cd web && npm install && npm run build"
      fi
      if dx_local_compose up -d --build --wait; then
        dx_pass up "stack running — http://127.0.0.1:${DX_CP_PORT}/"
      else
        dx_fail up "compose up failed — check: make logs"
      fi
      ;;
    down)
      local_require_docker
      if dx_local_compose down --remove-orphans; then
        dx_pass down "stack stopped (volumes preserved)"
      else
        dx_fail down "compose down failed"
      fi
      ;;
    restart)
      local_require_docker
      if dx_local_compose restart; then
        dx_pass restart "stack restarted"
      else
        dx_fail restart "compose restart failed"
      fi
      ;;
    rebuild)
      local_require_docker
      dx_info "local rebuild = docker compose build --pull + up -d (the fleet path is deploy/build-images.sh)"
      if dx_local_compose build --pull && dx_local_compose up -d --wait; then
        dx_pass rebuild "images rebuilt and stack restarted"
      else
        dx_fail rebuild "rebuild failed"
      fi
      ;;
    redeploy-cp)
      local_require_docker
      # The local stack has no node-agent and no image lineage to worry about, so
      # "control-plane only" is just the one service — same shape as the remote
      # path, minus the git checkout (this worktree IS the ref).
      dx_info "local redeploy-cp = compose build + force-recreate control-plane only (project=$QUASAR_INSTANCE)"
      if dx_local_compose up -d --build --force-recreate --no-deps --wait \
           --wait-timeout 300 control-plane; then
        dx_pass redeploy-cp "control-plane rebuilt and healthy — http://127.0.0.1:${DX_CP_PORT}/"
      else
        dx_fail redeploy-cp "control-plane redeploy failed — check: make logs S=control-plane"
      fi
      ;;
    status)
      local_require_docker
      local_status
      ;;
    health)
      local_health
      ;;
    logs)
      local_require_docker
      if [ -n "$LOG_SVC" ]; then
        dx_local_compose logs --no-color --tail="$LOG_LINES" "$LOG_SVC" || true
      else
        dx_local_compose logs --no-color --tail="$LOG_LINES" || true
      fi
      dx_pass logs "tail=$LOG_LINES${LOG_SVC:+ service=$LOG_SVC}"
      ;;
    logs-follow)
      local_require_docker
      dx_info "following logs (Ctrl-C to stop) tail=$LOG_LINES${LOG_SVC:+ service=$LOG_SVC}"
      if [ -n "$LOG_SVC" ]; then
        exec docker compose -p "$QUASAR_INSTANCE" -f "$DX_LOCAL_COMPOSE" \
          logs --no-color --tail="$LOG_LINES" -f "$LOG_SVC"
      else
        exec docker compose -p "$QUASAR_INSTANCE" -f "$DX_LOCAL_COMPOSE" \
          logs --no-color --tail="$LOG_LINES" -f
      fi
      ;;
  esac
}

# ─────────────────────────────────────────────────────────────────────────────
# REMOTE
# ─────────────────────────────────────────────────────────────────────────────
remote_compose() { # remote_compose <compose args...>
  # The fleet compose files live under <repo>/deploy on the host, and their
  # relative paths (bind mounts, env_file) resolve from there.
  dx_ssh_remote "cd '$DX_REMOTE_DIR/deploy' && docker compose $(dx_remote_compose_args) $*"
}

# remote_redeploy_args — resolve the two positional arguments every redeploy.sh
# invocation needs, into DX_REDEPLOY_LABEL and DX_REDEPLOY_REF. Returns non-zero
# (having already recorded a dx_fail) when either cannot be resolved.
#
# These set globals rather than echoing, deliberately: a `x="$(helper)"` form runs
# the helper in a SUBSHELL, so its dx_fail would bump DX_FAIL_N in a process that
# is about to exit and the run would still report status=ok.
#
# The hardware profile is MANDATORY and positional; omitting it prints usage and
# fails the target AFTER any image build has already succeeded, which used to
# cost a full rebuild every time.
#

# THE REF IS MANDATORY, for a reason that is not obvious from redeploy.sh's usage
# line: its `ref` argument DEFAULTS TO origin/main, and redeploy.sh git-checkouts
# that ref on the host. Calling it bare therefore silently drags the host off
# whatever branch is under test and back onto main — mid-run, while bash is still
# reading redeploy.sh itself, so the rest of the deploy executes main's copy of
# the script. The visible symptom is a deploy that fails with values nothing in
# the branch contains, and a host whose checkout reverts after every verified
# `git checkout`.
#
# Default to the branch the host is ALREADY on, so a redeploy ships what is being
# tested rather than main. Override with REF=<ref> for a deliberate move. A
# detached HEAD has no branch name to send, so send the exact commit instead.
#
# EVERY caller of redeploy.sh in this file goes through here. A new one that
# hand-rolls its arguments is how the trap comes back.
DX_REDEPLOY_LABEL=""
DX_REDEPLOY_REF=""
remote_redeploy_args() {
  DX_REDEPLOY_LABEL="$DX_REMOTE_REDEPLOY_LABEL"
  if [ -z "$DX_REDEPLOY_LABEL" ]; then
    dx_fail "$VERB" "$DX_REMOTE_NAME has no redeploy_label or gpu in hosts.json — redeploy.sh needs va|nvidia"
    return 1
  fi

  DX_REDEPLOY_REF="${REF:-}"
  if [ -z "$DX_REDEPLOY_REF" ]; then
    DX_REDEPLOY_REF="$(dx_ssh_remote "cd '$DX_REMOTE_DIR' && git rev-parse --abbrev-ref HEAD" 2>/dev/null | tr -d '\r\n')"
    if [ -z "$DX_REDEPLOY_REF" ] || [ "$DX_REDEPLOY_REF" = "HEAD" ]; then
      DX_REDEPLOY_REF="$(dx_ssh_remote "cd '$DX_REMOTE_DIR' && git rev-parse HEAD" 2>/dev/null | tr -d '\r\n')"
    fi
  fi
  if [ -z "$DX_REDEPLOY_REF" ]; then
    dx_fail "$VERB" "could not resolve $DX_REMOTE_NAME's current ref — pass REF=<branch|sha>"
    return 1
  fi

  # The ref is about to be interpolated into
  #   ssh <host> "cd '<dir>' && bash deploy/redeploy.sh '<label>' '<ref>' all"
  # A single quote in it closes the quoting and the remainder executes as the
  # fleet ssh account, which has docker access — host root. So the ref must be
  # proven to be a ref before it is sent, not merely wrapped in quotes.
  #
  # Both sources are checked, not just REF=. The fallback reads `git rev-parse`
  # output back off the HOST, and a value that arrives from the far side of an
  # ssh hop has no more claim to be well-formed than one typed locally.
  #
  # Not resolved to a sha first, deliberately: the whole point of the fallback
  # is to deploy the branch the HOST is on, which may not exist in this clone —
  # `git rev-parse` here would either fail or, worse, resolve a same-named local
  # branch at a different commit and deploy that.
  dx_require_safe "$VERB" "REF" "$DX_REDEPLOY_REF" "$DX_RE_REF" \
    "A ref is a branch, tag or sha. It is passed to redeploy.sh, which git-checks-it-out on $DX_REMOTE_NAME."
  dx_require_safe "$VERB" "redeploy label" "$DX_REDEPLOY_LABEL" "$DX_RE_NAME" "Expected va or nvidia."
}

run_remote() {
  dx_announce_remote_delegation "$VERB"
  if ! dx_have ssh; then
    dx_fail ssh "not on PATH — cannot reach $DX_REMOTE_NAME"
    dx_result "$VERB"
  fi
  case "$VERB" in
    up|down|restart)
      local remote_verb="$VERB"
      if [ "$VERB" = "up" ]; then remote_verb="up -d"; fi
      if remote_compose "$remote_verb"; then
        dx_pass "$VERB" "$DX_REMOTE_NAME stack $VERB completed"
      else
        dx_fail "$VERB" "$DX_REMOTE_NAME compose $VERB failed"
      fi
      ;;
    rebuild)
      # Never a hand-typed docker build: build-images.sh forces an explicit
      # --target, rejects undeclared --build-arg, and validates every artifact
      # against deploy/image-contract.json before promoting :latest.
      remote_redeploy_args || return 0
      local label="$DX_REDEPLOY_LABEL" ref="$DX_REDEPLOY_REF"
      dx_info "$DX_REMOTE_NAME rebuild deploying ref '$ref' (override with REF=<ref>)"
      if dx_ssh_remote "cd '$DX_REMOTE_DIR' && bash deploy/build-images.sh && bash deploy/redeploy.sh '$label' '$ref' all"; then
        dx_pass rebuild "$DX_REMOTE_NAME images rebuilt and redeployed"
      else
        dx_fail rebuild "$DX_REMOTE_NAME rebuild failed — read the build-images.sh contract output; never relax an assertion to make it green"
      fi
      ;;
    redeploy-cp)
      # Control-plane only. NO build-images.sh: the quasar-control-plane compose
      # service builds from deploy/Dockerfile.control, which shares nothing with
      # the Dockerfile.vulkan lineage that build-images.sh + image-contract.json
      # govern. There is no image to validate against the contract here, and
      # running the full builder for a Go-only change is the 40-minute cost this
      # verb exists to avoid.
      remote_redeploy_args || return 0
      local cp_label="$DX_REDEPLOY_LABEL" cp_ref="$DX_REDEPLOY_REF"
      dx_info "$DX_REMOTE_NAME redeploy-cp deploying ref '$cp_ref' (override with REF=<ref>)"
      if dx_ssh_remote "cd '$DX_REMOTE_DIR' && bash deploy/redeploy.sh '$cp_label' '$cp_ref' control"; then
        dx_pass redeploy-cp "$DX_REMOTE_NAME control-plane rebuilt and redeployed at '$cp_ref'"
      else
        dx_fail redeploy-cp "$DX_REMOTE_NAME control-plane redeploy failed — read redeploy.sh's 7/7 verify output above"
      fi
      # Independent confirmation, over the host's OWN compose file list from
      # hosts.json (never a hardcoded -f): redeploy.sh reports on the stack it
      # thinks it deployed, this reports on the containers actually running.
      local cp_ps
      cp_ps="$(remote_compose ps quasar-control-plane 2>&1 || true)"
      printf '%s\n' "$cp_ps"
      if printf '%s' "$cp_ps" | grep -qi 'unhealthy\|restarting\|exited\|dead'; then
        dx_fail redeploy-cp "$DX_REMOTE_NAME control-plane container is not healthy after the redeploy"
      elif printf '%s' "$cp_ps" | grep -qi 'up '; then
        dx_pass redeploy-cp-container "$DX_REMOTE_NAME quasar-control-plane is up"
      else
        dx_fail redeploy-cp-container "could not read $DX_REMOTE_NAME's control-plane container state"
      fi
      ;;
    status)
      local out
      out="$(remote_compose ps --all 2>&1 || true)"
      printf '%s\n' "$out"
      if printf '%s' "$out" | grep -qi 'unhealthy\|restarting'; then
        dx_warn status "$DX_REMOTE_NAME reports an unhealthy or restarting container"
      elif printf '%s' "$out" | grep -qi 'exited\|dead'; then
        dx_warn status "$DX_REMOTE_NAME reports a stopped container"
      elif printf '%s' "$out" | grep -qi 'up '; then
        dx_pass status "$DX_REMOTE_NAME containers up"
      else
        dx_fail status "could not read $DX_REMOTE_NAME container state"
      fi
      ;;
    health)
      # The URL comes from hosts.json `api` (the host-local, published-port URL),
      # never a hardcoded port. This probed http://localhost:8080 — the CONTAINER
      # port — so it reported "unreachable" against a perfectly healthy fleet
      # stack whose control plane publishes 18443/18080. A health verb that
      # fails on a healthy stack is worse than no health verb: it trains you to
      # ignore it. -k because fleet certs are self-signed (see the LAN-cert
      # note in docs); this checks reachability, not trust.
      local health_url="${DX_REMOTE_API:-}"
      if [ -z "$health_url" ]; then
        dx_fail health "$DX_REMOTE_NAME has no \`api\` in $DX_HOSTS_JSON — cannot probe health without guessing a port"
      elif dx_ssh_remote "curl -fsSk --max-time 5 '$health_url/health' >/dev/null"; then
        dx_pass health "$DX_REMOTE_NAME control-plane $health_url/health -> 200"
      else
        dx_fail health "$DX_REMOTE_NAME control-plane $health_url/health unreachable"
      fi
      ;;
    logs)
      # Always bounded — an unbounded remote log pull is how a session eats its
      # own context window. S= takes the COMPOSE SERVICE name (quasar-control-plane,
      # quasar-node-agent, quasar-postgres), same vocabulary as HOST=local.
      # No `|| true` + unconditional PASS here: a failed pull must FAIL — the
      # first live validation printed PASS over a docker error, which is exactly
      # the contradictory-success output this tooling exists to forbid.
      if remote_compose "logs --no-color --tail=${LOG_LINES} ${LOG_SVC}"; then
        dx_pass logs "tail=$LOG_LINES${LOG_SVC:+ service=$LOG_SVC}"
      else
        dx_fail logs "$DX_REMOTE_NAME log pull failed${LOG_SVC:+ — is '$LOG_SVC' a compose service name?}"
      fi
      ;;
    logs-follow)
      dx_info "following $DX_REMOTE_NAME logs (Ctrl-C to stop) tail=$LOG_LINES"
      # exec needs a real command, not a shell function — dx_ssh_remote's body
      # inlined here so the ssh process replaces this shell.
      if [ -n "${DX_REMOTE_SSH_ALIAS:-}" ]; then
        exec ssh -o ConnectTimeout="${DX_SSH_TIMEOUT:-10}" -o BatchMode=yes \
          -o StrictHostKeyChecking=accept-new "$DX_REMOTE_SSH_ALIAS" \
          "cd '$DX_REMOTE_DIR/deploy' && docker compose $(dx_remote_compose_args) logs --no-color --tail=${LOG_LINES} -f ${LOG_SVC}"
      else
        exec ssh -o IdentityAgent=none -o ConnectTimeout="${DX_SSH_TIMEOUT:-10}" \
          -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
          -i "$DX_REMOTE_KEY" "${DX_REMOTE_USER}@${DX_REMOTE_HOST}" \
          "cd '$DX_REMOTE_DIR/deploy' && docker compose $(dx_remote_compose_args) logs --no-color --tail=${LOG_LINES} -f ${LOG_SVC}"
      fi
      ;;
  esac
}

if [ "$DX_HOST" != "local" ]; then run_remote; else run_local; fi

dx_result "$VERB" "cp_port=$DX_CP_PORT"
