# shellcheck shell=bash
# _shared/lib.sh — host resolution for every Quasar skill script.
#
# Source this near the top of a skill script:
#     source "$(dirname "$0")/../../_shared/lib.sh"
#
# Skills speak in ROLES ("gpu-test", "aux-infra", "deploy-only"); the operator's
# _shared/hosts.json maps each role to a host and holds every host-specific fact.
# Nothing below hardcodes a host name, address, key path or hardware detail.
#
# Public API:
#   q_hosts                          list configured host names
#   q_roles                          list configured role names
#   q_role <role>                    host name bound to a role ("" if unbound)
#   q_resolve <role|host>            -> host name (role wins, then literal host name)
#   q_default_host                   host for $QUASAR_DEFAULT_ROLE (default: gpu-test)
#   q_host_cfg <role|host> <field>   read a host field ('~' expanded, "" if absent)
#   q_host_notes <role|host>         operator notes for a host, one per line
#   q_cfg <section> <key>            read a non-host section (harness, netem, ...)
#   q_ssh <role|host> [cmd...]       ssh to a host (headless; connection-multiplexed)
#   q_scp <role|host> <local> <dst>  copy a file to a host
#   q_ssh_prefix <role|host>         the ssh command prefix as a string (for python callers)
#
# Role safety (the deploy-only contract — see hosts.example.json):
#   q_has_role <role|host> <role>                          does that host hold the role?
#   q_guard_mutating <tool> <role|host> <0|1> <verb> [sev]  refuse/gate a write verb
#
# Exported convenience env (role-derived; an explicit env override always wins):
#   QUASAR_GPU_HOST/_SSH/_DIR/_IP/_API/_API_EXTERNAL/_AGENT_API/_IMAGE   (role gpu-test)
#   QUASAR_AUX_HOST/_SSH/_DIR/_IP/_API/_API_EXTERNAL/_AGENT_API/_IMAGE   (role aux-infra)
_QUASAR_SHARED_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUASAR_HOSTS_JSON="${QUASAR_HOSTS_JSON:-$_QUASAR_SHARED_DIR/hosts.json}"
QUASAR_DEFAULT_ROLE="${QUASAR_DEFAULT_ROLE:-gpu-test}"

if ! command -v python3 >/dev/null 2>&1; then
  echo "quasar skills: python3 is required to read $QUASAR_HOSTS_JSON" >&2
  return 1 2>/dev/null || exit 1
fi
if [ ! -f "$QUASAR_HOSTS_JSON" ]; then
  echo "quasar skills: host config not found: $QUASAR_HOSTS_JSON" >&2
  echo "quasar skills: copy _shared/hosts.example.json to hosts.json and fill it in." >&2
  return 1 2>/dev/null || exit 1
fi

# ── Config readers ───────────────────────────────────────────────────────────

q_hosts() {
  python3 - "$QUASAR_HOSTS_JSON" <<'PY'
import json, sys
print("\n".join(sorted(json.load(open(sys.argv[1])).get("hosts", {}))))
PY
}

q_roles() {
  python3 - "$QUASAR_HOSTS_JSON" <<'PY'
import json, sys
roles = json.load(open(sys.argv[1])).get("roles", {})
print("\n".join(sorted(k for k in roles if not k.startswith("_"))))
PY
}

q_role() {
  python3 - "$QUASAR_HOSTS_JSON" "${1:-}" <<'PY'
import json, sys
print(json.load(open(sys.argv[1])).get("roles", {}).get(sys.argv[2], "") or "")
PY
}

# Accept a role name or a literal host name; print the host name.
# Unknown names print nothing and return 2, so callers can fail loudly.
q_resolve() {
  python3 - "$QUASAR_HOSTS_JSON" "${1:-}" <<'PY'
import json, sys
cfg = json.load(open(sys.argv[1]))
want = sys.argv[2]
name = cfg.get("roles", {}).get(want) or want
if name in cfg.get("hosts", {}):
    print(name)
    sys.exit(0)
sys.exit(2)
PY
}

q_default_host() { q_resolve "$QUASAR_DEFAULT_ROLE"; }

q_host_cfg() {
  python3 - "$QUASAR_HOSTS_JSON" "${1:-}" "${2:-}" <<'PY'
import json, os, sys
cfg = json.load(open(sys.argv[1]))
name = cfg.get("roles", {}).get(sys.argv[2]) or sys.argv[2]
val = cfg.get("hosts", {}).get(name, {}).get(sys.argv[3], "")
if isinstance(val, str):
    print(os.path.expanduser(val))
elif isinstance(val, (list, tuple)):
    print("\n".join(str(v) for v in val))
elif val is None:
    print("")
else:
    print(val)
PY
}

q_host_notes() { q_host_cfg "$1" notes; }

# ── Role safety ──────────────────────────────────────────────────────────────
# hosts.example.json promises that a `deploy-only` host takes read-only verbs
# only. That promise was enforced in exactly one script (qhost) and ignored by
# the other four, so it lives here now: every skill script already sources this
# file, which makes the guard impossible to forget by omission and impossible to
# drift between copies.

q_has_role() {  # q_has_role <role|host> <role> -> true if that host holds the role
  local host
  host="$(q_resolve "${1:-}" 2>/dev/null)" || return 1
  [ -n "$host" ] && [ "$(q_role "${2:-}")" = "$host" ]
}

# q_guard_mutating <tool> <role|host> <explicit 0|1> <verb> [mutating|destructive]
#
# Two severities, graded the way qhost has always graded them:
#   mutating     changes state that can be put back (start a stack, build an
#                image, add a qdisc). Allowed against a deploy-only host only
#                when the caller NAMED that host — an accidental default must
#                never land there.
#   destructive  removes or overwrites something that cannot be handed back
#                (tear a stack down, force-reset a checkout, delete a qdisc
#                nobody can restore, mutate a live .env). Refused outright; no
#                flag confirms it.
# Exits 2 on refusal, like every other usage error in these skills.
q_guard_mutating() {
  local tool="${1:-skill}" host explicit="${3:-0}" verb="${4:-}" sev="${5:-mutating}"
  host="$(q_resolve "${2:-}" 2>/dev/null)" || return 0
  q_has_role "$host" deploy-only || return 0
  if [ "$sev" = destructive ]; then
    echo "$tool: refusing '$verb' — '$host' holds the deploy-only role (no destructive operations)." >&2
    exit 2
  fi
  if [ "$explicit" != 1 ]; then
    echo "$tool: '$verb' mutates '$host', which holds the deploy-only role — pass --host $host explicitly to confirm." >&2
    exit 2
  fi
}

# Non-host sections (harness, netem, ...).
q_cfg() {
  python3 - "$QUASAR_HOSTS_JSON" "${1:-}" "${2:-}" <<'PY'
import json, os, sys
cfg = json.load(open(sys.argv[1]))
val = cfg.get(sys.argv[2], {}).get(sys.argv[3], "")
print(os.path.expanduser(val) if isinstance(val, str) else val)
PY
}

# ── SSH ──────────────────────────────────────────────────────────────────────
# Two connection forms, both headless and connection-multiplexed:
#   ssh_alias                      a ~/.ssh/config entry that already works headlessly
#   ssh_host + ssh_user + ssh_key  explicit key auth with IdentityAgent=none, so an
#                                  interactive agent can never block an unattended run
_q_ssh_argv() {  # $1 = role|host ; prints one ssh argument per line, host spec last
  local host alias_ h u k
  host="$(q_resolve "${1:-}")" || return 2
  alias_="$(q_host_cfg "$host" ssh_alias)"
  if [ -n "$alias_" ]; then
    printf '%s\n' -o ConnectTimeout=15 -o ControlMaster=auto \
      -o "ControlPath=$HOME/.ssh/cm-q-%r@%h:%p" -o ControlPersist=8h "$alias_"
    return 0
  fi
  h="$(q_host_cfg "$host" ssh_host)"
  u="$(q_host_cfg "$host" ssh_user)"
  k="$(q_host_cfg "$host" ssh_key)"
  if [ -z "$h" ]; then
    echo "q_ssh: host '$host' has no ssh_alias and no ssh_host in $QUASAR_HOSTS_JSON" >&2
    return 2
  fi
  [ -n "$k" ] && printf '%s\n' -i "$k" -o IdentityAgent=none -o IdentitiesOnly=yes
  printf '%s\n' -o StrictHostKeyChecking=accept-new -o ControlMaster=auto \
    -o "ControlPath=$HOME/.ssh/cm-q-%r@%h:%p" -o ControlPersist=8h \
    -o ConnectTimeout=15 "${u:+$u@}$h"
}

q_ssh() {
  local host="${1:-}"; shift || true
  local args=() a
  if ! q_resolve "$host" >/dev/null 2>&1; then
    echo "q_ssh: unknown host or role '$host' (hosts: $(q_hosts | tr '\n' ' ')| roles: $(q_roles | tr '\n' ' '))" >&2
    return 2
  fi
  while IFS= read -r a; do args+=("$a"); done < <(_q_ssh_argv "$host")
  ssh "${args[@]}" "$@"
}

q_scp() {  # q_scp <role|host> <local> <remote-path>
  local host="${1:-}" src="${2:-}" dst="${3:-}"
  local args=() a spec last
  if ! q_resolve "$host" >/dev/null 2>&1; then
    echo "q_scp: unknown host or role '$host'" >&2
    return 2
  fi
  while IFS= read -r a; do args+=("$a"); done < <(_q_ssh_argv "$host")
  last=$(( ${#args[@]} - 1 ))
  spec="${args[$last]}"
  unset "args[$last]"
  scp "${args[@]}" "$src" "$spec:$dst"
}

# Printable ssh command prefix — for python (and other) callers that shell out.
q_ssh_prefix() {
  local host="${1:-}" out a
  if ! q_resolve "$host" >/dev/null 2>&1; then
    echo "q_ssh_prefix: unknown host or role '$host'" >&2
    return 2
  fi
  out="ssh"
  while IFS= read -r a; do out="$out $(printf '%q' "$a")"; done < <(_q_ssh_argv "$host")
  printf '%s' "$out"
}

# ── Role-derived env exports (an explicit env override always wins) ──────────
_q_export_role() {  # $1 = role, $2 = env prefix
  local host field name value pair
  host="$(q_resolve "$1" 2>/dev/null)" || return 0
  eval "export ${2}_HOST=\"\${${2}_HOST:-\$host}\""
  for pair in SSH:@prefix DIR:dir IP:ip API:api API_EXTERNAL:api_external \
              AGENT_API:agent_api IMAGE:runtime_image; do
    name="${pair%%:*}"; field="${pair#*:}"
    if [ "$field" = "@prefix" ]; then
      value="$(q_ssh_prefix "$host")"
    else
      value="$(q_host_cfg "$host" "$field")"
    fi
    eval "export ${2}_${name}=\"\${${2}_${name}:-\$value}\""
  done
}
_q_export_role gpu-test  QUASAR_GPU
_q_export_role aux-infra QUASAR_AUX
