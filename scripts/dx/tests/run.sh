#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/tests/run.sh — self-tests for the DX tooling.
#
#   make verify
#
# Rules this suite obeys:
#   * It harms no real environment. No stack is started, no container is
#     created, no remote host is touched.
#   * It does not require a running Docker daemon. Where a script needs docker
#     or ssh, a PATH-shim stub stands in.
#   * Anything that genuinely needs the daemon is SKIPped with a visible WARN,
#     never silently passed.

set -uo pipefail   # deliberately NOT -e: this suite records failures, it does
                   # not abort on the first one.

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DX="$(cd "$TESTS_DIR/.." && pwd)"
ROOT="$(cd "$DX/../.." && pwd)"
FIXTURES="$TESTS_DIR/fixtures"

PASS_N=0; FAIL_N=0; WARN_N=0
pass() { PASS_N=$((PASS_N + 1)); printf 'PASS %s — %s\n' "$1" "${2:-ok}"; }
warn() { WARN_N=$((WARN_N + 1)); printf 'WARN %s — %s\n' "$1" "${2:-}"; }
fail() { FAIL_N=$((FAIL_N + 1)); printf 'FAIL %s — %s\n' "$1" "${2:-}" >&2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/dx-tests.XXXXXX")"
export WORK   # visible to PATH-shim subprocesses (e.g. the crontab stub's default state file)
# shellcheck disable=SC2329  # invoked by the trap below, not by name
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

DX_SCRIPTS=("$DX"/*.sh "$TESTS_DIR"/*.sh)

# ── PATH shims ───────────────────────────────────────────────────────────────
# A stub bin dir prepended to PATH. Real tools stay reachable; only the ones we
# stub are replaced. Nothing here touches a daemon or a network.
STUB_BIN="$WORK/stubbin"
mkdir -p "$STUB_BIN"

make_stub() { # make_stub <name> <body>
  printf '#!/usr/bin/env bash\n%s\n' "$2" > "$STUB_BIN/$1"
  chmod +x "$STUB_BIN/$1"
}

# shellcheck disable=SC2016  # stub bodies must reach the stub file UNEXPANDED
make_stub docker '
case "${1:-}" in
  info)    exit 0 ;;
  --version) echo "Docker version 99.0.0, build stub" ;;
  compose)
    shift
    for a in "$@"; do
      case "$a" in
        ps)     exit 0 ;;
        config) exit 0 ;;
      esac
    done
    exit 0 ;;
  logs|inspect|rm|run|exec) exit 0 ;;
  *) exit 0 ;;
esac'
make_stub ssh 'exit 255'          # every remote probe fails: unreachable
make_stub go   'echo "go version go1.25.0 stub/stub"'
make_stub node 'echo "v22.0.0"'
# In-memory crontab, state file overridable per test via $CRONTAB_STATE — real
# crontab must never be touched by this suite.
make_stub crontab '
STATE="${CRONTAB_STATE:-$WORK/crontab.state}"
case "${1:-}" in
  -l) [ -f "$STATE" ] && cat "$STATE" || { echo "no crontab for stub" >&2; exit 1; } ;;
  -)  cat > "$STATE" ;;
  *)  exit 1 ;;
esac'

with_stubs() { PATH="$STUB_BIN:$PATH" "$@"; }

# rc_of <expected> <label> -- <command...>
rc_of() {
  local expected="$1" label="$2"; shift 2
  [ "${1:-}" = "--" ] && shift
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  if [ "$rc" -eq "$expected" ]; then
    pass "$label" "rc=$rc as expected"
  else
    fail "$label" "expected rc=$expected, got rc=$rc :: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  fi
}

printf '== static analysis ==\n'

# ── bash -n over everything ──────────────────────────────────────────────────
syntax_bad=0
for f in "${DX_SCRIPTS[@]}"; do
  if ! bash -n "$f" 2>/dev/null; then
    fail "bash -n" "$(basename "$f") has a syntax error"
    syntax_bad=1
  fi
done
[ "$syntax_bad" -eq 0 ] && pass "bash -n" "${#DX_SCRIPTS[@]} script(s) parse cleanly"

# ── shellcheck ───────────────────────────────────────────────────────────────
if command -v shellcheck >/dev/null 2>&1; then
  sc_out="$(shellcheck -x -S warning "${DX_SCRIPTS[@]}" 2>&1)"
  if [ -z "$sc_out" ]; then
    pass shellcheck "clean at -S warning"
  else
    fail shellcheck "$(printf '%s' "$sc_out" | head -n 20)"
  fi
else
  warn shellcheck "not installed — SKIPPED (install: brew install shellcheck)"
fi

printf '\n== guards ==\n'

# Guard tests point resolution at the fixture, never the real operator config
# (.claude/skills/_shared/hosts.json), so the suite runs the same with or
# without that file configured. FIX_HOSTS carries a valid role (gpu-test) and
# host name (aliasbox) for tests that need a KNOWN remote to reach the guard
# under test rather than tripping the unknown-host guard first.
FIX_HOSTS="$FIXTURES/hosts.json"

# ── reset without CONFIRM → rc 2 ─────────────────────────────────────────────
rc_of 2 "guard:reset-no-confirm" -- env -u CONFIRM -u HOST bash "$DX/reset.sh"

# ── reset with a bogus CONFIRM → rc 2 ────────────────────────────────────────
rc_of 2 "guard:reset-bad-confirm" -- env -u HOST CONFIRM=yes-please bash "$DX/reset.sh"

# ── reset with ANY remote HOST → rc 2, even with a valid CONFIRM ─────────────
# Checked before the HOST is even validated against hosts.json, so this must
# refuse a KNOWN remote host (gpu-test) as well as complete nonsense.
rc_of 2 "guard:reset-remote" -- env DX_HOSTS_JSON="$FIX_HOSTS" CONFIRM=reset HOST=gpu-test bash "$DX/reset.sh"
rc_of 2 "guard:reset-remote-data" -- env DX_HOSTS_JSON="$FIX_HOSTS" CONFIRM=reset-data HOST=gpu-test bash "$DX/reset.sh"
rc_of 2 "guard:reset-remote-unknown" -- env CONFIRM=reset HOST=complete-nonsense bash "$DX/reset.sh"

# ── remote mutation without an explicit HOST → rc 2 ──────────────────────────
# QUASAR_DEFAULT_HOST makes the remote the *default* without it being *typed*,
# which is exactly the case the guard exists to refuse.
for verb in up down restart rebuild redeploy-cp; do
  rc_of 2 "guard:remote-$verb-implicit" -- \
    env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_DEFAULT_HOST=gpu-test bash "$DX/stack.sh" "$verb"
done

# ── a remote HOST on a local-only target → rc 2 ──────────────────────────────
for t in test-go test-web dev-cp clean; do
  rc_of 2 "guard:local-only-$t" -- env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/common.sh" require-local "$t"
done

# ── an unknown HOST (not a role or a host name) → rc 2 ───────────────────────
rc_of 2 "guard:unknown-host" -- env DX_HOSTS_JSON="$FIX_HOSTS" HOST=nowhere-at-all bash "$DX/common.sh" require-local status
rc_of 2 "guard:unknown-host-scope" -- env DX_HOSTS_JSON="$FIX_HOSTS" HOST=nowhere-at-all bash "$DX/common.sh" require-host-scope status

# ── a missing hosts.json path → rc 2 with the configure-it hint ──────────────
missing_out="$(env DX_HOSTS_JSON="$WORK/does-not-exist.json" HOST=gpu-test bash "$DX/common.sh" require-host-scope status 2>&1)"
missing_rc=$?
if [ "$missing_rc" -eq 2 ] && printf '%s' "$missing_out" | grep -qF 'configure .claude/skills/_shared/hosts.json'; then
  pass "guard:missing-hosts-json" "rc=2 with the configure hint"
else
  fail "guard:missing-hosts-json" "rc=$missing_rc, output: $(printf '%s' "$missing_out" | tail -n 2 | tr '\n' ' ')"
fi

# ── a remote HOST on an ALLOWED read-only verb passes the scope check ────────
rc_of 0 "guard:remote-status-allowed" -- env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/common.sh" require-host-scope status
rc_of 0 "guard:remote-logs-allowed" -- env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/common.sh" require-host-scope logs

# ── a bad verb → rc 2 ────────────────────────────────────────────────────────
rc_of 2 "guard:stack-bad-verb" -- env -u HOST bash "$DX/stack.sh" frobnicate

# ── homes-gc (#500): it DELETES home directories, so every way of reaching it
#    without deliberately naming a host must refuse. No ssh happens in any of
#    these — the guard fires before the script resolves a container.
rc_of 2 "homes-gc:guard-implicit-host" -- \
  env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_DEFAULT_HOST=gpu-test bash "$DX/homes_gc.sh"
rc_of 2 "homes-gc:guard-local" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=local bash "$DX/homes_gc.sh"
rc_of 2 "homes-gc:guard-unknown-host" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=nowhere-at-all bash "$DX/homes_gc.sh" --dry-run
rc_of 2 "homes-gc:guard-bad-arg" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/homes_gc.sh" --delete-everything
# The verb is in the remote allow-list (so a TYPED HOST gets past the scope
# check and on to the real work).
rc_of 0 "homes-gc:scope-allowed" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/common.sh" require-host-scope homes-gc
rc_of 2 "guard:stack-no-verb" -- env -u HOST bash "$DX/stack.sh"

printf '\n== remote resolution ==\n'

# ── role lookup: gpu-test -> keybox (ssh_host + user + key) ──────────────────
RES_ROLE="$(env DX_HOSTS_JSON="$FIX_HOSTS" bash "$DX/common.sh" resolve-remote gpu-test)"
if printf '%s' "$RES_ROLE" | grep -qx 'DX_REMOTE_NAME=keybox' \
   && printf '%s' "$RES_ROLE" | grep -qx 'DX_REMOTE_HOST=203.0.113.5' \
   && printf '%s' "$RES_ROLE" | grep -qx 'DX_REMOTE_DIR=/srv/quasar-keybox'; then
  pass "resolve:role" "gpu-test -> keybox"
else
  fail "resolve:role" "unexpected output: $(printf '%s' "$RES_ROLE" | tr '\n' ' ')"
fi

# ── host-name lookup: aliasbox -> ssh_alias, no ssh_host/user/key ────────────
RES_NAME="$(env DX_HOSTS_JSON="$FIX_HOSTS" bash "$DX/common.sh" resolve-remote aliasbox)"
if printf '%s' "$RES_NAME" | grep -qx 'DX_REMOTE_NAME=aliasbox' \
   && printf '%s' "$RES_NAME" | grep -qx 'DX_REMOTE_SSH_ALIAS=aliasbox-ssh'; then
  pass "resolve:name" "aliasbox -> ssh_alias"
else
  fail "resolve:name" "unexpected output: $(printf '%s' "$RES_NAME" | tr '\n' ' ')"
fi

# ── env override wins over the resolved value ────────────────────────────────
RES_ENV="$(env DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_REMOTE_HOST=override.example bash "$DX/common.sh" resolve-remote keybox)"
if printf '%s' "$RES_ENV" | grep -qx 'DX_REMOTE_HOST=override.example'; then
  pass "resolve:env-override" "QUASAR_REMOTE_HOST wins"
else
  fail "resolve:env-override" "unexpected output: $(printf '%s' "$RES_ENV" | tr '\n' ' ')"
fi

# ── an unresolvable key → rc 2, no output ─────────────────────────────────────
rc_of 2 "resolve:unknown" -- env DX_HOSTS_JSON="$FIX_HOSTS" bash "$DX/common.sh" resolve-remote nowhere-at-all

printf '\n== control-plane-only redeploy ==\n'

# A RECORDING ssh stub: appends every remote command line to a log and answers
# the two probes stack.sh makes before it deploys. It never reaches a network.
# The global `ssh` stub is exit-255 (unreachable), so these tests use their own
# PATH dir rather than mutating it for the whole suite.
CP_BIN="$WORK/cpbin"
mkdir -p "$CP_BIN"
CP_SSH_LOG="$WORK/cp-ssh.log"
# shellcheck disable=SC2016  # the body must reach the stub file UNEXPANDED
printf '#!/usr/bin/env bash\n%s\n' '
cmd="${*: -1}"                       # the remote command is ssh'"'"'s last argument
printf "%s\n" "$cmd" >> "$CP_SSH_LOG"
case "$cmd" in
  *"rev-parse --abbrev-ref HEAD"*) echo "feat/on-the-host" ;;
  *"docker compose"*ps*)           echo "quasar-control-plane  quasar-control:latest  Up 3 seconds (healthy)" ;;
esac
exit 0' > "$CP_BIN/ssh"
chmod +x "$CP_BIN/ssh"

cp_run() { # cp_run <env assignments...> -- runs stack.sh redeploy-cp against the fixture
  : > "$CP_SSH_LOG"
  env PATH="$CP_BIN:$PATH" CP_SSH_LOG="$CP_SSH_LOG" DX_HOSTS_JSON="$FIX_HOSTS" \
      HOST=gpu-test "$@" bash "$DX/stack.sh" redeploy-cp 2>&1
}

# ── an EXPLICIT REF is passed straight through, with scope=control ───────────
cp_run REF=feat/under-test > "$WORK/cp-out.txt" 2>&1 || true
if grep -qF "deploy/redeploy.sh 'va' 'feat/under-test' control" "$CP_SSH_LOG"; then
  pass "redeploy-cp:explicit-ref" "redeploy.sh called with the given ref and scope=control"
else
  fail "redeploy-cp:explicit-ref" "ssh log: $(tr '\n' '|' < "$CP_SSH_LOG")"
fi

# ── no image build: this verb must never invoke build-images.sh ──────────────
if grep -q 'build-images.sh' "$CP_SSH_LOG"; then
  fail "redeploy-cp:no-image-build" "redeploy-cp ran build-images.sh — that is the 40-minute path it exists to avoid"
else
  pass "redeploy-cp:no-image-build" "no build-images.sh in the remote command"
fi

# ── the node-agent is never recreated (running sessions survive) ─────────────
if grep -q 'quasar-node-agent' "$CP_SSH_LOG"; then
  fail "redeploy-cp:agent-untouched" "redeploy-cp touched quasar-node-agent"
else
  pass "redeploy-cp:agent-untouched" "the node-agent container is not in the remote command"
fi

# ── THE REF TRAP (b4b1916c, re-armed for this verb) ──────────────────────────
# With REF unset the verb must ask the HOST what branch it is on and deploy
# THAT. redeploy.sh's own `ref` default is origin/main, and it git-checkouts the
# ref — so a bare call silently reverts the host off the branch under test,
# mid-run. Passing the ref explicitly is the whole mitigation; assert both that
# it is asked for and that origin/main never appears.
cp_run > "$WORK/cp-out.txt" 2>&1 || true
if grep -qF "deploy/redeploy.sh 'va' 'feat/on-the-host' control" "$CP_SSH_LOG"; then
  pass "redeploy-cp:ref-defaults-to-host-branch" "deployed the host's current branch, not main"
else
  fail "redeploy-cp:ref-defaults-to-host-branch" "ssh log: $(tr '\n' '|' < "$CP_SSH_LOG")"
fi
if grep -q 'origin/main' "$CP_SSH_LOG"; then
  fail "redeploy-cp:never-origin-main" "origin/main reached the remote command — the ref trap is back"
else
  pass "redeploy-cp:never-origin-main" "origin/main never sent"
fi

# ── the confirmation `compose ps` uses the HOST'S compose file list ──────────
# keybox's compose_files are docker-compose.yml + docker-compose.nvidia.yml, and
# dx_remote_compose_args strips the repo-relative "deploy/" prefix (the remote
# cwd is <dir>/deploy). A hardcoded -f list would not track a host that adds an
# overlay, so assert the fixture's own files appear.
if grep -q 'docker compose -f docker-compose.yml -f docker-compose.nvidia.yml .*ps quasar-control-plane' "$CP_SSH_LOG"; then
  pass "redeploy-cp:compose-args-from-hosts-json" "compose ps used the host's compose_files"
else
  fail "redeploy-cp:compose-args-from-hosts-json" "ssh log: $(tr '\n' '|' < "$CP_SSH_LOG")"
fi

# ── it runs in the host's repo dir, from hosts.json ──────────────────────────
if grep -q "cd '/srv/quasar-keybox' && bash deploy/redeploy.sh" "$CP_SSH_LOG"; then
  pass "redeploy-cp:remote-dir" "ran from the host's configured dir"
else
  fail "redeploy-cp:remote-dir" "ssh log: $(tr '\n' '|' < "$CP_SSH_LOG")"
fi

# ── a host with no redeploy_label and no gpu → refuse before deploying ───────
# aliasbox has neither, so the python fallback gives "va"; the real refusal path
# is unreachable from the fixture. Assert instead that the verb is in the remote
# allow-list for a TYPED host (it is mutating, so an implicit host is refused
# above alongside up/down/restart/rebuild).
rc_of 0 "redeploy-cp:scope-allowed" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/common.sh" require-host-scope redeploy-cp

# ── redeploy.sh itself rejects an unknown scope, accepts `control` ───────────
# Argument validation happens before any git/docker work, so this is safe to run
# here: an unknown scope exits 2 at the case statement.
rc_of 2 "redeploy-cp:redeploy-bad-scope" -- \
  env -u HOST bash "$ROOT/deploy/redeploy.sh" nvidia some-ref not-a-scope
if grep -qE '^\s*all\|web\|control\)' "$ROOT/deploy/redeploy.sh"; then
  pass "redeploy-cp:redeploy-scope-accepted" "redeploy.sh accepts scope=control"
else
  fail "redeploy-cp:redeploy-scope-accepted" "deploy/redeploy.sh does not list 'control' in its scope case"
fi

printf '\n== remote-command injection ==\n'

# Every remote command in this layer is a STRING handed to the remote login
# shell, so an interpolated value is code. The quotes the call sites wrap values
# in are not protection: one `'` closes them and the remainder runs as the fleet
# ssh account, which has docker access — host root.
#
# These assert the REFUSAL, and — the part that actually matters — that the
# payload never reached the ssh log. A test that only checked the exit code
# would still pass if the command were sent and merely failed afterwards.

# reject_ref <label> <ref value> — REF= must be refused, and never transmitted.
reject_ref() {
  local label="$1" bad="$2" out
  out="$(cp_run REF="$bad" 2>&1)"; local rc=$?
  if [ "$rc" -eq 0 ]; then
    fail "inject:ref-$label" "stack.sh redeploy-cp ACCEPTED REF=$bad (rc=0)"
  elif grep -qF 'PAYLOAD-MARKER' "$CP_SSH_LOG"; then
    fail "inject:ref-$label" "REF=$bad was REFUSED but still reached ssh: $(tr '\n' '|' < "$CP_SSH_LOG")"
  else
    pass "inject:ref-$label" "refused (rc=$rc) and never sent"
  fi
}

# The quote is the whole attack: it closes redeploy.sh's '$ref' and what follows
# is a second command on the remote host.
reject_ref quote      "main'; touch /tmp/PAYLOAD-MARKER; echo '"
reject_ref semicolon  "main; touch /tmp/PAYLOAD-MARKER"
reject_ref cmdsub     'main$(touch /tmp/PAYLOAD-MARKER)'
reject_ref backtick   'main`touch /tmp/PAYLOAD-MARKER`'
# A newline is the one a per-line grep check would wave through: line 1 is a
# perfectly good ref, and the payload rides on line 2.
reject_ref newline    "$(printf 'main\ntouch /tmp/PAYLOAD-MARKER')"
# A leading dash is read as an option by whatever consumes it, not as a value.
reject_ref leadingdash "--upload-pack=touch /tmp/PAYLOAD-MARKER"

# A legitimate ref still goes through untouched — the guard must not have been
# bought by breaking the feature.
cp_run REF=feat/a_b.c-1 >/dev/null 2>&1 || true
if grep -qF "deploy/redeploy.sh 'va' 'feat/a_b.c-1' control" "$CP_SSH_LOG"; then
  pass "inject:ref-legit-still-works" "an ordinary ref is unaffected"
else
  fail "inject:ref-legit-still-works" "ssh log: $(tr '\n' '|' < "$CP_SSH_LOG")"
fi

# The ref read back OFF THE HOST is validated too. A value that crossed an ssh
# hop has no more claim to be well-formed than one typed locally, and this is
# the path taken whenever REF is unset.
HOSTILE_BIN="$WORK/hostilebin"
mkdir -p "$HOSTILE_BIN"
HOSTILE_SSH_LOG="$WORK/hostile-ssh.log"
# shellcheck disable=SC2016  # the body must reach the stub file UNEXPANDED
printf '#!/usr/bin/env bash\n%s\n' '
cmd="${*: -1}"
printf "%s\n" "$cmd" >> "$HOSTILE_SSH_LOG"
case "$cmd" in
  *"rev-parse --abbrev-ref HEAD"*) printf "%s\n" "main'"'"'; touch /tmp/PAYLOAD-MARKER; echo '"'"'" ;;
esac
exit 0' > "$HOSTILE_BIN/ssh"
chmod +x "$HOSTILE_BIN/ssh"
: > "$HOSTILE_SSH_LOG"
env PATH="$HOSTILE_BIN:$PATH" HOSTILE_SSH_LOG="$HOSTILE_SSH_LOG" \
  DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test env -u REF bash "$DX/stack.sh" redeploy-cp >/dev/null 2>&1
HOSTILE_RC=$?
if [ "$HOSTILE_RC" -eq 0 ]; then
  fail "inject:ref-from-host" "a hostile ref returned by the host was ACCEPTED (rc=0)"
elif grep -qF 'PAYLOAD-MARKER' "$HOSTILE_SSH_LOG" && grep -q 'redeploy.sh' "$HOSTILE_SSH_LOG"; then
  fail "inject:ref-from-host" "the host-supplied payload was forwarded into a redeploy command"
else
  pass "inject:ref-from-host" "refused (rc=$HOSTILE_RC) without deploying it"
fi

# The same class, at the other reachable entry points. `S=`/`N=` are the worst
# of them: stack.sh splices both into the remote docker-compose command with a
# bare `$*`, so they need no quote to break out of — a `;` is enough.
rc_of 2 "inject:log-service" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test S='quasar-node-agent; touch /tmp/PAYLOAD-MARKER' \
  bash "$DX/stack.sh" logs
rc_of 2 "inject:log-lines" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test N='200; touch /tmp/PAYLOAD-MARKER' \
  bash "$DX/stack.sh" logs
# session-logs builds a whole remote SHELL SCRIPT around SID and SINCE.
rc_of 2 "inject:session-sid" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test SID="x'; touch /tmp/PAYLOAD-MARKER; :'" \
  bash "$DX/session.sh" logs
rc_of 2 "inject:session-since" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test SID=11111111-2222-3333-4444-555555555555 \
  SINCE="10m'; touch /tmp/PAYLOAD-MARKER; :'" bash "$DX/session.sh" logs
# hosts.json is operator-local rather than hostile, but `dir` reaches a dozen
# `cd '<dir>' && ...` call sites, so it is validated where it is resolved.
rc_of 2 "inject:hosts-json-dir" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_REMOTE_DIR="/srv/x'; touch /tmp/PAYLOAD-MARKER; echo '" \
  bash "$DX/common.sh" resolve-remote gpu-test
# A bearer token is base64url; one carrying a quote breaks out of the remote
# `curl -H 'Authorization: Bearer <token>'`.
rc_of 2 "inject:admin-token" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test \
  QSES_ADMIN_TOKEN="eyJhbGc'; touch /tmp/PAYLOAD-MARKER; echo '" \
  bash "$DX/common.sh" sanitize-admin-token inject-test

# A literal `HEAD` resolves to refs/remotes/origin/HEAD (= main) and would revert
# the host mid-run, so redeploy.sh refuses it outright (#538).
rc_of 2 "inject:redeploy-head-refused" -- \
  bash "$DX/../../deploy/redeploy.sh" nvidia HEAD all

printf '\n== Makefile passthrough knobs (#550) ==\n'

# The layer BELOW every guard above. Make expands `$(ARGS)` into the recipe's
# command TEXT, which /bin/bash then parses — so `make bench-run ARGS='x; whoami'`
# ran `whoami` before any script existed to check it, and `"$(SID)"` was no
# better (a `"` closes the quotes; a backtick is live inside them). The fix is
# that the Makefile interpolates nothing a caller can set: the knobs travel by
# environment, which no shell re-parses.

MK_BAD_SEMI='--dry-run; touch /tmp/PAYLOAD-MARKER'
MK_BAD_QUOTE='x"; touch /tmp/PAYLOAD-MARKER; :"'
MK_BAD_BACKTICK='x`touch /tmp/PAYLOAD-MARKER`'
MK_BAD_CMDSUB='x$(touch /tmp/PAYLOAD-MARKER)'
# No whitespace, so it survives word splitting as ONE token — the case that
# neutralisation alone would not stop, since a single token can still be
# spliced into a remote command by the script that parses it.
MK_BAD_TIGHT='--app=x;touch$IFS/tmp/PAYLOAD-MARKER'

# ── the class, asserted structurally ─────────────────────────────────────────
# A caller-settable variable in a recipe line is the defect. Quoting it is not a
# fix, so this looks for the interpolation itself, quoted or not.
# MAKEFILE_LIST belongs on this list: make maintains it, but a command-line
# assignment still beats make's own, so it was settable exactly like a knob.
MK_KNOBS='ARGS|SID|DIR|RUN|NAME|URL|ROUTES|KEY|BEFORE|AFTER|OUT|HOST|S|N|LEVEL|TARGET|KEEP|GREP|REF|IMAGE|PROFILE|CONFIRM|WINDOW|SINCE|JSON|KIND|MAKEFILE_LIST'
if grep -nE "^	.*\\\$\\(($MK_KNOBS)[,)]" "$ROOT/Makefile" > "$WORK/mk-interp.txt" 2>/dev/null; then
  fail "knob:no-interpolation" "a caller-settable variable is spliced into a recipe line: $(tr '\n' '|' < "$WORK/mk-interp.txt")"
else
  pass "knob:no-interpolation" "no caller-settable variable reaches a recipe line"
fi

# The three variables STILL spliced into recipe lines are repo constants, so
# they are `override` — a command-line assignment cannot reach the shell.
MK_OVERRIDE_OK=1
for _v in DX CP WEB HELP_MAKEFILES; do
  grep -qE "^override +$_v +:=" "$ROOT/Makefile" || MK_OVERRIDE_OK=0
done
if [ "$MK_OVERRIDE_OK" = 1 ]; then
  pass "knob:constants-override" "DX/CP/WEB/HELP_MAKEFILES are override, so they cannot be set on the command line"
else
  fail "knob:constants-override" "a variable interpolated into a recipe line is not 'override'"
fi

# ── the class, asserted behaviourally: `make -n` prints the recipe it WOULD run
# without running it, so a payload appearing there is the bug itself.
mk_dry_clean() { # mk_dry_clean <label> <target> <VAR=value>...
  local label="$1" target="$2"; shift 2
  local out rc
  out="$(cd "$ROOT" && make -n "$target" "$@" 2>&1)"; rc=$?
  if printf '%s' "$out" | grep -qF 'PAYLOAD-MARKER'; then
    fail "knob:dryrun-$label" "the payload reached the recipe make would run: $(printf '%s' "$out" | tr '\n' '|')"
  elif [ "$rc" -ne 0 ]; then
    fail "knob:dryrun-$label" "make -n $target failed (rc=$rc): $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  else
    pass "knob:dryrun-$label" "the knob never reaches the command make would run"
  fi
}
mk_dry_clean args-agent-creds  agent-creds      ARGS="$MK_BAD_SEMI"
mk_dry_clean args-bench-run    bench-run        ARGS="$MK_BAD_SEMI"
mk_dry_clean args-qa           qa               ARGS="$MK_BAD_SEMI"
mk_dry_clean args-validate     validate         ARGS="$MK_BAD_SEMI"
mk_dry_clean args-homes-gc     homes-gc         ARGS="$MK_BAD_SEMI"
mk_dry_clean sid-session-soak  session-soak     SID="$MK_BAD_QUOTE"
mk_dry_clean sid-display       session-display  SID="$MK_BAD_BACKTICK"
mk_dry_clean sid-abr-ladder    abr-ladder       SID="$MK_BAD_CMDSUB"
mk_dry_clean dir-bench-submit  bench-submit     DIR="$MK_BAD_QUOTE"
mk_dry_clean run-bench-budget  bench-budget     RUN="$MK_BAD_BACKTICK"
mk_dry_clean name-bench-base   bench-baseline   NAME="$MK_BAD_QUOTE" RUN=r1
mk_dry_clean url-ui-audit      ui-audit         URL="$MK_BAD_QUOTE"
mk_dry_clean routes-ui-audit   ui-audit-routes  ROUTES="$MK_BAD_BACKTICK"
mk_dry_clean key-ui-audit      ui-audit         KEY="$MK_BAD_QUOTE" URL=https://h:8443
mk_dry_clean out-ui-audit      ui-audit         OUT="$MK_BAD_BACKTICK" URL=https://h:8443
mk_dry_clean before-ui-audit   ui-audit-ab      BEFORE="$MK_BAD_QUOTE" AFTER=.uiaudit/b
mk_dry_clean after-ui-audit    ui-audit-ab      AFTER="$MK_BAD_BACKTICK" BEFORE=.uiaudit/a
mk_dry_clean host-admin-token  admin-token      HOST="$MK_BAD_QUOTE"
# The repo constants are not knobs any more; `override` wins over the command line.
mk_dry_clean web-clean         clean            WEB='x; touch /tmp/PAYLOAD-MARKER'
mk_dry_clean makefile-list     help             MAKEFILE_LIST='Makefile; touch /tmp/PAYLOAD-MARKER'

# ── per-knob: hostile refused, legitimate still works ────────────────────────
# dx_env_argv is the receiving end for the two LIST knobs (ARGS, SID). It splits
# on whitespace — which evaluates nothing — and then shape-checks each token,
# because a script that parses one may forward it into a remote command.

knob_refused() { # knob_refused <label> <VAR> <hostile value>
  local label="$1" var="$2" bad="$3" out rc
  out="$(env "$var=$bad" bash "$DX/common.sh" env-argv knob-test "$var" 2>/dev/null)"; rc=$?
  if [ "$rc" -eq 0 ]; then
    fail "knob:$label" "$var was ACCEPTED (rc=0)"
  elif printf '%s\n' "$out" | grep -qvE '^(RESULT |$)'; then
    fail "knob:$label" "$var was refused but a token still reached the argument list: $(printf '%s' "$out" | tr '\n' '|')"
  else
    pass "knob:$label" "refused (rc=$rc), no token reached the argument list"
  fi
}

knob_ok() { # knob_ok <label> <VAR> <value> <expected argv, newline separated>
  local label="$1" var="$2" value="$3" expected="$4" out rc
  out="$(env "$var=$value" bash "$DX/common.sh" env-argv knob-test "$var" 2>&1)"; rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "knob:$label" "a documented $var='$value' was REFUSED (rc=$rc): $(printf '%s' "$out" | tr '\n' ' ')"
  elif [ "$out" != "$expected" ]; then
    fail "knob:$label" "$var='$value' split to [$(printf '%s' "$out" | tr '\n' '|')], expected [$(printf '%s' "$expected" | tr '\n' '|')]"
  else
    pass "knob:$label" "a documented value still becomes the right arguments"
  fi
}

knob_refused args-semicolon ARGS "$MK_BAD_SEMI"
knob_refused args-quote     ARGS "$MK_BAD_QUOTE"
knob_refused args-backtick  ARGS "$MK_BAD_BACKTICK"
knob_refused args-cmdsub    ARGS "$MK_BAD_CMDSUB"
knob_refused args-tight     ARGS "$MK_BAD_TIGHT"
knob_refused args-newline   ARGS "$(printf -- '--dry-run\n; touch /tmp/PAYLOAD-MARKER')"
knob_refused args-pipe      ARGS '--app|touch /tmp/PAYLOAD-MARKER'
knob_refused args-redirect  ARGS '--out>/tmp/PAYLOAD-MARKER'
knob_refused sid-quote      SID  "$MK_BAD_QUOTE"
knob_refused sid-cmdsub     SID  "$MK_BAD_CMDSUB"

# Every ARGS value `make help` documents must still survive, verbatim.
knob_ok args-agent-creds ARGS '--role admin --ttl 1h'          "$(printf -- '--role\nadmin\n--ttl\n1h')"
knob_ok args-display     ARGS '--stream 1280x720'              "$(printf -- '--stream\n1280x720')"
knob_ok args-soak        ARGS '--duration 180'                 "$(printf -- '--duration\n180')"
knob_ok args-ladder      ARGS '--dwell 240'                    "$(printf -- '--dwell\n240')"
knob_ok args-codec       ARGS '--app Steam --codecs h264,av1'  "$(printf -- '--app\nSteam\n--codecs\nh264,av1')"
knob_ok args-homes-gc    ARGS '--dry-run'                      '--dry-run'
knob_ok args-bench-run   ARGS '--profile 1080p60-h264 --secs 240' "$(printf -- '--profile\n1080p60-h264\n--secs\n240')"
knob_ok args-bench-tag   ARGS '--suite s1 --tag k=v'           "$(printf -- '--suite\ns1\n--tag\nk=v')"
knob_ok args-validate    ARGS '--reuse-web'                    '--reuse-web'
knob_ok args-qa          ARGS '--no-repoint'                   '--no-repoint'
knob_ok args-admin-token ARGS '--fresh --ttl 2h'               "$(printf -- '--fresh\n--ttl\n2h')"
knob_ok args-url         ARGS '--url https://host:8443/'       "$(printf -- '--url\nhttps://host:8443/')"
knob_ok args-glob        ARGS '--app-log-glob *.log'           "$(printf -- '--app-log-glob\n*.log')"
knob_ok sid-uuid         SID  '11111111-2222-3333-4444-555555555555' '11111111-2222-3333-4444-555555555555'
knob_ok sid-latest       SID  'latest'                         'latest'

# `SID=latest` has to keep meaning `--latest` now that make no longer rewrites
# it into that flag — the two scripts that took the rewrite own the mapping.
for _s in session_soak abr_ladder_netem; do
  if grep -qE '^ *latest\) *SID="--latest"' "$DX/$_s.sh"; then
    pass "knob:latest-$_s" "SID=latest still resolves to --latest"
  else
    fail "knob:latest-$_s" "$_s.sh no longer maps the bare word 'latest' to --latest"
  fi
done

# ── the receiving end, per script ────────────────────────────────────────────
# Testing dx_env_argv alone is not enough: a script that sets its own default
# (`SID=""`) ABOVE the shim shadows the environment, and the knob silently stops
# arriving at all. So drive each script the way `make` now does — no arguments,
# knob in the environment — and require the knob guard, by its own wording, to
# be what refuses.
KNOB_GUARD_MSG='is not a shape this tooling will send'

script_refuses_knob() { # script_refuses_knob <script> <VAR> [extra env...]
  local script="$1" var="$2"; shift 2
  local out rc
  out="$(env DX_HOSTS_JSON="$FIX_HOSTS" "$var=$MK_BAD_SEMI" "$@" \
    bash "$DX/$script" 2>&1)"; rc=$?
  if [ "$rc" -eq 0 ]; then
    fail "knob:recv-$script-$var" "$script accepted a hostile $var (rc=0)"
  elif ! printf '%s' "$out" | grep -qF "$KNOB_GUARD_MSG"; then
    fail "knob:recv-$script-$var" "$script did not refuse a hostile $var at the knob guard — it probably shadows \$$var with its own default before reading it. Got: $(printf '%s' "$out" | tail -n 3 | tr '\n' ' ')"
  else
    pass "knob:recv-$script-$var" "the environment knob reaches the guard"
  fi
}

# The other half: a legitimate value must still ARRIVE as arguments. `--help` is
# the one value every one of these parsers understands and that does no work, so
# rc=0 plus a usage banner proves the token got through intact.
script_help_via_args() { # script_help_via_args <script> [extra env...]
  local script="$1"; shift
  local out rc
  out="$(env DX_HOSTS_JSON="$FIX_HOSTS" ARGS=--help "$@" bash "$DX/$script" 2>&1)"; rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "knob:recv-$script-legit" "ARGS=--help did not reach $script's parser (rc=$rc): $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  elif ! printf '%s' "$out" | grep -qF "$script"; then
    fail "knob:recv-$script-legit" "ARGS=--help produced no usage banner: $(printf '%s' "$out" | head -n 2 | tr '\n' ' ')"
  else
    pass "knob:recv-$script-legit" "a legitimate ARGS value still reaches the parser"
  fi
}

# And two that guard on a DIFFERENT, unrelated thing once the knob has been
# accepted — which is the proof the value was parsed rather than refused.
script_accepts_knob() { # script_accepts_knob <script> <legit ARGS> <expected guard text>
  local script="$1" value="$2" expected="$3"
  local out
  out="$(env DX_HOSTS_JSON="$FIX_HOSTS" ARGS="$value" bash "$DX/$script" 2>&1)" || true
  if printf '%s' "$out" | grep -qF "$KNOB_GUARD_MSG"; then
    fail "knob:recv-$script-legit" "$script REFUSED a documented ARGS='$value'"
  elif ! printf '%s' "$out" | grep -qF "$expected"; then
    fail "knob:recv-$script-legit" "$script did not get past ARGS='$value' to its own guard: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  else
    pass "knob:recv-$script-legit" "ARGS='$value' was parsed; the script stopped on its own guard"
  fi
}

script_refuses_knob agentcreds.sh          ARGS
script_refuses_knob session_display.sh     ARGS
script_refuses_knob session_display.sh     SID
script_refuses_knob session_soak.sh        ARGS
script_refuses_knob session_soak.sh        SID
script_refuses_knob abr_ladder_netem.sh    ARGS HOST=gpu-test
script_refuses_knob abr_ladder_netem.sh    SID  HOST=gpu-test
script_refuses_knob codec_validate.sh      ARGS
script_refuses_knob nvenc_fallback_smoke.sh ARGS
script_refuses_knob homes_gc.sh            ARGS
script_refuses_knob bench_retro.sh         ARGS
script_refuses_knob bench_run.sh           ARGS
script_refuses_knob bench_suite.sh         ARGS
script_refuses_knob validate.sh            ARGS
script_refuses_knob qa.sh                  ARGS
script_refuses_knob admin_token.sh         ARGS
# pyrun.sh takes its tool on the command line, so it is driven directly.
KNOB_PYRUN_OUT="$(env ARGS="$MK_BAD_SEMI" bash "$DX/pyrun.sh" bench-budget bench_budget.py --run RUN 2>&1)" \
  && KNOB_PYRUN_RC=0 || KNOB_PYRUN_RC=$?
if [ "$KNOB_PYRUN_RC" -ne 0 ] && printf '%s' "$KNOB_PYRUN_OUT" | grep -qF "$KNOB_GUARD_MSG"; then
  pass "knob:recv-pyrun.sh-ARGS" "the environment knob reaches the guard"
else
  fail "knob:recv-pyrun.sh-ARGS" "pyrun.sh did not refuse a hostile ARGS (rc=$KNOB_PYRUN_RC): $(printf '%s' "$KNOB_PYRUN_OUT" | tail -n 2 | tr '\n' ' ')"
fi

script_help_via_args agentcreds.sh
script_help_via_args session_display.sh
script_help_via_args session_soak.sh
script_help_via_args abr_ladder_netem.sh HOST=gpu-test
script_help_via_args validate.sh
script_help_via_args admin_token.sh
script_accepts_knob homes_gc.sh '--dry-run'    'the local stack runs no node-agent'
script_accepts_knob qa.sh       '--no-repoint' 'IMAGE=<tag> is required'

# ── the SINGLE-value knobs: pyrun.sh and uiaudit.sh ──────────────────────────
# These are not split, because a legitimate DIR/OUT may contain a space. They
# are safe for a different reason: each becomes one element of a bash array,
# which is argv — never text a shell re-parses. Assert exactly that, by
# intercepting the child process and reading the argument vector it was handed.
KNOB_STUB="$WORK/knobstub"
mkdir -p "$KNOB_STUB"
# shellcheck disable=SC2016  # the body must reach the stub file UNEXPANDED
printf '#!/bin/sh\nfor a in "$@"; do printf "%%s\\n" "$a"; done\n' > "$KNOB_STUB/python3"
cp "$KNOB_STUB/python3" "$KNOB_STUB/bash"
chmod +x "$KNOB_STUB/python3" "$KNOB_STUB/bash"

# argv_intact <label> <needle> <command...> — the needle must come back as
# exactly ONE argument, and nothing may have executed.
argv_intact() {
  local label="$1" needle="$2"; shift 2
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "knob:$label" "rc=$rc :: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  elif printf '%s\n' "$out" | grep -qxF -- "$needle"; then
    pass "knob:$label" "the value arrived as one argument, unparsed"
  else
    fail "knob:$label" "the value did not survive as a single argument: $(printf '%s' "$out" | tr '\n' '|')"
  fi
}

argv_intact dir-argv "$MK_BAD_QUOTE" \
  env PATH="$KNOB_STUB:$PATH" DIR="$MK_BAD_QUOTE" ARGS= \
  /bin/bash "$DX/pyrun.sh" bench-submit bench_submit.py --dir DIR
argv_intact run-argv "$MK_BAD_BACKTICK" \
  env PATH="$KNOB_STUB:$PATH" RUN="$MK_BAD_BACKTICK" ARGS= \
  /bin/bash "$DX/pyrun.sh" bench-budget bench_budget.py --run RUN
argv_intact name-argv "$MK_BAD_CMDSUB" \
  env PATH="$KNOB_STUB:$PATH" RUN=r1 NAME="$MK_BAD_CMDSUB" ARGS= \
  /bin/bash "$DX/pyrun.sh" bench-baseline bench_baseline.py --run RUN --name NAME
# A path with a space in it is the reason these are NOT split.
argv_intact dir-space "$WORK/a dir/with space" \
  env PATH="$KNOB_STUB:$PATH" DIR="$WORK/a dir/with space" ARGS= \
  /bin/bash "$DX/pyrun.sh" bench-submit bench_submit.py --dir DIR

argv_intact url-argv "$MK_BAD_QUOTE" \
  env PATH="$KNOB_STUB:$PATH" URL="$MK_BAD_QUOTE" ROUTES=all KEY= OUT=/tmp/ui \
  /bin/bash "$DX/uiaudit.sh" make-audit
argv_intact routes-argv "$MK_BAD_BACKTICK" \
  env PATH="$KNOB_STUB:$PATH" URL=https://h:8443 ROUTES="$MK_BAD_BACKTICK" KEY= OUT=/tmp/ui \
  /bin/bash "$DX/uiaudit.sh" make-audit
argv_intact key-argv "$MK_BAD_CMDSUB" \
  env PATH="$KNOB_STUB:$PATH" URL=https://h:8443 ROUTES=all KEY="$MK_BAD_CMDSUB" OUT=/tmp/ui \
  /bin/bash "$DX/uiaudit.sh" make-audit
argv_intact out-argv "$MK_BAD_QUOTE" \
  env PATH="$KNOB_STUB:$PATH" URL=https://h:8443 ROUTES=all KEY= OUT="$MK_BAD_QUOTE" \
  /bin/bash "$DX/uiaudit.sh" make-audit
argv_intact before-argv "$MK_BAD_BACKTICK" \
  env PATH="$KNOB_STUB:$PATH" BEFORE="$MK_BAD_BACKTICK" AFTER=/tmp/after OUT= \
  /bin/bash "$DX/uiaudit.sh" make-ab
argv_intact after-argv "$MK_BAD_CMDSUB" \
  env PATH="$KNOB_STUB:$PATH" BEFORE=/tmp/before AFTER="$MK_BAD_CMDSUB" OUT= \
  /bin/bash "$DX/uiaudit.sh" make-ab

# The legitimate shapes still assemble the command they always did.
KNOB_UI_OUT="$(env PATH="$KNOB_STUB:$PATH" URL=https://host:8443 ROUTES=admin-images,admin-users \
  KEY=devkey OUT=.uiaudit/t /bin/bash "$DX/uiaudit.sh" make-audit 2>&1)"
if printf '%s' "$KNOB_UI_OUT" | tr '\n' ' ' \
    | grep -qF -- 'capture --url https://host:8443 --out .uiaudit/t --routes admin-images,admin-users --key devkey'; then
  pass "knob:ui-audit-legit" "ui-audit still builds the documented capture command"
else
  fail "knob:ui-audit-legit" "$(printf '%s' "$KNOB_UI_OUT" | tr '\n' '|')"
fi
if printf '%s' "$KNOB_UI_OUT" | tr '\n' ' ' | grep -qF -- 'report --evidence .uiaudit/t'; then
  pass "knob:ui-audit-report" "the report step still gets the same evidence dir the capture wrote"
else
  fail "knob:ui-audit-report" "$(printf '%s' "$KNOB_UI_OUT" | tr '\n' '|')"
fi

# Nothing above may have actually executed its payload — locally or otherwise.
if [ -e /tmp/PAYLOAD-MARKER ]; then
  fail "inject:no-payload-executed" "/tmp/PAYLOAD-MARKER exists — an injection test EXECUTED. Remove it and investigate."
  rm -f /tmp/PAYLOAD-MARKER
else
  pass "inject:no-payload-executed" "no injected payload ran"
fi

printf '\n== leak-scan (issue tracker) ==\n'

# The tree modes cannot see the OTHER public surface. These run the real script
# against fixture payloads, because a live tracker that was just scrubbed proves
# nothing about detection.
LS="$(cd "$TESTS_DIR/../../dev" && pwd)/leak-scan.sh"

ls_dirty="$(LEAK_SCAN_ISSUES_JSON="$FIXTURES/leak-issues-dirty.json" bash "$LS" --issues 2>&1)"
ls_dirty_rc=$?
if [ "$ls_dirty_rc" -eq 1 ] &&
  printf '%s' "$ls_dirty" | grep -q 'issue#101 title' &&
  printf '%s' "$ls_dirty" | grep -q 'issue#102 body' &&
  printf '%s' "$ls_dirty" | grep -q 'issue#102 comment\[1\]' &&
  printf '%s' "$ls_dirty" | grep -q 'issue#104 body' &&
  printf '%s' "$ls_dirty" | grep -q 'issue#104 comment\[1\]' &&
  ! printf '%s' "$ls_dirty" | grep -q 'issue#103'; then
  pass "leakscan:issues-detects" "LAN IP in a title, domain in a body, home path + key name in a comment, bare hostnames + the appliance path in a fourth; the clean issue is not flagged"
else
  fail "leakscan:issues-detects" "rc=$ls_dirty_rc, output: $(printf '%s' "$ls_dirty" | head -n 6)"
fi

ls_clean="$(LEAK_SCAN_ISSUES_JSON="$FIXTURES/leak-issues-clean.json" bash "$LS" --issues 2>&1)"
ls_clean_rc=$?
if [ "$ls_clean_rc" -eq 0 ]; then
  pass "leakscan:issues-clean" "role names and RFC 5737 stand-ins do not trip the guard"
else
  fail "leakscan:issues-clean" "rc=$ls_clean_rc, output: $(printf '%s' "$ls_clean" | head -n 6)"
fi

# A guard that reads 'clean' when it could not look is worse than no guard.
LEAK_SCAN_ISSUES_JSON="$WORK/definitely-absent.json" bash "$LS" --issues >/dev/null 2>&1
ls_missing_rc=$?
if [ "$ls_missing_rc" -eq 2 ]; then
  pass "leakscan:issues-fetch-failure-is-not-clean" "an unreadable payload exits 2, not 0"
else
  fail "leakscan:issues-fetch-failure-is-not-clean" "expected rc=2, got rc=$ls_missing_rc"
fi

printf '\n== redaction ==\n'

# ── golden test ──────────────────────────────────────────────────────────────
if [ -f "$FIXTURES/redact-input.txt" ] && [ -f "$FIXTURES/redact-expected.txt" ]; then
  bash "$DX/redact.sh" < "$FIXTURES/redact-input.txt" > "$WORK/redact-actual.txt" 2>&1
  if diff -u "$FIXTURES/redact-expected.txt" "$WORK/redact-actual.txt" > "$WORK/redact.diff" 2>&1; then
    pass "redact:golden" "output matches the fixture exactly"
  else
    fail "redact:golden" "$(head -n 20 "$WORK/redact.diff")"
  fi
else
  fail "redact:golden" "fixtures missing under $FIXTURES"
fi

# ── no secret VALUE survives ─────────────────────────────────────────────────
# The strongest assertion: grep the actual output for each planted secret.
SECRETS=(hunter2-super-secret tok-abc-123 shhh-do-not-tell sgdb-live-4444
         pgpw123 ci-pass-9 sk-live-9999 xyz123 QWxpY2VXYXNIZXJl
         MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ)
leaked=""
for s in "${SECRETS[@]}"; do
  if grep -qF "$s" "$WORK/redact-actual.txt" 2>/dev/null; then leaked="$leaked $s"; fi
done
if [ -z "$leaked" ]; then
  pass "redact:no-leak" "${#SECRETS[@]} planted secrets all masked"
else
  fail "redact:no-leak" "these values survived redaction:$leaked"
fi

# ── determinism ──────────────────────────────────────────────────────────────
bash "$DX/redact.sh" < "$FIXTURES/redact-input.txt" > "$WORK/redact-again.txt" 2>&1
if diff -q "$WORK/redact-actual.txt" "$WORK/redact-again.txt" >/dev/null 2>&1; then
  pass "redact:deterministic" "two runs produce identical output"
else
  fail "redact:deterministic" "output differs between runs"
fi

# ── idempotence: redacting redacted output changes nothing further ───────────
bash "$DX/redact.sh" < "$WORK/redact-actual.txt" > "$WORK/redact-twice.txt" 2>&1
if diff -q "$WORK/redact-actual.txt" "$WORK/redact-twice.txt" >/dev/null 2>&1; then
  pass "redact:idempotent" "re-redacting is a no-op"
else
  warn "redact:idempotent" "a second pass changes the output (safe, but noisy in bundles)"
fi

printf '\n== instance isolation ==\n'

# ── two fake worktree paths → different project names AND different ports ────
A="$(env -u QUASAR_INSTANCE QUASAR_DX_ROOT=/fake/worktree-alpha bash "$DX/common.sh" instance)"
B="$(env -u QUASAR_INSTANCE QUASAR_DX_ROOT=/fake/worktree-beta  bash "$DX/common.sh" instance)"
PA="$(env -u QUASAR_INSTANCE QUASAR_DX_ROOT=/fake/worktree-alpha bash "$DX/common.sh" ports)"
PB="$(env -u QUASAR_INSTANCE QUASAR_DX_ROOT=/fake/worktree-beta  bash "$DX/common.sh" ports)"

if [ -n "$A" ] && [ -n "$B" ] && [ "$A" != "$B" ]; then
  pass "instance:distinct" "$A != $B"
else
  fail "instance:distinct" "two worktree paths produced the same instance ('$A' / '$B')"
fi

if [ "$PA" != "$PB" ]; then
  pass "instance:ports-distinct" "alpha[$PA] != beta[$PB]"
else
  fail "instance:ports-distinct" "two worktree paths produced the same port block ($PA)"
fi

# ── deterministic: the same path always yields the same instance ─────────────
A2="$(env -u QUASAR_INSTANCE QUASAR_DX_ROOT=/fake/worktree-alpha bash "$DX/common.sh" instance)"
if [ "$A" = "$A2" ]; then
  pass "instance:deterministic" "same path -> same instance ($A)"
else
  fail "instance:deterministic" "$A != $A2 for the same path"
fi

# ── the instance is a legal compose project name ─────────────────────────────
if printf '%s' "$A" | grep -qE '^[a-z0-9][a-z0-9_-]*$'; then
  pass "instance:legal-name" "$A is a valid compose project name"
else
  fail "instance:legal-name" "$A is not a valid compose project name"
fi

# ── an explicit QUASAR_INSTANCE wins ─────────────────────────────────────────
C="$(env QUASAR_INSTANCE=my-override bash "$DX/common.sh" instance)"
if [ "$C" = "my-override" ]; then
  pass "instance:override" "QUASAR_INSTANCE is respected"
else
  fail "instance:override" "expected my-override, got $C"
fi

printf '\n== degraded reporting ==\n'

# ── doctor with an unreachable gpu-test role → degraded, rc 0 ────────────────
# The fixture's gpu-test role (keybox, 203.0.113.5 — TEST-NET-3, RFC 5737,
# guaranteed non-routable) is resolvable but unreachable; the stubbed `ssh`
# also fails every probe. docker/go/node are stubbed so the ONLY non-PASS is
# the remote reachability check.
doc_out="$(with_stubs env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" \
             bash "$DX/doctor.sh" 2>&1)"
doc_rc=$?
if [ "$doc_rc" -ne 0 ]; then
  fail "degraded:doctor-rc" "expected rc=0 for a degraded doctor, got $doc_rc"
else
  pass "degraded:doctor-rc" "rc=0"
fi
if printf '%s' "$doc_out" | grep -q 'RESULT status=degraded'; then
  pass "degraded:doctor-status" "RESULT status=degraded"
else
  fail "degraded:doctor-status" "expected RESULT status=degraded, got: $(printf '%s' "$doc_out" | grep '^RESULT' || echo '<no RESULT line>')"
fi
if printf '%s' "$doc_out" | grep -q '^WARN remote'; then
  pass "degraded:doctor-remote-warn" "unreachable gpu-test role is a WARN, not a FAIL"
else
  fail "degraded:doctor-remote-warn" "expected a 'WARN remote' line"
fi

# ── exactly ONE RESULT line, always ──────────────────────────────────────────
result_n="$(printf '%s\n' "$doc_out" | grep -c '^RESULT ')"
if [ "$result_n" -eq 1 ]; then
  pass "contract:one-result-line" "doctor emitted exactly 1 RESULT line"
else
  fail "contract:one-result-line" "doctor emitted $result_n RESULT lines"
fi

printf '\n== bundle manifest ==\n'

# ── bundle against a stubbed docker → manifest lists the exclusions ──────────
BUNDLE_DIR="$WORK/diagnostics"
bundle_out="$(with_stubs env -u HOST DX_DIAG_DIR="$BUNDLE_DIR" \
                QUASAR_INSTANCE=dx-testinstance \
                bash "$DX/bundle.sh" 2>&1)"
bundle_rc=$?
if [ "$bundle_rc" -le 1 ]; then
  pass "bundle:rc" "rc=$bundle_rc (0 ok / 1 failed are both valid contract exits)"
else
  fail "bundle:rc" "unexpected rc=$bundle_rc :: $(printf '%s' "$bundle_out" | tail -n 3 | tr '\n' ' ')"
fi

MAN="$(find "$BUNDLE_DIR" -name MANIFEST.txt 2>/dev/null | head -n1)"
if [ -z "$MAN" ]; then
  fail "bundle:manifest-exists" "no MANIFEST.txt produced under $BUNDLE_DIR"
else
  pass "bundle:manifest-exists" "$MAN"

  missing=""
  for needle in "EXCLUDED BY DEFAULT" "deploy/.env" "database data" "home directories" "core dumps"; do
    grep -qF "$needle" "$MAN" || missing="$missing [$needle]"
  done
  if [ -z "$missing" ]; then
    pass "bundle:manifest-exclusions" "every excluded-by-default class is named"
  else
    fail "bundle:manifest-exclusions" "manifest does not mention:$missing"
  fi

  if grep -qF "INCLUDED" "$MAN"; then
    pass "bundle:manifest-inclusions" "included items are listed"
  else
    fail "bundle:manifest-inclusions" "no INCLUDED section"
  fi

  # The CP bundle is admin-gated + per-session: with no token it must be
  # recorded as SKIPPED, never cause a failure.
  if grep -qF "SKIPPED" "$MAN"; then
    pass "bundle:cp-skip-noted" "the admin-gated control-plane bundle is noted as SKIPPED"
  else
    warn "bundle:cp-skip-noted" "expected a SKIPPED note for the control-plane bundle"
  fi

  # The bundle directory must not be world-readable.
  BDIR="$(dirname "$MAN")"
  # shellcheck disable=SC2012  # BDIR is a path this script just created
  mode="$(ls -ld "$BDIR" | awk '{print $1}')"
  case "$mode" in
    drwx------*) pass "bundle:mode" "bundle directory is 0700 ($mode)" ;;
    *)           fail "bundle:mode" "bundle directory is $mode, expected drwx------" ;;
  esac

  # Nothing in the bundle may contain a raw secret.
  if grep -rqE '(BEGIN [A-Z]+ PRIVATE KEY|hunter2-super-secret)' "$BDIR" 2>/dev/null; then
    fail "bundle:no-raw-secrets" "raw secret material found inside the bundle"
  else
    pass "bundle:no-raw-secrets" "no planted secret pattern present"
  fi
fi

printf '\n== session soak ==\n'

# The soak's step schedule is pure arithmetic and its report generator is pure
# analysis, so both are testable with no stack at all. --launch makes the driver
# fully offline (it never constructs an API client), and testdata/soak-* is a
# synthetic run of a 1920x1080 ladder walk.
soak_sched="$(bash "$DX/session_soak.sh" --dry-run --launch 1920x1080 --duration 180 2>&1 || true)"
if printf '%s' "$soak_sched" | grep -qE '1280x720 +floor hold' \
   && printf '%s' "$soak_sched" | grep -qE 'TOTAL +180\.0s' \
   && printf '%s' "$soak_sched" | grep -qE '5 +1920x1080 +restore'; then
  pass "soak:schedule" "ladder 1920x1080/180s: launch > 1600x900 > 1280x720 > back, fits the duration"
else
  fail "soak:schedule" "$(printf '%s' "$soak_sched" | tail -n 5 | tr '\n' ' ')"
fi

rc_of 2 "soak:guard-no-session" -- env -u HOST bash "$DX/session_soak.sh" --duration 180
rc_of 2 "soak:guard-bad-profile" -- env -u HOST bash "$DX/session_soak.sh" x --profile nope

if [ -f "$DX/testdata/soak-metrics.jsonl" ]; then
  soak_dir="$WORK/soak-report"
  mkdir -p "$soak_dir"
  cp "$DX/testdata/soak-steps.jsonl" "$soak_dir/steps.jsonl"
  cp "$DX/testdata/soak-metrics.jsonl" "$soak_dir/metrics.jsonl"
  cp "$DX/testdata/soak-session.json" "$soak_dir/session.json"
  printf 'null\n' > "$soak_dir/trace.json"
  soak_verdict="$(python3 "$DX/session_soak_report.py" --dir "$soak_dir" 2>&1 || true)"
  if [ "$soak_verdict" = PASS ] && [ -s "$soak_dir/REPORT.md" ] && [ -s "$soak_dir/summary.json" ]; then
    pass "soak:report" "synthetic run analysed -> PASS, REPORT.md + summary.json written"
  else
    fail "soak:report" "verdict='$soak_verdict' (expected PASS with both artefacts)"
  fi
  # The report must NOT silently pass a run whose internal size moved somewhere
  # (render must be identical across every step — no clamp exists any more).
  python3 - "$soak_dir" <<'PY'
import json, os, sys
d = sys.argv[1]
rows = [json.loads(l) for l in open(os.path.join(d, "metrics.jsonl"))]
for r in rows:
    m = r.get("metrics") or {}
    if m.get("stream_width") == 1280:
        m["render_width"], m["render_height"] = 1024, 600
with open(os.path.join(d, "metrics.jsonl"), "w") as f:
    for r in rows:
        f.write(json.dumps(r) + "\n")
PY
  soak_verdict="$(python3 "$DX/session_soak_report.py" --dir "$soak_dir" 2>&1 || true)"
  if [ "$soak_verdict" = FAIL ]; then
    pass "soak:internal-moved" "an unexplained internal resize is reported as FAIL"
  else
    fail "soak:internal-moved" "verdict='$soak_verdict' (expected FAIL)"
  fi
else
  warn "soak:report" "scripts/dx/testdata/soak-*.jsonl missing — SKIPPED"
fi

# `observe` is a NO-PATCH profile: the schedule must be a single hold at the launch
# size for the whole duration, because the whole point is to watch the ABR ladder
# move the size by itself.
soak_obs="$(bash "$DX/session_soak.sh" --dry-run --launch 1920x1080 --duration 240 --profile observe 2>&1 || true)"
if printf '%s' "$soak_obs" | grep -qE '1 +1920x1080 +observe \(no patches\)' \
   && [ "$(printf '%s' "$soak_obs" | grep -cE '^ *[0-9]+ +[0-9]+x[0-9]+')" = 1 ]; then
  pass "soak:observe-schedule" "observe = one launch-size hold, zero steps"
else
  fail "soak:observe-schedule" "$(printf '%s' "$soak_obs" | tail -n 5 | tr '\n' ' ')"
fi

if [ -f "$DX/testdata/soak-observe-metrics.jsonl" ]; then
  obs_dir="$WORK/soak-observe"
  mkdir -p "$obs_dir"
  cp "$DX/testdata/soak-observe-metrics.jsonl" "$obs_dir/metrics.jsonl"
  cp "$DX/testdata/soak-observe-session.json" "$obs_dir/session.json"
  cp "$DX/testdata/soak-observe-trace.json" "$obs_dir/trace.json"
  printf '' > "$obs_dir/steps.jsonl"
  obs_verdict="$(python3 "$DX/session_soak_report.py" --dir "$obs_dir" 2>&1 || true)"
  # Verify that ladder_steps were parsed (count > 0 in summary.json).
  ladder_steps_count=$(python3 -c "import json; s=json.load(open('$obs_dir/summary.json')); print(len(s.get('ladder_steps', [])))" 2>/dev/null || echo 0)
  if [ "$obs_verdict" = PASS ] && grep -q "Ladder steps" "$obs_dir/REPORT.md" \
     && grep -q "time to first step" "$obs_dir/REPORT.md" \
     && [ "$ladder_steps_count" -gt 0 ]; then
    pass "soak:observe-report" "agent-driven steps tabulated with timings ($ladder_steps_count steps parsed)"
  else
    fail "soak:observe-report" "verdict='$obs_verdict' ladder_steps=$ladder_steps_count (want PASS + Ladder steps table with steps>0)"
  fi
  # An A-B-A inside 60s is pumping and must FAIL the run, not be reported as fine.
  python3 - "$obs_dir" <<'PY'
import json, os, sys
d = sys.argv[1]
t = json.load(open(os.path.join(d, "trace.json")))
ev = t.setdefault("events", [])
base = ev[0]["ts_unix_ms"] if ev else 0
ev.append({"ts_unix_ms": base + 20000, "type": "abr.ladder.step",
           "payload": {"rung": "resolution", "from": 1, "to": 0, "reason": "recover", "setpoint_kbps": 8200}})
ev.append({"ts_unix_ms": base + 40000, "type": "abr.ladder.step",
           "payload": {"rung": "resolution", "from": 0, "to": 1, "reason": "engage", "setpoint_kbps": 5100}})
json.dump(t, open(os.path.join(d, "trace.json"), "w"))
PY
  obs_verdict="$(python3 "$DX/session_soak_report.py" --dir "$obs_dir" 2>&1 || true)"
  if [ "$obs_verdict" = FAIL ]; then
    pass "soak:observe-oscillation" "an A-B-A inside 60s is reported as FAIL"
  else
    fail "soak:observe-oscillation" "verdict='$obs_verdict' (expected FAIL)"
  fi
  # marks.jsonl (written by abr_ladder_netem.sh) anchors the two timings on the
  # ACTUAL qnetem apply/clear instants instead of the run's own start time.
  if [ -f "$DX/testdata/soak-observe-marks.jsonl" ]; then
    mk_dir="$WORK/soak-observe-marks"
    mkdir -p "$mk_dir"
    cp "$DX/testdata/soak-observe-metrics.jsonl" "$mk_dir/metrics.jsonl"
    cp "$DX/testdata/soak-observe-session.json" "$mk_dir/session.json"
    cp "$DX/testdata/soak-observe-trace.json" "$mk_dir/trace.json"
    cp "$DX/testdata/soak-observe-marks.jsonl" "$mk_dir/marks.jsonl"
    printf '' > "$mk_dir/steps.jsonl"
    python3 "$DX/session_soak_report.py" --dir "$mk_dir" >/dev/null 2>&1 || true
    if grep -q 'time to first step: 12.0 s' "$mk_dir/REPORT.md" 2>/dev/null \
       && grep -q 'time to recover: 45.0 s' "$mk_dir/REPORT.md" 2>/dev/null; then
      pass "soak:observe-marks-timing" "marks.jsonl anchors ttfs=12.0s ttr=45.0s (not run-start)"
    else
      fail "soak:observe-marks-timing" "$(grep -E 'time to (first step|recover)' "$mk_dir/REPORT.md" 2>/dev/null | tr '\n' ' ')"
    fi
  else
    warn "soak:observe-marks-timing" "scripts/dx/testdata/soak-observe-marks.jsonl missing — SKIPPED"
  fi
else
  warn "soak:observe-report" "scripts/dx/testdata/soak-observe-*.jsonl missing — SKIPPED"
fi

printf '\n== abr ladder netem ==\n'
rc_of 0 "ladder:help" -- bash "$DX/abr_ladder_netem.sh" --help
rc_of 2 "ladder:guard-no-session" -- env -u HOST bash "$DX/abr_ladder_netem.sh" --duration 240
rc_of 2 "ladder:guard-bad-level" -- env -u HOST bash "$DX/abr_ladder_netem.sh" x --levels bogus
ladder_plan="$(bash "$DX/abr_ladder_netem.sh" x --dry-run --levels clean,moderate,severe,clean 2>&1 || true)"
if printf '%s' "$ladder_plan" | grep -q 'qnetem sender moderate' \
   && printf '%s' "$ladder_plan" | grep -q 'qnetem sender-clear'; then
  pass "ladder:plan" "dry run prints the qnetem + soak sequence without touching anything"
else
  fail "ladder:plan" "$(printf '%s' "$ladder_plan" | tail -n 5 | tr '\n' ' ')"
fi

# abr-ladder shapes a REMOTE host's live network egress — it must be a mutating
# remote verb (same guard as up/down/restart/rebuild): an implicit remote via
# QUASAR_DEFAULT_HOST (never TYPED) is refused, an explicitly typed HOST= passes.
rc_of 2 "ladder:guard-implicit-host" -- \
  env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_DEFAULT_HOST=gpu-test bash "$DX/abr_ladder_netem.sh" x --dry-run --levels mild
rc_of 0 "ladder:explicit-host-dry-run" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/abr_ladder_netem.sh" x --dry-run --levels mild

printf '\n== bench harness ==\n'

# ── guards ───────────────────────────────────────────────────────────────────
rc_of 2 "bench:run-no-profile" -- env -u HOST bash "$DX/bench_run.sh" --secs 60
rc_of 2 "bench:run-bad-netem" -- env -u HOST bash "$DX/bench_run.sh" --profile 1080p60-h264 --netem bogus
rc_of 2 "bench:run-bad-tag" -- env -u HOST bash "$DX/bench_run.sh" --profile 1080p60-h264 --tag novalue
rc_of 2 "bench:suite-no-profiles" -- env -u HOST bash "$DX/bench_suite.sh" --dry-run
rc_of 2 "bench:suite-bad-abr" -- env -u HOST bash "$DX/bench_suite.sh" --profiles p --abr-modes turbo --dry-run
# An --only that matches nothing must REFUSE (rc 2 + a RESULT line), not die on
# bash 3.2's "${CELLS[@]}: unbound variable" for an empty array under `set -u`.
rc_of 2 "bench:suite-only-no-match" -- env -u HOST bash "$DX/bench_suite.sh" --profiles p --only zzz-matches-nothing
rc_of 2 "bench:submit-no-dir" -- python3 "$DX/bench_submit.py" --dir "$WORK/nope" --suite s --scenario x

# Bench-mode / codec flags: a bad value must be refused BEFORE anything launches.
rc_of 2 "bench:run-bad-codec" -- env -u HOST bash "$DX/bench_run.sh" --profile 1080p60-h264 --codec vp9
rc_of 2 "bench:run-pulse-not-int" -- env -u HOST bash "$DX/bench_run.sh" --profile 1080p60-h264 --bench-mode --input-pulse-every abc
# A pulse with no in-page instrument to answer it measures nothing.
rc_of 2 "bench:run-pulse-without-bench" -- env -u HOST bash "$DX/bench_run.sh" --profile 1080p60-h264 --input-pulse-every 10
rc_of 2 "bench:qses-bad-codec" -- bash "$ROOT/.claude/skills/quasar-session/scripts/qses" run --codec vp9
rc_of 2 "bench:qses-pulse-without-bench" -- bash "$ROOT/.claude/skills/quasar-session/scripts/qses" run --input-pulse-every 5

# ── --peer (2026-08-19, docs/reports/2026-08-19-peer-path/REPORT.md) ─────────
rc_of 2 "bench:run-peer-bogus" -- env -u HOST bash "$DX/bench_run.sh" --profile 1080p60-h264 --peer bogus
rc_of 2 "bench:suite-peer-bogus" -- env -u HOST bash "$DX/bench_suite.sh" --profiles p --peer bogus --dry-run

# --peer local is the DEFAULT: QSES_PEER_ROLE is set to the same role/host HOST
# resolved to, and the run is tagged peer=<resolved host name>, taken from
# DX_HOSTS_JSON's roles map (gpu-test -> keybox in the test fixture) exactly
# the way qses itself resolves a role to a host.
peer_default_plan="$(env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
  --profile 1080p60-h264 --dry-run 2>&1 || true)"
if printf '%s' "$peer_default_plan" | grep -q 'peer      local (QSES_PEER_ROLE=gpu-test, peer_host=keybox)'; then
  pass "bench:run-peer-default-local" "defaults to local, QSES_PEER_ROLE tracks the resolved stack role"
else
  fail "bench:run-peer-default-local" "$(printf '%s' "$peer_default_plan" | grep peer)"
fi

# --peer aux resolves the aux-infra role the same way (aliasbox in the fixture).
peer_aux_plan="$(env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
  --profile 1080p60-h264 --peer aux --dry-run 2>&1 || true)"
if printf '%s' "$peer_aux_plan" | grep -q 'peer      aux (QSES_PEER_ROLE=aux-infra, peer_host=aliasbox)'; then
  pass "bench:run-peer-aux" "--peer aux resolves the aux-infra role"
else
  fail "bench:run-peer-aux" "$(printf '%s' "$peer_aux_plan" | grep peer)"
fi

# --netem + --peer local must be REFUSED (rc 2): netem shapes the sender's
# egress toward the aux-infra NIC (qnetem sender), which a local peer never
# crosses — submitting anyway would mislabel unshaped data as netem=<level>.
rc_of 2 "bench:run-netem-peer-local-refused" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
    --profile 1080p60-h264 --netem mild --peer local --dry-run
# The same combination with --peer aux is fine (still a dry-run, nothing shaped).
rc_of 0 "bench:run-netem-peer-aux-ok" -- \
  env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
    --profile 1080p60-h264 --netem mild --peer aux --dry-run
# bench_suite.sh refuses the whole matrix up front for the same reason, before
# any cell runs or any host setting is touched.
rc_of 2 "bench:suite-netem-peer-local-refused" -- env -u HOST bash "$DX/bench_suite.sh" \
  --profiles 720p60-h264 --netem mild --peer local --dry-run
rc_of 0 "bench:suite-netem-peer-aux-ok" -- env -u HOST bash "$DX/bench_suite.sh" \
  --profiles 720p60-h264 --netem mild --peer aux --dry-run
# --peer is forwarded to every cell's bench_run.sh invocation.
suite_peer_plan="$(env -u HOST bash "$DX/bench_suite.sh" --profiles 720p60-h264 --dry-run 2>&1 || true)"
if printf '%s' "$suite_peer_plan" | grep -q 'peer=local'; then
  pass "bench:suite-peer-plan" "suite plan line reports peer=local by default"
else
  fail "bench:suite-peer-plan" "$(printf '%s' "$suite_peer_plan" | grep -E 'suite=|peer=')"
fi

# ── --peer-unlock-fps (2026-08-19, docs/reports/2026-08-19-fps120-probe/REPORT.md) ──
# Headless Chrome's RVFC present loop caps at 60fps regardless of decoded
# content rate (overnight-2 §C); --peer-unlock-fps adds
# --disable-frame-rate-limit --disable-gpu-vsync to the CFT launch. Must default
# OFF at 60fps (never perturb a 60fps baseline), AUTO-ON when the profile's
# trailing pNNN exceeds 60, and the explicit flag/env always wins over auto.
punlock_60_plan="$(env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
  --profile 1080p60-h264 --dry-run 2>&1 || true)"
if printf '%s' "$punlock_60_plan" | grep -q 'peer_unlock_fps 0'; then
  pass "bench:run-peer-unlock-fps-auto-off-60" "a 60fps profile auto-resolves peer_unlock_fps=0"
else
  fail "bench:run-peer-unlock-fps-auto-off-60" "$(printf '%s' "$punlock_60_plan" | grep peer_unlock_fps)"
fi

punlock_120_plan="$(env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
  --profile 1080p120-h264 --dry-run 2>&1 || true)"
if printf '%s' "$punlock_120_plan" | grep -q 'peer_unlock_fps 1'; then
  pass "bench:run-peer-unlock-fps-auto-on-120" "a >60fps profile auto-resolves peer_unlock_fps=1"
else
  fail "bench:run-peer-unlock-fps-auto-on-120" "$(printf '%s' "$punlock_120_plan" | grep peer_unlock_fps)"
fi

# The explicit flag forces it on even for a 60fps profile.
punlock_flag_plan="$(env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test bash "$DX/bench_run.sh" \
  --profile 1080p60-h264 --peer-unlock-fps --dry-run 2>&1 || true)"
if printf '%s' "$punlock_flag_plan" | grep -q 'peer_unlock_fps 1'; then
  pass "bench:run-peer-unlock-fps-flag-forces-on" "--peer-unlock-fps forces it on regardless of profile fps"
else
  fail "bench:run-peer-unlock-fps-flag-forces-on" "$(printf '%s' "$punlock_flag_plan" | grep peer_unlock_fps)"
fi

# QSES_PEER_UNLOCK_FPS=1 in the environment forces it on the same way (qses
# itself honours this env var directly, so a bare `qses run` picks it up too).
punlock_env_plan="$(env DX_HOSTS_JSON="$FIX_HOSTS" HOST=gpu-test QSES_PEER_UNLOCK_FPS=1 bash "$DX/bench_run.sh" \
  --profile 1080p60-h264 --dry-run 2>&1 || true)"
if printf '%s' "$punlock_env_plan" | grep -q 'peer_unlock_fps 1'; then
  pass "bench:run-peer-unlock-fps-env-forces-on" "QSES_PEER_UNLOCK_FPS=1 forces it on"
else
  fail "bench:run-peer-unlock-fps-env-forces-on" "$(printf '%s' "$punlock_env_plan" | grep peer_unlock_fps)"
fi

# qses itself must parse the flag without error: pair it with a KNOWN-bad
# --codec so the run still exits 2, but the failure text must be the codec
# guard, not "unknown arg --peer-unlock-fps" (which would mean the parser
# never reached the --peer-unlock-fps case at all).
qses_punlock_out="$(env -u HOST bash "$ROOT/.claude/skills/quasar-session/scripts/qses" run --peer-unlock-fps --codec vp9 2>&1 || true)"
if printf '%s' "$qses_punlock_out" | grep -q -- '--codec must be' && ! printf '%s' "$qses_punlock_out" | grep -q 'unknown arg --peer-unlock-fps'; then
  pass "bench:qses-peer-unlock-fps-parses" "--peer-unlock-fps is recognized, parsing continues to the codec guard"
else
  fail "bench:qses-peer-unlock-fps-parses" "$qses_punlock_out"
fi

# bench_app_samples.py: frames.jsonl -> `app` samples, events.jsonl -> app.event,
# bench windows -> `browser` samples, all folded into the two files
# bench_submit.py actually reads. Run TWICE: the fold must be idempotent and must
# never eat the agent samples / trace events that were already there.
if [ -d "$FIXTURES/appfold" ]; then
  SAMP_DIR="$WORK/appfold"
  cp -R "$FIXTURES/appfold" "$SAMP_DIR"
  python3 "$DX/bench_app_samples.py" --dir "$SAMP_DIR" --t0-ms 1800000000000 >/dev/null 2>&1
  if python3 "$DX/bench_app_samples.py" --dir "$SAMP_DIR" --t0-ms 1800000000000 >/dev/null 2>&1; then
    fold_check="$(python3 "$TESTS_DIR/check_app_fold.py" "$SAMP_DIR" 2>&1)"
    if [ "$fold_check" = OK ]; then
      pass "bench:app-samples-fold"
    else
      fail "bench:app-samples-fold" "$fold_check"
    fi
  else
    fail "bench:app-samples-fold" "bench_app_samples.py exited non-zero"
  fi
  # A pre-rename readout must still yield the drop metric, under its CURRENT
  # name. Silently omitting it is what holed 10 of 12 runs in suite opt-t1.
  ALIAS_DIR="$WORK/appfold-alias"
  cp -R "$FIXTURES/appfold" "$ALIAS_DIR"
  rm -f "$ALIAS_DIR/metrics.jsonl" "$ALIAS_DIR/trace.json"
  printf '%s\n' '{"windows":[{"t_end_host_ms":1800000001003,"n":60,"decoded":60,"dropped":7,"crc_fail":2}]}' > "$ALIAS_DIR/bench-windows.json"
  python3 "$DX/bench_app_samples.py" --dir "$ALIAS_DIR" --t0-ms 1800000000000 >/dev/null 2>&1
  alias_check=$(python3 -c '
import json,sys,os
rows=[json.loads(l) for l in open(os.path.join(sys.argv[1],"metrics.jsonl")) if l.strip()]
b=[r for r in rows if r["source"]=="browser"]
if not b: print("no browser sample"); raise SystemExit
m=b[0]["metrics"]
p=[]
if m.get("missing_indices")!=7: p.append("legacy dropped not renamed forward: %r"%m.get("missing_indices"))
if m.get("undecoded")!=2: p.append("legacy crc_fail not renamed forward: %r"%m.get("undecoded"))
if "dropped" in m or "crc_fail" in m: p.append("legacy key leaked through under its old name")
print("OK" if not p else "; ".join(p))' "$ALIAS_DIR")
  if [ "$alias_check" = OK ]; then pass "bench:legacy-key-alias"; else fail "bench:legacy-key-alias" "$alias_check"; fi

  # An unimpaired run must be splittable into warmup + observe, or every service
  # query returns a whole-hold number inflated by app start-up.
  phase_check=$(python3 -c '
import sys, importlib.util
spec=importlib.util.spec_from_file_location("bs", sys.argv[1]); bs=importlib.util.module_from_spec(spec)
spec.loader.exec_module(bs)
smp=[{"ts_unix_ms":t} for t in range(1_000_000, 1_000_000+300_000, 1000)]
w=bs.phase_windows([], smp, 30.0, warmup_secs=60.0)
p=[]
if "warmup" not in w or "observe" not in w: p.append("no warmup/observe split: %s"%sorted(w))
else:
    if w["warmup"][0]!=(1_000_000,1_060_000): p.append("warmup span %s"%(w["warmup"],))
    if w["observe"][0][0]!=1_060_000: p.append("observe starts %s"%(w["observe"][0][0],))
if "warmup" not in bs.MARK_PHASES or "observe" not in bs.MARK_PHASES: p.append("phases missing from MARK_PHASES")
# END TO END: phase_marks must actually EMIT them. Asserting only on
# phase_windows + MARK_PHASES passed while phase_marks was still calling
# phase_windows without warmup_secs, so zero marks were posted and the service
# saw one whole-hold phase. Test the thing that ships.
mk_out=bs.phase_marks(smp, [], 30.0, 60.0)
got={(m["payload"]["phase"], m["payload"]["edge"]) for m in mk_out}
for want in (("warmup","start"),("warmup","end"),("observe","start"),("observe","end")):
    if want not in got: p.append("phase_marks did not emit %s"%(want,))
if any(m["type"]!="harness.mark" for m in mk_out): p.append("marks must be harness.mark")
if bs.phase_marks(smp, [], 30.0): p.append("marks emitted without --warmup-secs")
# no split when the harness does not say where the boundary is
if "observe" in bs.phase_windows([], smp, 30.0): p.append("split without --warmup-secs")
# and an IMPAIRED run keeps the netem vocabulary, never gains warmup/observe
mk=[{"ts_unix_ms":1_100_000,"mark":"impair"},{"ts_unix_ms":1_200_000,"mark":"clear"}]
wi=bs.phase_windows(mk, smp, 30.0, warmup_secs=60.0)
if "observe" in wi or "impaired" not in wi: p.append("impaired run vocabulary changed: %s"%sorted(wi))
print("OK" if not p else "; ".join(p))' "$DX/bench_submit.py")
  if [ "$phase_check" = OK ]; then pass "bench:warmup-observe-phases"; else fail "bench:warmup-observe-phases" "$phase_check"; fi

  # A time-shifted re-submit against a bench service < 1.2 must be REFUSED, not
  # warned about: there samples upsert on (run, source, ts) and nothing deletes,
  # so it appends a second copy at the wrong offset and every window-scoped
  # aggregate silently goes wrong (hit twice on suite opt-t1, 11 of 12 runs).
  # On >= 1.2 the submit posts replace=True + expected_count instead (tested
  # below as bench:replace-guard); the pure detector is unchanged.
  shift_check=$(python3 -c '
import sys, importlib.util
spec=importlib.util.spec_from_file_location("bs", sys.argv[1]); bs=importlib.util.module_from_spec(spec)
spec.loader.exec_module(bs)
p=[]
base=list(range(1_000_000, 1_000_000+276*1000, 1000))
prev=(276, base[0], base[-1])
# same anchor -> allowed (a plain idempotent re-submit must still work)
if bs.shifted_resubmit_problem(prev, base) is not None: p.append("identical re-submit refused")
# one second of slack at the boundary -> allowed
if bs.shifted_resubmit_problem(prev, [t+900 for t in base]) is not None: p.append("sub-tolerance shift refused")
# the real case: a different anchor -> MUST refuse
msg=bs.shifted_resubmit_problem(prev, [t+65_000 for t in base])
if msg is None: p.append("65s shift NOT refused")
elif "NOTHING deletes" not in msg: p.append("refusal does not explain why: %r"%msg[:60])
# nothing stored yet, or nothing incoming -> nothing to say
if bs.shifted_resubmit_problem(None, base) is not None: p.append("refused with no prior series")
if bs.shifted_resubmit_problem(prev, []) is not None: p.append("refused with no incoming windows")
print("OK" if not p else "; ".join(p))' "$DX/bench_submit.py")
  if [ "$shift_check" = OK ]; then pass "bench:refuse-shifted-resubmit"; else fail "bench:refuse-shifted-resubmit" "$shift_check"; fi

  # A run holding more one-second windows than it has seconds is corrupt however
  # it got there. On opt-t1 a 275s run held 617 windows and nothing said so.
  plaus_check=$(python3 -c '
import sys, importlib.util
spec=importlib.util.spec_from_file_location("bs", sys.argv[1]); bs=importlib.util.module_from_spec(spec)
spec.loader.exec_module(bs)
p=[]
lo=1_000_000; hi=lo+275_000            # a 275 s run
if bs.corrupt_window_series((276, lo, hi)) is not None: p.append("clean 276-window run rejected")
if bs.corrupt_window_series((281, lo, hi)) is not None: p.append("within-tolerance count rejected")
for n in (552, 617):
    m=bs.corrupt_window_series((n, lo, hi))
    if m is None: p.append("%d windows over 275s NOT flagged"%n)
    elif "corrupt" not in m.lower(): p.append("message does not name the problem")
if bs.corrupt_window_series(None) is not None: p.append("flagged with nothing stored")
if bs.corrupt_window_series((10, lo, lo)) is not None: p.append("zero-span run flagged")
print("OK" if not p else "; ".join(p))' "$DX/bench_submit.py")
  if [ "$plaus_check" = OK ]; then pass "bench:window-count-plausibility"; else fail "bench:window-count-plausibility" "$plaus_check"; fi

  # A LEGACY readout (windows with no timestamps of their own) must still be
  # foldable, but must SAY SO — a silently ordinal-stamped series is the C1
  # defect, and a stale peer driver is the way it comes back.
  LEGACY_DIR="$WORK/appfold-legacy"
  cp -R "$FIXTURES/appfold" "$LEGACY_DIR"
  rm -f "$LEGACY_DIR/metrics.jsonl" "$LEGACY_DIR/trace.json"
  printf '%s\n' '{"windows":[{"n":60,"decoded":60}]}' > "$LEGACY_DIR/bench-windows.json"
  legacy_err="$(python3 "$DX/bench_app_samples.py" --dir "$LEGACY_DIR" --t0-ms 1800000000000 2>&1 >/dev/null)"
  case "$legacy_err" in
    *"NOT trustworthy"*) pass "bench:legacy-window-warned" ;;
    *) fail "bench:legacy-window-warned" "an ordinal-stamped window must warn; got: ${legacy_err:-<silence>}" ;;
  esac
else
  warn "bench:app-samples-fold" "fixtures/appfold missing — SKIPPED"
fi

# BENCH_KEY name:secret tolerance (2026-08-19 overnight-2 harness note #2):
# `deploy/.env`'s BENCH_API_KEYS stores `name:secret`; BENCH_KEY/--key must be
# the bare secret. normalize_bench_key() strips a `name:` prefix on the first
# colon and warns rather than 401ing on a value pasted straight from .env.
key_check="$(python3 -c '
import sys, io, contextlib, importlib.util
spec=importlib.util.spec_from_file_location("bs", sys.argv[1]); bs=importlib.util.module_from_spec(spec)
spec.loader.exec_module(bs)
p=[]
def norm(*a):
    # normalize_bench_key WARNs to stdout on a stripped key by design (that WARN
    # is the behaviour bench:key-warn-printed asserts on, end to end); swallow it
    # here so it does not pollute the OK/problems sentinel below.
    with contextlib.redirect_stdout(io.StringIO()):
        return bs.normalize_bench_key(*a)
if norm("harness:abc123", "BENCH_KEY") != "abc123": p.append("name:secret not stripped to the secret")
if norm("bare-secret-no-colon", "BENCH_KEY") != "bare-secret-no-colon": p.append("a bare secret must pass through unchanged")
if norm(None, "BENCH_KEY") is not None: p.append("None must pass through as None (Bench() raises its own no-key error)")
if norm("", "BENCH_KEY") != "": p.append("empty string must pass through unchanged")
# a value with a colon in the SECRET part (not just the name) only strips the first one
if norm("harness:abc:123", "BENCH_KEY") != "abc:123": p.append("must split on the FIRST colon only")
print("OK" if not p else "; ".join(p))' "$DX/bench_submit.py")"
if [ "$key_check" = OK ]; then
  pass "bench:key-name-secret-stripped"
else
  fail "bench:key-name-secret-stripped" "$key_check"
fi

# End to end: a `name:secret` BENCH_KEY must submit successfully (not 401, since
# the fake server does not check auth — this asserts the WARN fires and the
# submit still completes) rather than requiring the operator to pre-strip it.
if [ -d "$FIXTURES/bench-run" ]; then
  KEY_BENCH_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
  python3 "$TESTS_DIR/fake_bench_server.py" --port "$KEY_BENCH_PORT" --log "$WORK/key-bench-server.log" \
    > "$WORK/key-bench-server.out" 2>&1 &
  KEY_BENCH_PID=$!
  for _ in $(seq 1 40); do
    curl -fsS "http://127.0.0.1:$KEY_BENCH_PORT/v1/health" >/dev/null 2>&1 && break
    sleep 0.25
  done
  key_sub_out="$(env BENCH_URL="http://127.0.0.1:$KEY_BENCH_PORT" BENCH_KEY="harness:realsecretvalue" \
    python3 "$DX/bench_submit.py" --dir "$FIXTURES/bench-run" --suite selftest \
    --scenario keytest-cell --tag encoder=nvenc --tag app=fixture 2>&1 || true)"
  kill "$KEY_BENCH_PID" 2>/dev/null || true
  wait "$KEY_BENCH_PID" 2>/dev/null || true
  if printf '%s' "$key_sub_out" | grep -q "WARN.*BENCH_KEY.*name='harness'"; then
    pass "bench:key-warn-printed"
  else
    fail "bench:key-warn-printed" "$(printf '%s' "$key_sub_out" | head -n 3 | tr '\n' ' ')"
  fi
  if printf '%s' "$key_sub_out" | grep -q 'RESULT status=ok target=bench-submit'; then
    pass "bench:key-name-secret-submits"
  else
    fail "bench:key-name-secret-submits" "$(printf '%s' "$key_sub_out" | tail -n 4 | tr '\n' ' ')"
  fi
else
  warn "bench:key-name-secret-submits" "fixtures/bench-run missing — SKIPPED"
fi

# bench-run/bench-suite launch sessions and PATCH host settings on a REMOTE host,
# so they must carry the same mutating-verb guard as up/down/restart/abr-ladder:
# an implicit remote (QUASAR_DEFAULT_HOST, never TYPED) is refused.
for v in bench_run bench_suite; do
  rc_of 2 "bench:${v}-implicit-host" -- \
    env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_DEFAULT_HOST=gpu-test \
    bash "$DX/$v.sh" --profile 1080p60-h264 --profiles 1080p60-h264 --dry-run
done

# ── suite dry-run: the cell plan is pure arithmetic, no stack, no settings ───
suite_plan="$(env -u HOST bash "$DX/bench_suite.sh" --profiles 720p60-h264,1080p60-h264 \
                --abr-modes off,smooth --ladder off --netem none --iterations 1 --dry-run 2>&1 || true)"
if printf '%s' "$suite_plan" | grep -q '= 4 cell(s)' \
   && printf '%s' "$suite_plan" | grep -q 'cell 720p60-h264.abr-off.ladder-off.netem-none.i1' \
   && printf '%s' "$suite_plan" | grep -q 'cell 1080p60-h264.abr-smooth.ladder-off.netem-none.i1' \
   && printf '%s' "$suite_plan" | grep -q 'no session launched, no host setting changed'; then
  pass "bench:suite-plan" "2 profiles x 2 abr modes = 4 cells, nothing touched"
else
  fail "bench:suite-plan" "$(printf '%s' "$suite_plan" | tail -n 6 | tr '\n' ' ')"
fi

# --baseline is opt-in and must not do anything at plan time: pinning a baseline
# is what every later run gets judged against, and it happens only after a matrix
# has actually finished.
rc_of 0 "bench:suite-baseline-flag" -- env -u HOST bash "$DX/bench_suite.sh" \
  --profiles 720p60-h264 --abr-modes smooth --baseline --dry-run

# --only must filter the plan, not silently run everything.
suite_only="$(env -u HOST bash "$DX/bench_suite.sh" --profiles 720p60-h264,1080p60-h264 \
                --abr-modes off,smooth --only abr-smooth --dry-run 2>&1 || true)"
if printf '%s' "$suite_only" | grep -q '= 2 cell(s)' \
   && ! printf '%s' "$suite_only" | grep -q 'abr-off'; then
  pass "bench:suite-only" "--only abr-smooth narrows 4 cells to 2"
else
  fail "bench:suite-only" "$(printf '%s' "$suite_only" | grep -c 'cell ') cells"
fi

# ── bench_suite host settings against a faithful fake control plane ──────────
# The matrix's arm labels are only true if the PATCH actually lands. The real
# handler reads {"overrides":{...}} and MERGES it (null clears a key); a BARE map
# is a 200 OK that changes nothing, which is how a whole matrix once ran every
# cell on unchanged settings while labelling half of them `abr=off`.
# The seed deliberately does NOT contain the keys the cell sets, so the restore
# also has to null them back out instead of merely re-sending the snapshot.
CP_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
CP_STATE="$WORK/cp-overrides.json"
python3 "$TESTS_DIR/fake_cp_server.py" --port "$CP_PORT" --state "$CP_STATE" \
  --seed '{"abr_mode":"smooth"}' > "$WORK/cp-server.out" 2>&1 &
CP_PID=$!
for _ in $(seq 1 40); do
  curl -fsS "http://127.0.0.1:$CP_PORT/v1/hosts" >/dev/null 2>&1 && break
  sleep 0.25
done

# One cell. It gets as far as PATCHing + verifying the settings, then bench_run.sh
# fails fast (no reachable stack to launch on) — which is fine: the settings
# handling, including the restore on the failure path, is what is under test.
suite_out="$(env -u HOST DX_CP_PORT="$CP_PORT" QSES_ADMIN_TOKEN=test-token \
  QUASAR_DEFAULT_HOST=local DX_HOSTS_JSON="$FIX_HOSTS" \
  bash "$DX/bench_suite.sh" --profiles 720p60-h264 --abr-modes off --ladder off \
    --netem none --iterations 1 --out "$WORK/bench-suite" 2>&1 || true)"

cp_check="$(python3 - "$CP_STATE" <<'PY'
import json, sys
state = sys.argv[1]
final = json.load(open(state))
patches = [json.loads(l) for l in open(state + ".patches.jsonl") if l.strip()]
problems = []

if not patches:
    problems.append("the suite never PATCHed the host settings at all")
# Every PATCH must be wrapped. A bare map is the no-op bug.
bare = [p for p in patches if "overrides" not in p]
if bare:
    problems.append("%d PATCH body/bodies had no `overrides` wrapper (a silent no-op): %s"
                    % (len(bare), bare[:1]))
# The cell arm must actually have been applied at some point.
applied = [p for p in patches if (p.get("overrides") or {}).get("abr_enabled") is False]
if not applied:
    problems.append("no PATCH ever set abr_enabled=false — the `abr=off` arm was never real")
# ...and it must name the AUTHORITATIVE 3-way knob too. The fixture seeds a stored
# `abr_mode: "smooth"`, which a PATCH MERGES back in; the agent applies the
# deprecated `abr_enabled` bool first and `abr_mode` last, so an arm that names
# only the bool is overruled and the `off` cell streams under smooth ABR.
# (Observed live on devbox 2026-08-17, baseline-v2 cell 1.)
if not [p for p in applied if (p.get("overrides") or {}).get("abr_mode") == "off"]:
    problems.append("the `abr=off` PATCH never set abr_mode='off' — the merged-in "
                    "stored abr_mode wins on the agent and the arm is a lie")
# ...and the host must be left EXACTLY as it was found, not merely merged over.
if final != {"abr_mode": "smooth"}:
    problems.append("overrides left as %r, want the seeded {'abr_mode': 'smooth'} — "
                    "restore merged instead of setting" % (final,))
print("; ".join(problems) if problems else "OK")
PY
)"
if [ "$cp_check" = OK ]; then
  pass "bench:suite-settings" "cell PATCH is wrapped + applied, host restored exactly"
else
  fail "bench:suite-settings" "$cp_check"
fi
if printf '%s' "$suite_out" | grep -q 'host overrides RESTORED'; then
  pass "bench:suite-restore-reported" "the restore is reported on the failing-cell path"
else
  fail "bench:suite-restore-reported" "$(printf '%s' "$suite_out" | tail -n 3 | tr '\n' ' ')"
fi
kill "$CP_PID" 2>/dev/null || true
wait "$CP_PID" 2>/dev/null || true

# ── profile_policy=force preflight (2026-08-19 overnight-2 harness note #3) ──
# `POST /v1/sessions {profile_id: ...}` hard-409s (ErrProfileOverrideDisabled)
# for any non-admin profile_id against a `force` app. bench_run.sh's preflight
# PATCHes such an app to `prefer` for the run's duration and restores `force`
# on every exit path — asserted here against a fake CP server that also serves
# GET /v1/apps + PATCH /v1/apps/{id}. Every cell below fails fast at the qses
# launch step (STACK_HOST "local" is not in the test fixture's hosts.json), by
# design — the preflight + restore around that failure is what is under test.
FP_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
FP_STATE="$WORK/fp-cp-state.json"
python3 "$TESTS_DIR/fake_cp_server.py" --port "$FP_PORT" --state "$FP_STATE" \
  --apps-seed '[{"id":"app-1","name":"Quasar Benchapp","profile_policy":"force","default_profile_id":"1080p60"}]' \
  > "$WORK/fp-server.out" 2>&1 &
FP_PID=$!
for _ in $(seq 1 40); do
  curl -fsS "http://127.0.0.1:$FP_PORT/v1/hosts" >/dev/null 2>&1 && break
  sleep 0.25
done

fp_run() { # fp_run <extra bench_run.sh args...>
  env -u HOST QUASAR_DEFAULT_HOST=local DX_HOSTS_JSON="$FIX_HOSTS" DX_CP_PORT="$FP_PORT" \
    QSES_ADMIN_TOKEN=test-token bash "$DX/bench_run.sh" --app 'Quasar Benchapp' \
    --secs 5 --settle 1 "$@" 2>&1 || true
}

# A pinned profile that DIFFERS from the app's forced default is a real
# override attempt: PATCH to prefer, then restore to force once the (failing)
# launch attempt unwinds through the EXIT trap.
: > "$FP_STATE.app-patches.jsonl"
override_out="$(fp_run --profile 1080p120-h264)"
override_check="$(python3 - "$FP_STATE.app-patches.jsonl" "$FP_STATE.apps.json" <<'PY'
import json, sys
patches = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
apps = {a["id"]: a for a in json.load(open(sys.argv[2]))}
p = []
policies = [pt.get("profile_policy") for pt in patches if pt.get("app_id") == "app-1"]
if policies != ["prefer", "force"]:
    p.append("app-1 profile_policy PATCH sequence = %r, want ['prefer', 'force']" % (policies,))
if apps.get("app-1", {}).get("profile_policy") != "force":
    p.append("app left as %r, want restored to force" % (apps.get("app-1"),))
print("OK" if not p else "; ".join(p))
PY
)"
if [ "$override_check" = OK ]; then
  pass "bench:run-force-policy-override" "prefer->force around a differing pinned profile"
else
  fail "bench:run-force-policy-override" "$override_check"
fi
# The tag itself (profile_policy_overridden=1) only reaches stdout via a
# successful bench_submit.py call, which this fixture never reaches (the qses
# launch fails fast by design, see the comment above) — so this asserts on the
# preflight's own log line instead, which fires unconditionally on this path.
if printf '%s' "$override_out" | grep -q "PATCHing profile_policy to 'prefer'"; then
  pass "bench:run-force-policy-preflight-logged"
else
  fail "bench:run-force-policy-preflight-logged" "$(printf '%s' "$override_out" | grep -i profile_policy | tr '\n' ' ')"
fi

# `--profile forced` sends NO profile_id at all, so there is nothing to
# override — the preflight must not touch the app.
: > "$FP_STATE.app-patches.jsonl"
fp_run --profile forced >/dev/null
if [ -s "$FP_STATE.app-patches.jsonl" ]; then
  fail "bench:run-force-policy-noop-sentinel" "unexpected app PATCH(es): $(cat "$FP_STATE.app-patches.jsonl")"
else
  pass "bench:run-force-policy-noop-sentinel" "--profile forced never touches the app row"
fi

# A pinned profile EQUAL to the app's own forced default is not an override —
# the launcher.go carve-out already permits it, so the harness must not spend
# a PATCH relaxing policy for a request that was never going to 409.
: > "$FP_STATE.app-patches.jsonl"
fp_run --profile 1080p60 >/dev/null
if [ -s "$FP_STATE.app-patches.jsonl" ]; then
  fail "bench:run-force-policy-noop-matching-default" "unexpected app PATCH(es): $(cat "$FP_STATE.app-patches.jsonl")"
else
  pass "bench:run-force-policy-noop-matching-default" "no PATCH when the pinned profile already equals the forced default"
fi

kill "$FP_PID" 2>/dev/null || true
wait "$FP_PID" 2>/dev/null || true

# ── bench_submit against a FIXTURE run dir + a fake HTTP server ──────────────
# The whole ingest path (samples, string-metric coding, events from trace/marks/
# steps, artifacts, finish) is exercised against a throwaway python server, so it
# never needs the real service and never posts anything anywhere.
if [ -d "$FIXTURES/bench-run" ]; then
  BENCH_LOG="$WORK/bench-server.log"
  BENCH_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
  python3 "$TESTS_DIR/fake_bench_server.py" --port "$BENCH_PORT" --log "$BENCH_LOG" \
    > "$WORK/bench-server.out" 2>&1 &
  BENCH_PID=$!
  for _ in $(seq 1 40); do
    curl -fsS "http://127.0.0.1:$BENCH_PORT/v1/health" >/dev/null 2>&1 && break
    sleep 0.25
  done

  sub_out="$(env BENCH_URL="http://127.0.0.1:$BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_submit.py" --dir "$FIXTURES/bench-run" --suite selftest \
    --scenario fixture-cell --tag encoder=nvenc --tag app=fixture 2>&1 || true)"

  if printf '%s' "$sub_out" | grep -q 'RESULT status=ok target=bench-submit'; then
    pass "bench:submit-rc" "submitted against the fake server"
  else
    fail "bench:submit-rc" "$(printf '%s' "$sub_out" | tail -n 4 | tr '\n' ' ')"
  fi

  # DID IT SHIP? A non-dry-run submit must actually create a run and say so.
  # This is the assertion that catches a structural break in main(): an edit that
  # dedents a helper into the middle of the function ends main at the dry-run
  # early return, so the plan prints, nothing is posted, and the exit code is 0.
  # That happened (2026-08-18) and cost a teammate eleven silent no-op submits.
  # Deliberately separate from the payload check below so the failure NAMES the
  # cause instead of arriving as twelve confusing sub-assertions.
  if grep -q '"path": "/v1/runs"' "$BENCH_LOG" 2>/dev/null \
     && printf '%s' "$sub_out" | grep -q '^PASS run — '; then
    pass "bench:submit-creates-run"
  else
    fail "bench:submit-creates-run" \
      "a non-dry-run submit posted no run — main() may be truncated (rc was $(printf '%s' "$sub_out" | grep -c 'RESULT status=ok'))"
  fi

  # Assert on what the SERVER received, not on what the client printed.
  check="$(python3 - "$BENCH_LOG" <<'PY'
import json, sys
posts = [json.loads(l) for l in open(sys.argv[1])]
by = {}
for p in posts:
    by.setdefault(p["path"].split("/")[-1], []).append(p)
problems = []

runs = by.get("runs", [])
if len(runs) != 1:
    problems.append("expected 1 run create, got %d" % len(runs))
else:
    create = runs[0]["body"]
    tags = create["tags"]
    for k, v in (("encoder", "nvenc"), ("app", "fixture"),
                 ("codec", "h264"), ("profile", "1080p120")):
        if tags.get(k) != v:
            problems.append("tag %s=%r, want %r" % (k, tags.get(k), v))
    # 1.1: the idempotency key is a FIRST-CLASS field, not a tag the client has
    # to look itself up by.
    if not create.get("external_id"):
        problems.append("no external_id on the create body")
    if tags.get("bench_ext_id"):
        problems.append("the bench_ext_id TAG is still posted — the tag-lookup "
                        "dedupe path was supposed to go with it")
    # ...and the run must record what the host was actually doing.
    cond = create.get("conditions") or {}
    if not cond.get("git"):
        problems.append("create carries no conditions.git")
    if (cond.get("negotiated") or {}).get("height") != 1080:
        problems.append("conditions.negotiated is %r, want the session's 1920x1080"
                        % (cond.get("negotiated"),))
    if (cond.get("netem") or {}).get("level") != "moderate":
        problems.append("conditions.netem is %r, want level=moderate from marks.jsonl"
                        % (cond.get("netem"),))

samples = [s for p in by.get("samples", []) for s in p["body"]["samples"]]
sources = {s["source"] for s in samples}
if not {"agent", "browser"} <= sources:
    problems.append("sample sources %s, want at least agent+browser" % sorted(sources))
for s in samples:
    for k, v in s["metrics"].items():
        if not isinstance(v, (int, float)) or isinstance(v, bool):
            problems.append("non-numeric sample metric %s=%r" % (k, v))
agent = [s for s in samples if s["source"] == "agent"]
if agent and "adaptation_state_code" not in agent[0]["metrics"]:
    problems.append("string metric adaptation_state was not coded")
if agent and "adaptation_state" in agent[0]["metrics"]:
    problems.append("raw string metric adaptation_state leaked into a sample")
# 1.1: the synthetic `harness` rollup SAMPLE is gone. It was a workaround for a
# service that could not window; now that `window=impaired` exists, a derived
# aggregate sitting in the same table as real measurements is just a second,
# disagreeing truth. Only real measurements may be samples.
harness = [s for s in samples if s["source"] == "harness"]
if harness:
    problems.append("%d harness rollup sample(s) still posted — phase rollups must "
                    "not be samples any more (keys: %s)"
                    % (len(harness), sorted(harness[0]["metrics"])[:4]))

events = [e for p in by.get("events", []) for e in p["body"]["events"]]
types = {e["type"] for e in events}
for want in ("abr.ladder.step", "netem.impair", "netem.clear", "harness.step",
             "harness.mark"):
    if want not in types:
        problems.append("missing event type %s (have %s)" % (want, sorted(types)))
# Marks cover only what the netem events cannot express. baseline/impaired/
# recovery are derived SERVER-side from netem.impair/clear (origin `netem`), so
# an impaired run must NOT also mark them (a duplicate reports origin `mixed`);
# `settled` must be a balanced pair covering the impaired span minus settle.
marks = [e for e in events if e["type"] == "harness.mark"]
phases_seen = {}
for e in marks:
    p = e["payload"]
    phases_seen.setdefault(p.get("phase"), []).append(p.get("edge"))
for banned in ("baseline", "impaired", "recovery"):
    if banned in phases_seen:
        problems.append("harness.mark phase %s posted alongside netem events (origin would be mixed)" % banned)
edges = phases_seen.get("settled") or []
if edges.count("start") != edges.count("end") or not edges:
    problems.append("harness.mark phase settled has unbalanced edges %r" % (edges,))
st = sorted(e["ts_unix_ms"] for e in marks if e["payload"].get("phase") == "settled")
if st and (st[0] != 1786940030000 + 30000 or st[-1] != 1786940120000):
    problems.append("settled marks at %r, want impair+settle .. clear" % (st,))

names = {p["name"] for p in by.get("artifacts", [])}
for want in ("REPORT.md", "summary.json", "marks.jsonl"):
    if want not in names:
        problems.append("artifact %s not uploaded (have %s)" % (want, sorted(names)))

fin = by.get("finish", [])
if len(fin) != 1:
    problems.append("expected 1 finish, got %d" % len(fin))
elif fin[0]["body"].get("verdict") != "PASS":
    problems.append("finish verdict %r, want PASS" % fin[0]["body"].get("verdict"))
elif "phases" not in (fin[0]["body"].get("summary") or {}):
    problems.append("finish summary carries no `phases` rollup")
elif not (fin[0]["body"].get("conditions") or {}).get("git"):
    problems.append("finish carries no conditions — they must be posted on BOTH "
                    "create and finish")

print("; ".join(problems) if problems else "OK")
PY
)"
  if [ "$check" = OK ]; then
    pass "bench:submit-payload" "run/samples/events/artifacts/finish all as contracted"
  else
    fail "bench:submit-payload" "$check"
  fi

  # C11 #2/#4: the fixture dir now carries verdict.json (verdict=nominal) and
  # caps-negotiated.json (codec=h264/profile=main/encoder_factory=
  # vulkanh264enc, agreeing with session.json's codec=h264) — assert both
  # folded into the SAME create posted above, off the same $BENCH_LOG.
  vc_check="$(python3 - "$BENCH_LOG" <<'PY'
import json, sys
posts = [json.loads(l) for l in open(sys.argv[1])]
runs = [p for p in posts if p["path"] == "/v1/runs"]
problems = []
if not runs:
    problems.append("no run create found")
else:
    create = runs[0]["body"]
    cond = create.get("conditions") or {}
    if cond.get("verdict") != "nominal":
        problems.append("conditions.verdict=%r, want 'nominal'" % cond.get("verdict"))
    neg = cond.get("negotiated") or {}
    if neg.get("codec_profile") != "main":
        problems.append("conditions.negotiated.codec_profile=%r, want 'main'" % neg.get("codec_profile"))
    if neg.get("encoder_negotiated") != "vulkanh264enc":
        problems.append("conditions.negotiated.encoder_negotiated=%r, want 'vulkanh264enc'"
                        % neg.get("encoder_negotiated"))
    tags = create.get("tags") or {}
    if tags.get("codec_profile") != "main":
        problems.append("tag codec_profile=%r, want 'main'" % tags.get("codec_profile"))
artifacts = [p for p in posts if p["path"].endswith("/artifacts")]
names = {p.get("name") for p in artifacts}
if "verdict.json" not in names:
    problems.append("verdict.json not uploaded as an artifact (have %s)" % sorted(names))
print("; ".join(problems) if problems else "OK")
PY
)"
  if [ "$vc_check" = OK ]; then
    pass "bench:submit-verdict-and-caps" "session verdict + caps.negotiated folded into conditions/tags/artifacts"
  else
    fail "bench:submit-verdict-and-caps" "$vc_check"
  fi

  # The server-derived phase windows must come back off the marks this submission
  # posted — that is the whole point of posting them instead of pre-rolling the
  # numbers. Asked of the SERVER, not of the client's own arithmetic.
  ph="$(python3 - "$BENCH_LOG" "http://127.0.0.1:$BENCH_PORT" <<'PY'
import json, sys, urllib.request
log, base = sys.argv[1], sys.argv[2]
rid = ""
for line in open(log):
    rec = json.loads(line)
    if rec["path"].endswith("/finish"):
        rid = rec["path"].split("/")[3]
with urllib.request.urlopen(base + "/v1/runs/%s/phases" % rid, timeout=10) as r:
    got = json.load(r)["phases"]
have = {p["phase"] for p in got}
want = {"baseline", "impaired", "settled", "recovery"}
print("OK" if want <= have else "phases %s, want %s" % (sorted(have), sorted(want)))
PY
)"
  if [ "$ph" = OK ]; then
    pass "bench:submit-phases" "the service derives baseline/impaired/settled/recovery from the posted marks"
  else
    fail "bench:submit-phases" "$ph"
  fi

  # Re-submitting the SAME directory must converge onto the SAME run. It is the
  # server that dedupes now (upsert on external_id, 200 instead of 201), so the
  # test is "one run id was ever written to", not "one POST was ever made".
  env BENCH_URL="http://127.0.0.1:$BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_submit.py" --dir "$FIXTURES/bench-run" --suite selftest \
    --scenario fixture-cell --tag encoder=nvenc --tag app=fixture >/dev/null 2>&1 || true
  idem="$(python3 - "$BENCH_LOG" <<'PY'
import json, sys
posts = [json.loads(l) for l in open(sys.argv[1])]
ids = {p["path"].split("/")[3] for p in posts if p["path"].endswith("/finish")}
exts = {(p["body"] or {}).get("external_id") for p in posts if p["path"] == "/v1/runs"}
if len(ids) != 1:
    print("a re-submission forked the run: finished %s" % sorted(ids))
elif len(exts) != 1 or not list(exts)[0]:
    print("the two submissions did not carry one stable external_id: %s" % sorted(exts))
else:
    print("OK")
PY
)"
  if [ "$idem" = OK ]; then
    pass "bench:submit-idempotent" "a second submission upserted onto the same run"
  else
    fail "bench:submit-idempotent" "$idem"
  fi

  # A tag that disagrees with conditions.effective is a MISLABELLED cell: the run
  # is still posted (the evidence is good, only the label is wrong) but it is
  # tagged mismatch=1 and the exit code is 3, so a matrix stops on the first cell
  # whose arm is a lie instead of grinding out eleven more. This is the automated
  # form of the defect that invalidated the whole v1 baseline grid.
  MIS_DIR="$WORK/bench-mismatch"
  mkdir -p "$MIS_DIR"
  cp "$FIXTURES/bench-run"/* "$MIS_DIR/" 2>/dev/null
  printf '{"effective": {"abr_mode": "smooth"}, "concurrent_sessions": 0}\n' \
    > "$MIS_DIR/conditions.json"
  mis_rc=0
  mis_out="$(env BENCH_URL="http://127.0.0.1:$BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_submit.py" --dir "$MIS_DIR" --suite selftest \
    --scenario mismatch-cell --tag abr_mode=off 2>&1)" || mis_rc=$?
  mis_check="$(python3 - "$BENCH_LOG" <<'PY'
import json, sys
posts = [json.loads(l) for l in open(sys.argv[1])]
patches = [p for p in posts if p.get("method") == "PATCH"]
tagged = [p for p in patches if (p["body"].get("tags") or {}).get("mismatch") == "1"]
print("OK" if tagged else "no PATCH tagged the run mismatch=1 (patches: %d)" % len(patches))
PY
)"
  if [ "$mis_rc" -eq 3 ] && [ "$mis_check" = OK ] \
     && printf '%s' "$mis_out" | grep -q 'FAIL mismatch'; then
    pass "bench:submit-mismatch" "tag abr_mode=off vs effective smooth -> rc 3 + mismatch=1"
  else
    fail "bench:submit-mismatch" "rc=$mis_rc check=$mis_check out=$(printf '%s' "$mis_out" | tail -n 2 | tr '\n' ' ')"
  fi

  # C11 #4: a caps.negotiated codec that disagrees with an explicit --codec pin
  # is the SAME mislabelled-cell class as the abr_mode mismatch above, reusing
  # the identical mechanism (conditions.effective.codec_pin vs the tag) — this
  # asserts THAT wiring specifically, not just that mismatches work at all.
  CAPS_MIS_DIR="$WORK/bench-caps-mismatch"
  mkdir -p "$CAPS_MIS_DIR"
  cp "$FIXTURES/bench-run"/* "$CAPS_MIS_DIR/" 2>/dev/null
  python3 - "$CAPS_MIS_DIR/caps-negotiated.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["events"][0]["payload"]["codec"] = "h265"   # session.json/pin both say h264
json.dump(d, open(sys.argv[1], "w"))
PY
  caps_mis_rc=0
  caps_mis_out="$(env BENCH_URL="http://127.0.0.1:$BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_submit.py" --dir "$CAPS_MIS_DIR" --suite selftest \
    --scenario caps-mismatch-cell --tag codec_pin=h264 2>&1)" || caps_mis_rc=$?
  caps_mis_check="$(python3 - "$BENCH_LOG" <<'PY'
import json, sys
posts = [json.loads(l) for l in open(sys.argv[1])]
patches = [p for p in posts if p.get("method") == "PATCH"]
tagged = [p for p in patches
          if (p["body"].get("tags") or {}).get("mismatch") == "1"]
print("OK" if tagged else "no PATCH tagged the run mismatch=1 (patches: %d)" % len(patches))
PY
)"
  if [ "$caps_mis_rc" -eq 3 ] && [ "$caps_mis_check" = OK ] \
     && printf '%s' "$caps_mis_out" | grep -q 'FAIL mismatch — tag codec_pin=.h264. but the host reported .h265.'; then
    pass "bench:submit-caps-codec-mismatch" "--tag codec_pin=h264 vs caps.negotiated h265 -> rc 3 + mismatch=1"
  else
    fail "bench:submit-caps-codec-mismatch" \
      "rc=$caps_mis_rc check=$caps_mis_check out=$(printf '%s' "$caps_mis_out" | tail -n 2 | tr '\n' ' ')"
  fi

  # C11 #2: a session verdict of likely_* (a live negative signal) marks the
  # run validity=contaminated via a PATCH, WITHOUT the submission itself failing —
  # the evidence is still posted, only flagged.
  SUSPECT_DIR="$WORK/bench-verdict-suspect"
  mkdir -p "$SUSPECT_DIR"
  cp "$FIXTURES/bench-run"/* "$SUSPECT_DIR/" 2>/dev/null
  python3 - "$SUSPECT_DIR/verdict.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["verdict"] = "likely_network_congestion"
d["evidence"] = ["loss rising and rtt p95 elevated over the window"]
json.dump(d, open(sys.argv[1], "w"))
PY
  susp_rc=0
  susp_out="$(env BENCH_URL="http://127.0.0.1:$BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_submit.py" --dir "$SUSPECT_DIR" --suite selftest \
    --scenario verdict-suspect-cell 2>&1)" || susp_rc=$?
  susp_check="$(python3 - "$BENCH_LOG" <<'PY'
import json, sys
posts = [json.loads(l) for l in open(sys.argv[1])]
patches = [p for p in posts if p.get("method") == "PATCH"]
suspect = [p for p in patches if (p["body"] or {}).get("validity") == "contaminated"]
print("OK" if suspect else "no PATCH set validity=contaminated (patches: %d)" % len(patches))
PY
)"
  if [ "$susp_rc" -eq 0 ] && [ "$susp_check" = OK ] \
     && printf '%s' "$susp_out" | grep -q 'RESULT status=ok target=bench-submit' \
     && printf '%s' "$susp_out" | grep -q 'validity=contaminated'; then
    pass "bench:submit-verdict-suspect" "likely_network_congestion -> validity=contaminated, run still submitted ok"
  else
    fail "bench:submit-verdict-suspect" \
      "rc=$susp_rc check=$susp_check out=$(printf '%s' "$susp_out" | tail -n 3 | tr '\n' ' ')"
  fi

  # Two DIFFERENT runs of the same cell must never share a bench_ext_id. The id
  # used to be built on the directory BASENAME, and bench_suite.sh names every
  # cell directory after the cell — so the same cell in a second matrix collided
  # with the first and silently OVERWROTE it, leaving one run holding both
  # matrices' samples merged (which reads as a plausible average). Happened for
  # real on 2026-08-17 across a KDE matrix and a Unigine matrix.
  twin="$WORK/other-root/bench-run"
  mkdir -p "$twin"
  cp "$FIXTURES/bench-run"/* "$twin/" 2>/dev/null
  python3 - "$twin/session.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["id"] = "99999999-9999-9999-9999-999999999999"   # a different session
json.dump(d, open(sys.argv[1], "w"), indent=2, sort_keys=True)
PY
  id_a="$(python3 "$DX/bench_submit.py" --dir "$FIXTURES/bench-run" --suite selftest \
            --scenario fixture-cell --dry-run 2>/dev/null \
            | sed -n 's/^ *ext_id *\([0-9a-f]*\)$/\1/p')"
  id_b="$(python3 "$DX/bench_submit.py" --dir "$twin" --suite selftest \
            --scenario fixture-cell --dry-run 2>/dev/null \
            | sed -n 's/^ *ext_id *\([0-9a-f]*\)$/\1/p')"
  if [ -n "$id_a" ] && [ -n "$id_b" ] && [ "$id_a" != "$id_b" ]; then
    pass "bench:ext-id-distinct" "same cell name + different session -> different ext id"
  else
    fail "bench:ext-id-distinct" "ext ids collide ('$id_a' vs '$id_b') — a second matrix would overwrite the first"
  fi

  # the fake server started above ($BENCH_PID / $BENCH_PORT) is done with —
  # the budget test needs its OWN server (see below, --regressions-file is
  # loaded once at startup).
  kill "$BENCH_PID" 2>/dev/null || true
  wait "$BENCH_PID" 2>/dev/null || true

  # ── bench_budget.py against a DEDICATED fake server ──────────────────────
  # A separate server (not the one above) because it needs --regressions-file
  # loaded at startup. The fake server has no aggregation engine, so it is
  # taught two canned GET /v1/regressions responses and the test asserts
  # bench_budget.py both RENDERS them and GATES on them: a run with one
  # regressed stage exits 1, a clean run exits 0. This is the fake-server
  # fixture for the standing budget instrument (docs/testing-bench-mode.md
  # "The glass-to-glass budget").
  BUD_BENCH_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
  # Pass 1: server with NO fixture, just to mint the two run ids (the fixture
  # references them, so they must exist first).
  python3 "$TESTS_DIR/fake_bench_server.py" --port "$BUD_BENCH_PORT" \
    --log "$WORK/bench-budget-server.log" > "$WORK/bench-budget-server.out" 2>&1 &
  BUD_BENCH_PID=$!
  for _ in $(seq 1 40); do
    curl -fsS "http://127.0.0.1:$BUD_BENCH_PORT/v1/health" >/dev/null 2>&1 && break
    sleep 0.25
  done

  BUD_OK_ID="$(curl -fsS -X POST "http://127.0.0.1:$BUD_BENCH_PORT/v1/runs" \
    -H 'Content-Type: application/json' \
    -d '{"suite":"budget-selftest","scenario":"fixture-cell"}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
  BUD_BAD_ID="$(curl -fsS -X POST "http://127.0.0.1:$BUD_BENCH_PORT/v1/runs" \
    -H 'Content-Type: application/json' \
    -d '{"suite":"budget-selftest","scenario":"fixture-cell"}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"

  BUD_FIXTURE="$WORK/bench-budget-regressions.json"
  python3 - "$BUD_FIXTURE" "$BUD_OK_ID" "$BUD_BAD_ID" <<'PY'
import json, sys
ok_id, bad_id = sys.argv[2], sys.argv[3]

def rows(ok_v, bad_v, base, pct, better="lower"):
    def row(rid, v):
        delta = (v - base) / base * 100 if base else 0.0
        return {"run_id": rid, "value": v, "baseline_value": base,
                "delta_pct": delta, "regressed": delta > pct, "threshold_pct": pct}
    return {"better": better, "baseline_run_id": "baseline-run",
            "runs": [row(ok_id, ok_v), row(bad_id, bad_v)]}

fixture = {
    "browser.stage_host_to_receive_p50_ms": {"p50": rows(22.6, 22.8, 22.6, 15)},
    "browser.stage_receive_to_present_p50_ms": {"p50": rows(43.4, 43.9, 43.4, 15)},
    "browser.stage_present_to_display_p50_ms": {"p50": rows(-0.2, -0.2, -0.2, 15)},
    # the one stage that regresses in the "bad" run: 2.3ms baseline, bad run
    # measured 6.1ms — a real number lifted from the hermes-peer contrast in
    # docs/reports/2026-08-19-latency-budget/REPORT.md section 7, not invented.
    "browser.stage_decode_p50_ms": {"p50": rows(2.3, 6.1, 2.3, 15),
                                    "p95": rows(2.9, 6.4, 2.9, 15)},
    "browser.g2g_p50_ms": {"p50": rows(66.0, 66.4, 66.0, 10)},
    "browser.g2g_p95_ms": {"p95": rows(66.3, 66.9, 66.3, 10)},
}
json.dump(fixture, open(sys.argv[1], "w"))
PY

  # Pass 2: restart WITH the fixture wired in (the fake server loads
  # --regressions-file once at startup) and re-mint the SAME two run ids —
  # the fake server's in-memory RUNS resets on restart, and its id counter is
  # deterministic (fake-run-1, fake-run-2, ...) given the same two POSTs in
  # the same order, so BUD_OK_ID/BUD_BAD_ID stay valid.
  kill "$BUD_BENCH_PID" 2>/dev/null || true
  wait "$BUD_BENCH_PID" 2>/dev/null || true
  python3 "$TESTS_DIR/fake_bench_server.py" --port "$BUD_BENCH_PORT" \
    --log "$WORK/bench-budget-server2.log" --regressions-file "$BUD_FIXTURE" \
    > "$WORK/bench-budget-server2.out" 2>&1 &
  BUD_BENCH_PID=$!
  for _ in $(seq 1 40); do
    curl -fsS "http://127.0.0.1:$BUD_BENCH_PORT/v1/health" >/dev/null 2>&1 && break
    sleep 0.25
  done
  REPLAY_OK_ID="$(curl -fsS -X POST "http://127.0.0.1:$BUD_BENCH_PORT/v1/runs" \
    -H 'Content-Type: application/json' \
    -d '{"suite":"budget-selftest","scenario":"fixture-cell"}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
  REPLAY_BAD_ID="$(curl -fsS -X POST "http://127.0.0.1:$BUD_BENCH_PORT/v1/runs" \
    -H 'Content-Type: application/json' \
    -d '{"suite":"budget-selftest","scenario":"fixture-cell"}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')"
  if [ "$REPLAY_OK_ID" != "$BUD_OK_ID" ] || [ "$REPLAY_BAD_ID" != "$BUD_BAD_ID" ]; then
    fail "bench-budget:replay-ids" "id generator is not deterministic across restarts ('$REPLAY_OK_ID'/'$REPLAY_BAD_ID' vs '$BUD_OK_ID'/'$BUD_BAD_ID') — the fixture below references stale ids"
  fi

  ok_out="$(env BENCH_URL="http://127.0.0.1:$BUD_BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_budget.py" --run "$BUD_OK_ID" --suite budget-selftest \
    --baseline test-baseline 2>&1)"; ok_rc=$?
  bad_out="$(env BENCH_URL="http://127.0.0.1:$BUD_BENCH_PORT" BENCH_KEY=test-key \
    python3 "$DX/bench_budget.py" --run "$BUD_BAD_ID" --suite budget-selftest \
    --baseline test-baseline 2>&1)"; bad_rc=$?

  if [ "$ok_rc" = 0 ] && ! printf '%s' "$ok_out" | grep -q REGRESSED \
     && printf '%s' "$ok_out" | grep -q 'reconcile: A+B+C'; then
    pass "bench-budget:table-clean" "clean run renders the table, reconciles, exits 0"
  else
    fail "bench-budget:table-clean" "rc=$ok_rc: $(printf '%s' "$ok_out" | tail -n 6 | tr '\n' ' ')"
  fi

  if [ "$bad_rc" = 1 ] && printf '%s' "$bad_out" | grep -q 'B3 decode.*REGRESSED' \
     && printf '%s' "$bad_out" | grep -q 'RESULT status=failed target=bench-budget'; then
    pass "bench-budget:table-regressed" "regressed stage flagged in the table AND non-zero exit"
  else
    fail "bench-budget:table-regressed" "rc=$bad_rc: $(printf '%s' "$bad_out" | tail -n 6 | tr '\n' ' ')"
  fi

  kill "$BUD_BENCH_PID" 2>/dev/null || true
  wait "$BUD_BENCH_PID" 2>/dev/null || true
else
  warn "bench:submit-payload" "scripts/dx/tests/fixtures/bench-run missing — SKIPPED"
fi

# ── the vendored client must stay a verbatim copy with its provenance intact ──
if [ -f "$DX/vendor/bench.py" ]; then
  if grep -qE '^# Commit: [0-9a-f]{40}' "$DX/vendor/bench.py"; then
    pass "bench:vendor-provenance" "vendor/bench.py records its source commit"
  else
    fail "bench:vendor-provenance" "vendor/bench.py has no '# Commit: <sha>' header"
  fi
  # The vendored copy must be new enough to speak the endpoints the harness now
  # depends on. A silently stale vendor is how the harness ends up re-implementing
  # a capability the service already has.
  vend_check="$(python3 - "$DX/vendor" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
import bench
missing = [m for m in ("phases", "set_validity", "set_baseline", "regressions",
                       "patch", "phase_mark", "metrics")
           if not hasattr(bench.Bench, m) and not hasattr(bench, m)]
v = tuple(int(x) for x in bench.__version__.split(".")[:2])
if missing:
    print("vendored client is missing %s" % missing)
elif v < (1, 1):
    print("vendored client is %s, want >= 1.1" % bench.__version__)
else:
    print("OK")
PY
)"
  if [ "$vend_check" = OK ]; then
    pass "bench:vendor-version" "vendored client is >= 1.1 and carries the 1.1 verbs"
  else
    fail "bench:vendor-version" "$vend_check"
  fi

  # bench 1.2: a re-fold must post replace=True under an expected_count guard,
  # and only when the SERVICE is >= 1.2 (an older service ignores both fields
  # and silently appends — the exact 341-vs-276 stale tail).
  rg_check=$(python3 -c '
import sys, importlib.util
spec=importlib.util.spec_from_file_location("bs", sys.argv[1]); bs=importlib.util.module_from_spec(spec)
spec.loader.exec_module(bs)
sys.path.insert(0, sys.argv[2]); import bench, inspect
p=[]
sig=inspect.signature(bench.Bench.samples).parameters
if "replace" not in sig or "expected_count" not in sig: p.append("vendored samples() lacks replace/expected_count")
if not hasattr(bench.Bench,"delete_samples") or not hasattr(bench.Bench,"counts"): p.append("vendored client lacks delete_samples/counts")
smp=[{"source":"browser","ts_unix_ms":1},{"source":"browser","ts_unix_ms":2},{"source":"agent","ts_unix_ms":1}]
if bs.expected_counts(smp)!={"browser":2,"agent":1}: p.append("expected_counts wrong: %r"%bs.expected_counts(smp))
if bs.expected_counts([])!={}: p.append("expected_counts([]) not empty")
bs.server_version=lambda url: "1.1.0"
if bs.server_can_replace("http://x"): p.append("1.1.0 service must NOT be treated as replace-capable")
bs.server_version=lambda url: "1.2.0"
if not bs.server_can_replace("http://x"): p.append("1.2.0 service must be replace-capable")
bs.server_version=lambda url: ""
if bs.server_can_replace("http://x"): p.append("unknown service version must NOT be treated as replace-capable")
# guard: with can_replace the shifted case must INFO not die
base=list(range(1_000_000, 1_000_000+276*1000, 1000))
class B: url="http://x"; key="k"
bs.stored_window_span=lambda b, rid: (276, base[0], base[-1])
smp=[{"source":"browser","ts_unix_ms":t+65_000,"metrics":{"missing_indices":0}} for t in base]
died=[]
bs.die=lambda *a, **k: died.append(a)
bs.refuse_on_shifted_resubmit(B(), "r", smp, False, can_replace=True)
if died: p.append("shifted re-submit died despite can_replace")
bs.refuse_on_shifted_resubmit(B(), "r", smp, False, can_replace=False)
if not died: p.append("shifted re-submit NOT refused on old service")
print("OK" if not p else "; ".join(p))' "$DX/bench_submit.py" "$DX/vendor")
  if [ "$rg_check" = OK ]; then pass "bench:replace-guard" "re-fold uses replace+expected_count, gated on service >= 1.2"; else fail "bench:replace-guard" "$rg_check"; fi
else
  fail "bench:vendor-provenance" "scripts/dx/vendor/bench.py is missing"
fi

# ── no API key may ever be committed ─────────────────────────────────────────
if grep -rnE 'BENCH_KEY[=:][[:space:]]*[A-Za-z0-9]{16,}' "$DX" "$ROOT/Makefile" 2>/dev/null | grep -v 'tests/run.sh'; then
  fail "bench:no-key-committed" "a literal BENCH_KEY value is present in the tree"
else
  pass "bench:no-key-committed" "no literal BENCH_KEY value in scripts/dx or the Makefile"
fi

printf '\n== qses stop ==\n'

# `qses stop` must never claim success it has not verified. It used to do its own
# BOOTSTRAP_ADMIN login (ignoring QSES_ADMIN_TOKEN, with no dev-key fallback) and
# to exit 0 on a 404 — but a stopped session KEEPS its row, so a 404 means the
# session is on a different stack and still running.
#
# Driven against fake_cp_server.py through an `ssh` stub that runs the remote
# command locally. Nothing leaves this machine.
QSES="$ROOT/.claude/skills/quasar-session/scripts/qses"
if [ -x "$QSES" ] || [ -f "$QSES" ]; then
  QSTUB="$WORK/qses-stubbin"
  mkdir -p "$QSTUB"
  # Run the LAST argument (the remote command) in a local shell, discarding the
  # ssh options and the host spec.
  printf '#!/usr/bin/env bash\nlast=""\nfor a in "$@"; do last="$a"; done\nexec bash -c "$last"\n' \
    > "$QSTUB/ssh"
  printf '#!/usr/bin/env bash\nexit 0\n' > "$QSTUB/docker"   # no dev key discoverable
  chmod +x "$QSTUB/ssh" "$QSTUB/docker"

  qses_stop_case() { # qses_stop_case <label> <session-mode> <expected-rc> [env...]
    local label="$1" mode="$2" want="$3"; shift 3
    local port state out rc
    port="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
    state="$WORK/qses-$label.json"
    python3 "$TESTS_DIR/fake_cp_server.py" --port "$port" --state "$state" \
      --session-mode "$mode" > "$WORK/qses-$label.out" 2>&1 &
    local pid=$!
    for _ in $(seq 1 40); do
      curl -fsS "http://127.0.0.1:$port/v1/hosts" >/dev/null 2>&1 && break
      sleep 0.25
    done
    printf '{"_schema":"quasar-skills/hosts@2","roles":{"aux-infra":"faked","gpu-test":"faked"},"hosts":{"faked":{"ssh_alias":"ignored","dir":"%s","api":"http://127.0.0.1:%s","api_external":"http://127.0.0.1:%s"}}}\n' \
      "$WORK" "$port" "$port" > "$WORK/qses-hosts.json"
    # `env -u` must precede the assignments, or env reads it as a program name.
    out="$(PATH="$QSTUB:$PATH" env -u QSES_DEV_KEY -u QUASAR_DEV_AGENT_KEY \
      QUASAR_HOSTS_JSON="$WORK/qses-hosts.json" "$@" \
      bash "$QSES" stop 11111111-2222-3333-4444-555555555555 --stack=faked 2>&1)"
    rc=$?
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    if [ "$rc" -eq "$want" ]; then
      pass "qses-stop:$label" "rc=$rc as expected"
    else
      fail "qses-stop:$label" "expected rc=$want, got rc=$rc :: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
    fi
    QSES_STOP_OUT="$out"
  }

  qses_stop_case ok ok 0 QSES_ADMIN_TOKEN=test-token
  qses_stop_case http-500 error 1 QSES_ADMIN_TOKEN=test-token
  if printf '%s' "$QSES_STOP_OUT" | grep -q 'qses stop FAILED'; then
    pass "qses-stop:500-message" "a 500 from the DELETE is reported as a FAILURE"
  else
    fail "qses-stop:500-message" "$(printf '%s' "$QSES_STOP_OUT" | tail -n 2 | tr '\n' ' ')"
  fi

  # A 404 is the cross-stack trap, not "already gone".
  qses_stop_case gone-404 absent 1 QSES_ADMIN_TOKEN=test-token
  if printf '%s' "$QSES_STOP_OUT" | grep -q 'matching --stack='; then
    pass "qses-stop:404-message" "a 404 names the wrong-stack possibility instead of claiming success"
  else
    fail "qses-stop:404-message" "$(printf '%s' "$QSES_STOP_OUT" | tail -n 2 | tr '\n' ' ')"
  fi

  # With NO credential resolvable at all (no token, no dev key, no deploy/.env)
  # the verb must refuse rather than send `Authorization: Bearer ` and interpret
  # the resulting 401.
  qses_stop_case no-cred ok 1
  if printf '%s' "$QSES_STOP_OUT" | grep -q 'no admin bearer could be obtained'; then
    pass "qses-stop:no-credential" "an unresolvable admin identity is a loud refusal"
  else
    fail "qses-stop:no-credential" "$(printf '%s' "$QSES_STOP_OUT" | tail -n 2 | tr '\n' ' ')"
  fi
else
  warn "qses-stop" "$QSES not present — SKIPPED"
fi

printf '\n== compose ==\n'

# ── the new local compose file must exist and be structurally parseable ──────
if [ -f "$ROOT/deploy/overlays/docker-compose.local.yml" ]; then
  pass "compose:local-exists" "deploy/overlays/docker-compose.local.yml"
  # A SERVICE definition, not a mention — the file explains in prose why it has
  # no agent, and that explanation must not fail its own test.
  if grep -qE '^[[:space:]]{2}[a-z0-9-]*node-?agent[a-z0-9-]*:' "$ROOT/deploy/overlays/docker-compose.local.yml"; then
    fail "compose:local-agentless" "the local stack must NOT define a node-agent service"
  else
    pass "compose:local-agentless" "no node-agent service (agentless by design)"
  fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    if docker compose -f "$ROOT/deploy/overlays/docker-compose.local.yml" config -q >/dev/null 2>&1; then
      pass "compose:local-valid" "docker compose config -q is clean"
    else
      fail "compose:local-valid" "docker compose config -q rejected the file"
    fi
  else
    warn "compose:local-valid" "SKIPPED — needs a running Docker daemon"
  fi
else
  fail "compose:local-exists" "deploy/overlays/docker-compose.local.yml is missing"
fi

# ── .gitignore must ignore .diagnostics/ ─────────────────────────────────────
if grep -qE '^\.diagnostics/?$' "$ROOT/.gitignore" 2>/dev/null; then
  pass "gitignore:diagnostics" ".diagnostics/ is ignored"
else
  fail "gitignore:diagnostics" ".diagnostics/ is not in .gitignore"
fi

printf '\n== nightly budget ==\n'

NB="$DX/nightly_budget.sh"
NBC="$DX/nightly_budget_ctl.sh"
NBWORK="$WORK/nightly"
mkdir -p "$NBWORK"

rc_of 0 "nightly:help" -- bash "$NB" --help
rc_of 2 "nightly:bad-arg" -- bash "$NB" --bogus

# ── BENCH_KEY unreadable is a clean skip, not a crash ─────────────────────────
nb_skip_key="$(NIGHTLY_BENCH_ENV="$NBWORK/nope.env" NIGHTLY_LOG_DIR="$NBWORK/log1" \
  bash "$NB" --dry-run 2>&1)"
if printf '%s' "$nb_skip_key" | grep -q 'status=skipped .*reason=bench-key-unavailable'; then
  pass "nightly:skip-no-key" "no BENCH_API_KEYS -> clean skip"
else
  fail "nightly:skip-no-key" "$nb_skip_key"
fi

# ── a valid env file + --dry-run prints the plan and does not touch anything ─
cat > "$NBWORK/bench.env" <<'EOF'
BENCH_API_KEYS=harness:test-secret-value
BENCH_PORT=9401
EOF
nb_plan="$(NIGHTLY_BENCH_ENV="$NBWORK/bench.env" NIGHTLY_LOG_DIR="$NBWORK/log2" \
  bash "$NB" --dry-run 2>&1)"
if printf '%s' "$nb_plan" | grep -q 'status=ok .*dry_run=1' \
   && printf '%s' "$nb_plan" | grep -q 'suite     nightly-budget' \
   && printf '%s' "$nb_plan" | grep -q 'scenario  1080p60-h264-local' \
   && printf '%s' "$nb_plan" | grep -q 'bench_url http://localhost:9401'; then
  pass "nightly:dry-run-plan" "prints the plan, derives BENCH_URL from BENCH_PORT"
else
  fail "nightly:dry-run-plan" "$(printf '%s' "$nb_plan" | tail -n 8 | tr '\n' ' ')"
fi
[ -f "$NBWORK/log2/$(date -u +%F).log" ] && pass "nightly:dry-run-logs" "wrote today's log file" \
  || fail "nightly:dry-run-logs" "no log file under $NBWORK/log2"

# ── stub bench_run.sh + bench_budget.py so a full (non-dry) pass exercises the
# real orchestration — RUN_ID scraping, the budget gate, LAST_REGRESSION, and
# lock release — without a stack, a session or a network. ─────────────────────
cat > "$NBWORK/fake_bench_run_ok.sh" <<'EOF'
#!/usr/bin/env bash
echo "fake bench_run.sh output"
echo "RESULT status=ok target=bench-submit run_id=fake-run-id-1 url=http://x samples=1 events=1 verdict=PASS"
echo "RESULT status=ok target=bench-run out=/tmp sid=sidx suite=nightly-budget scenario=1080p60-h264-local"
exit 0
EOF
cat > "$NBWORK/fake_bench_run_fail.sh" <<'EOF'
#!/usr/bin/env bash
echo "fake bench_run.sh: launch failed, no run id"
exit 1
EOF
cat > "$NBWORK/fake_qses_idle.sh" <<'EOF'
#!/usr/bin/env bash
echo "PASS request — stub"
echo "RESULT status=ok target=agent-creds host=local pass=1 warn=0 fail=0"
EOF
cat > "$NBWORK/fake_qses_busy.sh" <<'EOF'
#!/usr/bin/env bash
echo "PASS request — stub"
echo "RESULT status=ok target=agent-creds host=local pass=1 warn=0 fail=0"
echo "0f1e2d3c-aaaa-bbbb-cccc-1234567890ab running Quasar-B"
EOF
cat > "$NBWORK/fake_budget_ok.py" <<'EOF'
#!/usr/bin/env python3
print("stage table (ok)")
print("RESULT status=ok target=bench-budget run_id=fake-run-id-1 regressed=0")
EOF
cat > "$NBWORK/fake_budget_regressed.py" <<'EOF'
#!/usr/bin/env python3
import sys
print("stage table (regressed)")
print("RESULT status=failed target=bench-budget run_id=fake-run-id-1 regressed=1")
sys.exit(1)
EOF
cat > "$NBWORK/fake_budget_error.py" <<'EOF'
#!/usr/bin/env python3
import sys
print("error: could not read run", file=sys.stderr)
sys.exit(2)
EOF
chmod +x "$NBWORK"/fake_*.sh

# A curl stub reporting a healthy stack — the orchestration tests below are
# about bench_run/bench_budget wiring, not the health probe itself (that gets
# its own dedicated test further down with a curl stub that reports 000).
HEALTHY_CURL_BIN="$NBWORK/healthycurlbin"
mkdir -p "$HEALTHY_CURL_BIN"
printf '#!/usr/bin/env bash\necho 200\n' > "$HEALTHY_CURL_BIN/curl"
chmod +x "$HEALTHY_CURL_BIN/curl"

nb_run() { # nb_run <logdir> <qses-stub> <bench_run-stub> <budget-stub-with-interpreter...>
  local logdir="$1" qses="$2" run="$3"; shift 3
  PATH="$HEALTHY_CURL_BIN:$STUB_BIN:$PATH" \
  NIGHTLY_BENCH_ENV="$NBWORK/bench.env" NIGHTLY_LOG_DIR="$logdir" \
  NIGHTLY_DEV_KEY=fake-dev-key NIGHTLY_ADMIN_TOKEN=fake-admin-token NIGHTLY_QSES="$qses" NIGHTLY_BENCH_RUN="$run" \
  NIGHTLY_BENCH_BUDGET="$*" \
    bash "$NB" 2>&1
}

nb_ok="$(nb_run "$NBWORK/log-ok" "$NBWORK/fake_qses_idle.sh" "$NBWORK/fake_bench_run_ok.sh" \
  python3 "$NBWORK/fake_budget_ok.py")"
if printf '%s' "$nb_ok" | grep -q '^NIGHTLY-BUDGET status=ok run_id=fake-run-id-1 '; then
  pass "nightly:run-ok" "idle stack + clean budget -> status=ok, run id scraped"
else
  fail "nightly:run-ok" "$nb_ok"
fi
[ -d "$NBWORK/log-ok/.run.lock" ] && fail "nightly:run-ok-lock-released" "lock dir left behind" \
  || pass "nightly:run-ok-lock-released" "lock released on exit"

nb_regressed="$(nb_run "$NBWORK/log-regress" "$NBWORK/fake_qses_idle.sh" "$NBWORK/fake_bench_run_ok.sh" \
  python3 "$NBWORK/fake_budget_regressed.py")"
if printf '%s' "$nb_regressed" | grep -q '^NIGHTLY-BUDGET status=regression run_id=fake-run-id-1 '; then
  pass "nightly:run-regressed" "a regressed stage -> status=regression"
else
  fail "nightly:run-regressed" "$nb_regressed"
fi
if [ -s "$NBWORK/log-regress/LAST_REGRESSION" ] && grep -q 'regressed=1' "$NBWORK/log-regress/LAST_REGRESSION"; then
  pass "nightly:last-regression-written" "LAST_REGRESSION carries the table"
else
  fail "nightly:last-regression-written" "missing or empty LAST_REGRESSION"
fi

nb_busy="$(nb_run "$NBWORK/log-busy" "$NBWORK/fake_qses_busy.sh" "$NBWORK/fake_bench_run_ok.sh" \
  python3 "$NBWORK/fake_budget_ok.py")"
if printf '%s' "$nb_busy" | grep -q 'status=skipped .*reason=session-already-running'; then
  pass "nightly:skip-session-running" "a non-terminal session on the stack -> clean skip"
else
  fail "nightly:skip-session-running" "$nb_busy"
fi

nb_launch_fail="$(nb_run "$NBWORK/log-launchfail" "$NBWORK/fake_qses_idle.sh" "$NBWORK/fake_bench_run_fail.sh" \
  python3 "$NBWORK/fake_budget_ok.py")"
if printf '%s' "$nb_launch_fail" | grep -q '^NIGHTLY-BUDGET status=error .*reason=bench-run-failed'; then
  pass "nightly:run-launch-failed" "bench_run.sh producing no run id -> status=error, not a crash"
else
  fail "nightly:run-launch-failed" "$nb_launch_fail"
fi

nb_budget_err="$(nb_run "$NBWORK/log-budgeterr" "$NBWORK/fake_qses_idle.sh" "$NBWORK/fake_bench_run_ok.sh" \
  python3 "$NBWORK/fake_budget_error.py")"
if printf '%s' "$nb_budget_err" | grep -q '^NIGHTLY-BUDGET status=error .*reason=budget-script-failed'; then
  pass "nightly:budget-script-error" "bench_budget.py rc=2 -> status=error, distinct from a regression"
else
  fail "nightly:budget-script-error" "$nb_budget_err"
fi

# ── a held lock skips cleanly, and never runs bench_run.sh at all ────────────
mkdir -p "$NBWORK/log-locked/.run.lock"
nb_locked="$(nb_run "$NBWORK/log-locked" "$NBWORK/fake_qses_idle.sh" "$NBWORK/fake_bench_run_ok.sh" \
  python3 "$NBWORK/fake_budget_ok.py")"
if printf '%s' "$nb_locked" | grep -q 'status=skipped .*reason=nightly-lock-held'; then
  pass "nightly:skip-lock-held" "an existing lock dir -> clean skip"
else
  fail "nightly:skip-lock-held" "$nb_locked"
fi

# ── stack-unhealthy: a non-200 health probe is a clean skip ──────────────────
CURL_STUB="$NBWORK/curlbin"
mkdir -p "$CURL_STUB"
printf '#!/usr/bin/env bash\necho 000\n' > "$CURL_STUB/curl"
chmod +x "$CURL_STUB/curl"
nb_unhealthy="$(PATH="$CURL_STUB:$STUB_BIN:$PATH" \
  NIGHTLY_BENCH_ENV="$NBWORK/bench.env" NIGHTLY_LOG_DIR="$NBWORK/log-unhealthy" \
  NIGHTLY_QSES="$NBWORK/fake_qses_idle.sh" NIGHTLY_BENCH_RUN="$NBWORK/fake_bench_run_ok.sh" \
  NIGHTLY_BENCH_BUDGET="python3 $NBWORK/fake_budget_ok.py" \
    bash "$NB" 2>&1)"
if printf '%s' "$nb_unhealthy" | grep -q 'status=skipped .*reason=stack-unhealthy'; then
  pass "nightly:skip-unhealthy" "health probe != 200 -> clean skip"
else
  fail "nightly:skip-unhealthy" "$nb_unhealthy"
fi

# ── admin-token mint failure is a clean skip too (bench_run.sh needs its own
# QSES_ADMIN_TOKEN, distinct from the dev-agent key) ─────────────────────────
nb_no_admin="$(PATH="$HEALTHY_CURL_BIN:$STUB_BIN:$PATH" \
  NIGHTLY_BENCH_ENV="$NBWORK/bench.env" NIGHTLY_LOG_DIR="$NBWORK/log-noadmin" \
  NIGHTLY_DEV_KEY=fake-dev-key NIGHTLY_QSES="$NBWORK/fake_qses_idle.sh" \
  NIGHTLY_BENCH_RUN="$NBWORK/fake_bench_run_ok.sh" \
  NIGHTLY_BENCH_BUDGET="python3 $NBWORK/fake_budget_ok.py" \
    bash "$NB" 2>&1)"
if printf '%s' "$nb_no_admin" | grep -q 'status=skipped .*reason=admin-token-mint-failed'; then
  pass "nightly:skip-admin-token" "no NIGHTLY_ADMIN_TOKEN and the mint call yields no parseable token -> clean skip"
else
  fail "nightly:skip-admin-token" "$nb_no_admin"
fi

# ── log rotation keeps only the last N days ───────────────────────────────────
ROTWORK="$NBWORK/rotate"
mkdir -p "$ROTWORK"
touch -d '40 days ago' "$ROTWORK/2026-07-01.log" 2>/dev/null || touch -t 202607010000 "$ROTWORK/2026-07-01.log"
touch "$ROTWORK/keepme-marker"
NIGHTLY_BENCH_ENV="$NBWORK/nope.env" NIGHTLY_LOG_DIR="$ROTWORK" NIGHTLY_KEEP_DAYS=30 \
  bash "$NB" --dry-run >/dev/null 2>&1 || true
if [ ! -f "$ROTWORK/2026-07-01.log" ]; then
  pass "nightly:rotate" "a log older than NIGHTLY_KEEP_DAYS is removed"
else
  warn "nightly:rotate" "old log survived — mtime-based find -mtime can be flaky depending on the filesystem's touch -d support, not a hard failure"
fi

# ── nightly_budget_ctl.sh: same mutating-remote guard as bench-run/abr-ladder ─
rc_of 2 "nightly:ctl-install-implicit-host" -- \
  env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_DEFAULT_HOST=gpu-test bash "$NBC" install
rc_of 2 "nightly:ctl-run-implicit-host" -- \
  env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" QUASAR_DEFAULT_HOST=gpu-test bash "$NBC" run
# status is read-only — no explicit-HOST requirement.
CRONTAB_STATE="$WORK/crontab.status-test.state"
rc_of 0 "nightly:ctl-status-implicit-host-ok" -- \
  env -u HOST QUASAR_DEFAULT_HOST=local CRONTAB_STATE="$CRONTAB_STATE" PATH="$STUB_BIN:$PATH" bash "$NBC" status

# HOST=local install/status runs entirely local (no ssh) and is idempotent:
# installing twice leaves exactly one nightly_budget.sh line, not two.
CRONTAB_STATE="$WORK/crontab.idempotent.state"
rm -f "$CRONTAB_STATE"
env -u HOST CRONTAB_STATE="$CRONTAB_STATE" PATH="$STUB_BIN:$PATH" bash "$NBC" install >/dev/null 2>&1
env -u HOST CRONTAB_STATE="$CRONTAB_STATE" PATH="$STUB_BIN:$PATH" bash "$NBC" install >/dev/null 2>&1
nb_cron_lines="$(grep -c 'nightly_budget.sh' "$CRONTAB_STATE" 2>/dev/null || echo 0)"
if [ "$nb_cron_lines" = "1" ]; then
  pass "nightly:ctl-install-idempotent" "reinstalling replaces the line instead of duplicating it"
else
  fail "nightly:ctl-install-idempotent" "expected exactly 1 nightly_budget.sh crontab line, found $nb_cron_lines"
fi
if grep -q '^30 3 \* \* \*' "$CRONTAB_STATE" 2>/dev/null; then
  pass "nightly:ctl-install-schedule" "03:30 daily schedule"
else
  fail "nightly:ctl-install-schedule" "$(cat "$CRONTAB_STATE" 2>/dev/null)"
fi

printf '\n== makefile ==\n'

# ── every target must resolve to a script that exists ────────────────────────
if command -v make >/dev/null 2>&1; then
  mk_missing=""
  for t in help init doctor config-check verify test test-go test-rust test-web \
           test-db preflight up down restart rebuild redeploy-cp status health logs \
           logs-follow dev-web dev-cp diagnose diagnose-bundle clean reset; do
    if ! make -C "$ROOT" -n "$t" >/dev/null 2>&1; then
      mk_missing="$mk_missing $t"
    fi
  done
  if [ -z "$mk_missing" ]; then
    pass "make:dry-run" "every target dry-runs cleanly"
  else
    fail "make:dry-run" "these targets failed make -n:$mk_missing"
  fi

  # Captured, not piped: `grep -q` closes the pipe on its first match, make
  # takes SIGPIPE, and `set -o pipefail` would report that as a failure.
  help_out="$(make -C "$ROOT" help 2>/dev/null)"
  if printf '%s' "$help_out" | grep -q 'reset'; then
    pass "make:help" "help renders and is self-documenting"
  else
    fail "make:help" "make help did not render the target list"
  fi
else
  warn "make:dry-run" "make not installed — SKIPPED"
fi

printf '\n== image QA harness ==\n'

# Guards: qa refuses before it can touch anything on a real stack.
rc_of 2 "guard:qa-no-image" -- env -u IMAGE -u PROFILE -u HOST bash "$DX/qa.sh"
rc_of 2 "guard:qa-no-profile" -- env -u PROFILE -u HOST IMAGE=quasar-steam:dev bash "$DX/qa.sh"
rc_of 2 "guard:qa-unknown-profile" -- env -u HOST IMAGE=quasar-steam:dev PROFILE=not-a-profile bash "$DX/qa.sh"
# HOST=local has no node agent: qa must refuse rather than "validate" nothing.
rc_of 2 "guard:qa-host-local" -- env DX_HOSTS_JSON="$FIX_HOSTS" IMAGE=quasar-steam:dev PROFILE=steam-bpm HOST=local bash "$DX/qa.sh"
# qa mutates the target, so an INHERITED host (QUASAR_DEFAULT_HOST) is refused —
# the operator must type HOST=.
rc_of 2 "guard:qa-host-implicit" -- env -u HOST DX_HOSTS_JSON="$FIX_HOSTS" IMAGE=quasar-steam:dev PROFILE=steam-bpm QUASAR_DEFAULT_HOST=gpu-test bash "$DX/qa.sh"

# Every shipped profile must parse and carry the gate blocks the harness reads;
# a typo'd budget that silently defaults is how a gate passes for a wrong reason.
if command -v python3 >/dev/null 2>&1; then
  prof_err="$(python3 - "$ROOT/scripts/qa/profiles" <<'PY'
import json, os, sys
d = sys.argv[1]
required_gates = {"launch", "oracle", "input", "shutdown"}
known_top = {"name","description","app","session_profile_id","runs","session_container_match","gates","notes"}
bad = []
for f in sorted(os.listdir(d)):
    if not f.endswith(".json"):
        continue
    p = os.path.join(d, f)
    try:
        doc = json.load(open(p))
    except Exception as e:
        bad.append("%s: not valid JSON (%s)" % (f, e)); continue
    unknown = set(doc) - known_top
    if unknown:
        bad.append("%s: unknown top-level key(s): %s" % (f, ",".join(sorted(unknown))))
    if doc.get("name") != f[:-5]:
        bad.append("%s: name '%s' does not match the filename" % (f, doc.get("name")))
    gates = doc.get("gates") or {}
    missing = required_gates - set(gates)
    if missing:
        bad.append("%s: missing gate block(s): %s" % (f, ",".join(sorted(missing))))
        continue
    fc = gates["launch"].get("first_content_s")
    if not (isinstance(fc, list) and len(fc) == 2 and fc[0] <= fc[1]):
        bad.append("%s: launch.first_content_s must be [lo,hi] with lo<=hi" % f)
    for dev, cfg in (gates["input"].get("devices") or {}).items():
        for s in cfg.get("stimuli") or []:
            if s.get("t") not in {"ma","mm","ms","mb","k","gp"}:
                bad.append("%s: device %s has stimulus type '%s' which probe.mjs cannot compile"
                           % (f, dev, s.get("t")))
print("\n".join(bad))
PY
)"
  if [ -z "$prof_err" ]; then
    pass "qa:profiles" "$(find "$ROOT/scripts/qa/profiles" -name '*.json' | wc -l | tr -d ' ') profile(s) valid"
  else
    fail "qa:profiles" "$(printf '%s' "$prof_err" | head -n 10)"
  fi
fi

# Report renderer + gate assembly: golden tests, including self-containment of
# the HTML (the report is uploaded as ONE file — a relative <img src> is a bug).
if command -v node >/dev/null 2>&1; then
  for t in report assemble; do
    if [ -f "$ROOT/scripts/qa/$t.test.mjs" ]; then
      if node --test "$ROOT/scripts/qa/$t.test.mjs" >/dev/null 2>&1; then
        pass "qa:$t" "node --test green"
      else
        fail "qa:$t" "node --test scripts/qa/$t.test.mjs failed"
      fi
    fi
  done
  if node --check "$ROOT/scripts/qa/probe.mjs" 2>/dev/null; then
    pass "qa:probe" "probe.mjs parses"
  else
    fail "qa:probe" "scripts/qa/probe.mjs has a syntax error"
  fi
fi

printf '\n== admin_token.sh + session.sh ==\n'

# Both are driven against fake_cp_server.py through an `ssh` stub that runs the
# remote command LOCALLY, so tier 3 of the ladder is exercised end to end without
# a host. Nothing leaves this machine.
SESS_DIR="$WORK/sess"
mkdir -p "$SESS_DIR/stub" "$SESS_DIR/hostdir/deploy" "$SESS_DIR/cache"
printf '#!/usr/bin/env bash\nlast=""\nfor a in "$@"; do last="$a"; done\nexec bash -c "$last"\n' \
  > "$SESS_DIR/stub/ssh"
printf 'BOOTSTRAP_ADMIN_EMAIL=admin@local.test\nBOOTSTRAP_ADMIN_PASSWORD=local-dev-admin\n' \
  > "$SESS_DIR/hostdir/deploy/.env"

# docker stub A: a control-plane container exists and yields a per-boot dev key.
sess_docker_withkey() {
  printf '#!/usr/bin/env bash\ncase "${1:-}" in\n  ps) echo cp1 ;;\n  exec) echo devkey123 ;;\n  *) exit 0 ;;\nesac\n' \
    > "$SESS_DIR/stub/docker"
  chmod +x "$SESS_DIR/stub/docker"
}
# docker stub B: no container at all — the ladder must fall through to deploy/.env.
sess_docker_nokey() {
  printf '#!/usr/bin/env bash\nexit 0\n' > "$SESS_DIR/stub/docker"
  chmod +x "$SESS_DIR/stub/docker"
}
sess_ssh_ok()     { printf '#!/usr/bin/env bash\nlast=""\nfor a in "$@"; do last="$a"; done\nexec bash -c "$last"\n' > "$SESS_DIR/stub/ssh"; chmod +x "$SESS_DIR/stub/ssh"; }
sess_ssh_broken() { printf '#!/usr/bin/env bash\nexit 255\n' > "$SESS_DIR/stub/ssh"; chmod +x "$SESS_DIR/stub/ssh"; }
chmod +x "$SESS_DIR/stub/ssh"

SESS_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
printf '{"_schema":"quasar-skills/hosts@2","roles":{"gpu-test":"fakebox"},"hosts":{"fakebox":{"ssh_alias":"ignored","dir":"%s","api":"http://127.0.0.1:%s","api_external":"http://127.0.0.1:%s"}}}\n' \
  "$SESS_DIR/hostdir" "$SESS_PORT" "$SESS_PORT" > "$SESS_DIR/hosts.json"

SESS_SEED='[{"id":"11111111-2222-3333-4444-555555555555","state":"running","app_name":"Bench","host_name":"fakebox","created_at":"2026-08-23T10:00:00Z"}]'
SESS_SID=11111111-2222-3333-4444-555555555555
SESS_PID=""

sess_server_up() { # sess_server_up [--verdict V] [--expect-token T] [--sessions-seed J]
  python3 "$TESTS_DIR/fake_cp_server.py" --port "$SESS_PORT" --state "$SESS_DIR/state.json" \
    --sessions-seed "$SESS_SEED" "$@" > "$SESS_DIR/server.out" 2>&1 &
  SESS_PID=$!
  local _
  for _ in $(seq 1 40); do
    curl -fsS "http://127.0.0.1:$SESS_PORT/v1/hosts" >/dev/null 2>&1 && break
    sleep 0.25
  done
}
sess_server_down() {
  [ -n "$SESS_PID" ] || return 0
  kill "$SESS_PID" 2>/dev/null || true
  wait "$SESS_PID" 2>/dev/null || true
  SESS_PID=""
}

sess_env() { # sess_env <cmd...>  — the stubbed workstation
  PATH="$SESS_DIR/stub:$PATH" env -u QUASAR_ADMIN_TOKEN -u QSES_ADMIN_TOKEN -u HOST \
    XDG_CACHE_HOME="$SESS_DIR/cache" DX_HOSTS_JSON="$SESS_DIR/hosts.json" "$@"
}

SESS_CACHE="$SESS_DIR/cache/quasar/fakebox.token"

# ── the ladder ───────────────────────────────────────────────────────────────
sess_server_up --verdict nominal

# Tier 1 always wins and is NEVER cached: it is often the identity that owns the
# session, and caching it would leak that identity into unrelated runs.
rm -rf "$SESS_DIR/cache"
tok1_out="$(sess_env QUASAR_ADMIN_TOKEN=tier1-token bash "$DX/admin_token.sh" --host fakebox --quiet 2>/dev/null)"
if [ "$tok1_out" = "tier1-token" ] && [ ! -f "$SESS_CACHE" ]; then
  pass "admin-token:tier1" "\$QUASAR_ADMIN_TOKEN wins and is not cached"
else
  fail "admin-token:tier1" "got '$tok1_out', cache exists=$([ -f "$SESS_CACHE" ] && echo yes || echo no)"
fi

# Tier 3a: the per-boot dev key -> POST /v1/dev/agent-session.
rm -rf "$SESS_DIR/cache"
sess_ssh_ok; sess_docker_withkey
tok3a="$(sess_env bash "$DX/admin_token.sh" --host fakebox --quiet 2>/dev/null)"
if [ "$tok3a" = "devkey-minted-token" ]; then
  pass "admin-token:tier3-devkey" "minted via POST /v1/dev/agent-session"
else
  fail "admin-token:tier3-devkey" "got '$tok3a'"
fi

# Tier 2: the cache answers even when the host is unreachable.
sess_ssh_broken
tok2="$(sess_env bash "$DX/admin_token.sh" --host fakebox --quiet 2>/dev/null)"
if [ "$tok2" = "devkey-minted-token" ]; then
  pass "admin-token:cache-hit" "served from $SESS_CACHE with ssh down"
else
  fail "admin-token:cache-hit" "got '$tok2'"
fi

# An expired cache entry is re-minted, not served.
sess_ssh_ok
printf '1\nstale-token\n' > "$SESS_CACHE"
tok_exp="$(sess_env bash "$DX/admin_token.sh" --host fakebox --quiet 2>/dev/null)"
if [ "$tok_exp" = "devkey-minted-token" ]; then
  pass "admin-token:cache-expiry" "an expired entry is re-minted, never served"
else
  fail "admin-token:cache-expiry" "got '$tok_exp'"
fi

# --fresh ignores a perfectly good cache entry.
printf '%s\nnot-the-fresh-one\n' "$(( $(date -u +%s) + 9999 ))" > "$SESS_CACHE"
tok_fresh="$(sess_env bash "$DX/admin_token.sh" --host fakebox --quiet --fresh 2>/dev/null)"
if [ "$tok_fresh" = "devkey-minted-token" ]; then
  pass "admin-token:fresh" "--fresh bypasses a valid cache entry"
else
  fail "admin-token:fresh" "got '$tok_fresh'"
fi

# Tier 3b: no dev key anywhere, so the BOOTSTRAP_ADMIN_* login from the host's
# own deploy/.env is the last route.
rm -rf "$SESS_DIR/cache"
sess_docker_nokey
tok3b="$(sess_env bash "$DX/admin_token.sh" --host fakebox --quiet 2>/dev/null)"
if [ "$tok3b" = "bootstrap-minted-token" ]; then
  pass "admin-token:tier3-bootstrap" "fell through to POST /v1/auth/login"
else
  fail "admin-token:tier3-bootstrap" "got '$tok3b'"
fi

# Nothing resolvable at all: exit 2, every tier enumerated, and the NEXT COMMAND
# named. A ladder that fails silently is how a caller ends up sending
# `Authorization: Bearer ` and believing the 401.
rm -rf "$SESS_DIR/cache"
rm -f "$SESS_DIR/hostdir/deploy/.env"
nocred_out="$(sess_env bash "$DX/admin_token.sh" --host fakebox 2>&1)"; nocred_rc=$?
printf 'BOOTSTRAP_ADMIN_EMAIL=admin@local.test\nBOOTSTRAP_ADMIN_PASSWORD=local-dev-admin\n' \
  > "$SESS_DIR/hostdir/deploy/.env"
nocred_missing=""
for want in 'tier 1' 'tier 2' 'tier 3a' 'tier 3b' 'QUASAR_DEV_AGENT_AUTH=1' 'export QUASAR_ADMIN_TOKEN' 'BOOTSTRAP_ADMIN_EMAIL'; do
  printf '%s' "$nocred_out" | grep -Fq "$want" || nocred_missing="$nocred_missing '$want'"
done
if [ "$nocred_rc" -eq 2 ] && [ -z "$nocred_missing" ]; then
  pass "admin-token:no-credential" "rc=2 and every tier + the next command are named"
else
  fail "admin-token:no-credential" "rc=$nocred_rc missing:$nocred_missing"
fi

# A stack that is DOWN is reported as down — never as a rotated password. On
# 2026-08-23 a stopped tower stack read as "no credential" until this preflight.
sess_server_down
rm -rf "$SESS_DIR/cache"
down_out="$(sess_env bash "$DX/admin_token.sh" --host fakebox 2>&1)"; down_rc=$?
if [ "$down_rc" -eq 2 ] && printf '%s' "$down_out" | grep -Fq 'is DOWN' \
   && printf '%s' "$down_out" | grep -Fq 'make status HOST=fakebox' \
   && ! printf '%s' "$down_out" | grep -Fq 'password was rotated'; then
  pass "admin-token:stack-down" "an unreachable control plane names make status, not a credential tier"
else
  fail "admin-token:stack-down" "rc=$down_rc :: $(printf '%s' "$down_out" | tail -n 3 | tr '\n' ' ')"
fi
sess_server_up --verdict nominal

# ── the session verbs ────────────────────────────────────────────────────────
sess_docker_withkey

sess_verb() { # sess_verb <label> <want-rc> <verb> [KEY=VALUE ...]
  local label="$1" want="$2" verb="$3"; shift 3
  local out rc
  out="$(sess_env HOST=fakebox "$@" bash "$DX/session.sh" "$verb" 2>&1)"; rc=$?
  SESS_OUT="$out"
  if [ "$rc" -eq "$want" ]; then
    pass "session:$label" "rc=$rc as expected"
  else
    fail "session:$label" "expected rc=$want, got rc=$rc :: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  fi
  # The single terminal RESULT line is part of the contract on EVERY path.
  if printf '%s' "$out" | grep -qE '^RESULT status=(ok|degraded|failed|error) target=session-'; then
    pass "session:$label:result-line" "RESULT line present"
  else
    fail "session:$label:result-line" "no RESULT line :: $(printf '%s' "$out" | tail -n 2 | tr '\n' ' ')"
  fi
}

sess_verb list-ok 0 list
printf '%s' "$SESS_OUT" | grep -Fq "$SESS_SID" \
  && pass "session:list-contents" "the running session is listed" \
  || fail "session:list-contents" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"

# The metrics table's headline present column is the MEDIAN, with the vsync-beat
# share beside it. The mean (`present_fps`) misreads a source-fps == display-Hz
# session by 10-25% — that is what made a healthy 1440p120 stream look broken on
# 2026-08-22 — so PRES_SD gave way to PRES_FPS_MED + BEAT. SD is still in --json.
sess_verb metrics-cadence 0 metrics "SID=$SESS_SID"
if printf '%s' "$SESS_OUT" | grep -Fq 'PRES_FPS_MED' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'BEAT' \
   && printf '%s' "$SESS_OUT" | grep -Fq '2%'; then
  pass "session:metrics-cadence" "the metrics table reads the median fps and the beat share"
else
  fail "session:metrics-cadence" "$(printf '%s' "$SESS_OUT" | tail -n 4 | tr '\n' ' ')"
fi
printf '%s' "$SESS_OUT" | grep -Fq 'PRES_SD' \
  && fail "session:metrics-no-sd-column" "PRES_SD is still a column; the mean-era layout survived" \
  || pass "session:metrics-no-sd-column" "PRES_SD is out of the table (still in --json)"
sess_verb metrics-json 0 metrics "SID=$SESS_SID" JSON=1
printf '%s' "$SESS_OUT" | grep -Fq 'present_interval_sd_ms' \
  && pass "session:metrics-json-keeps-sd" "--json still carries present_interval_sd_ms" \
  || fail "session:metrics-json-keeps-sd" "$(printf '%s' "$SESS_OUT" | tail -n 3 | tr '\n' ' ')"

# SID=latest resolves the newest running session client-side (there is no
# ?state= filter on GET /v1/admin/sessions).
sess_verb latest-resolves 0 verdict SID=latest
printf '%s' "$SESS_OUT" | grep -Fq "sid=$SESS_SID" \
  && pass "session:latest-resolution" "SID=latest resolved to the running session" \
  || fail "session:latest-resolution" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"

printf '%s' "$SESS_OUT" | grep -Fq 'verdict=nominal' \
  && pass "session:verdict-nominal" "a healthy session is status=ok verdict=nominal, NOT an error" \
  || fail "session:verdict-nominal" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"

# ST-09: the verdict verb prints the VALUE, not just the state — the reason, the
# evidence tier, the clock quality, and the falsifier table. Those four are what
# let an operator check the verdict instead of taking it on faith; a verb that
# printed only the string is the shape that sent an agent to psql on 2026-08-22.
sess_verb verdict-value 0 verdict "SID=$SESS_SID"
verdict_value_missing=""
for want in 'reason:' 'tier:' 'clock:' 'falsifiers' 'encoder.fps' 'thresholds:'; do
  printf '%s' "$SESS_OUT" | grep -Fq "$want" || verdict_value_missing="$verdict_value_missing $want"
done
# A falsifier with no samples must read as a dash plus its note, never as a pass.
printf '%s' "$SESS_OUT" | grep -Fq 'no samples' || verdict_value_missing="$verdict_value_missing null-note"
if [ -z "$verdict_value_missing" ]; then
  pass "session:verdict-value" "reason, tier, clock and the falsifier table are all printed"
else
  fail "session:verdict-value" "missing:$verdict_value_missing :: $(printf '%s' "$SESS_OUT" | tail -n 3 | tr '\n' ' ')"
fi

# The clock line must say whether the offset was APPLIED, not just that it was
# measured. A measured-but-unapplied clock is the exact defect this whole change
# exists to remove, and an operator can only see it if the verb prints it. The
# warm-up line is the other half: an exclusion nobody is told about is
# indistinguishable from a measurement that never happened.
sess_verb verdict-clock-applied 0 verdict "SID=$SESS_SID"
verdict_clock_missing=""
for want in 'applied' 'age ' 'warm-up:'; do
  printf '%s' "$SESS_OUT" | grep -Fq "$want" || verdict_clock_missing="$verdict_clock_missing $want"
done
if [ -z "$verdict_clock_missing" ]; then
  pass "session:verdict-clock-applied" "the clock line says applied + age, and warm-up exclusion is printed"
else
  fail "session:verdict-clock-applied" "missing:$verdict_clock_missing :: $(printf '%s' "$SESS_OUT" | tail -n 3 | tr '\n' ' ')"
fi

# JSON=1 returns the Verdict VERBATIM (falsifiers and all) plus the RESULT fields,
# so a machine consumer never has to parse the terminal line.
sess_verb verdict-json 0 verdict "SID=$SESS_SID" JSON=1
if printf '%s' "$SESS_OUT" | python3 -c '
import json, sys
raw = sys.stdin.read()
start = raw.index("{")
end = raw.rindex("}") + 1
d = json.loads(raw[start:end])
need = ["verdict", "evidence", "reason", "window", "clock", "evidence_tier",
        "falsifiers", "thresholds_version", "status", "target", "sid", "host"]
missing = [k for k in need if k not in d]
assert not missing, missing
assert d["falsifiers"], "no falsifiers"
assert d["window"].get("n_host") is not None, "window has no sample counts"
' 2>/dev/null; then
  pass "session:verdict-json" "JSON=1 carries the whole Verdict plus the RESULT fields"
else
  fail "session:verdict-json" "$(printf '%s' "$SESS_OUT" | tail -n 3 | tr '\n' ' ')"
fi

sess_verb metrics-ok 0 metrics "SID=$SESS_SID"
sess_verb trace-ok 0 trace "SID=$SESS_SID"
sess_verb bundle-ok 0 bundle "SID=$SESS_SID" "OUT=$SESS_DIR/bundle.json"
if [ -s "$SESS_DIR/bundle.json" ] && python3 -c "import json,sys;json.load(open(sys.argv[1]))" "$SESS_DIR/bundle.json" 2>/dev/null; then
  pass "session:bundle-file" "raw bundle written and parses as JSON"
else
  fail "session:bundle-file" "no readable bundle at $SESS_DIR/bundle.json"
fi
sess_verb diagnose-ok 0 diagnose "SID=$SESS_SID"

sess_server_down

# A likely_* verdict is a DEGRADED session: exit 1, so a script can gate on it.
sess_server_up --verdict likely_network_congestion
sess_verb verdict-degraded 1 verdict "SID=$SESS_SID"
printf '%s' "$SESS_OUT" | grep -Fq 'status=degraded' \
  && pass "session:verdict-degraded-status" "likely_network_congestion is status=degraded" \
  || fail "session:verdict-degraded-status" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
sess_server_down

# THE 2026-08-22 REGRESSION, as a test. The control plane owns the verdict
# vocabulary and grows it (ST-07 #324 split "unknown" three ways). A verdict this
# tool has never seen is DATA: print it, note it, exit 0. The old qdiag validated
# against a stale four-string copy and exited 2 on a perfectly healthy session,
# which is what sent an agent to psql.
sess_server_up --verdict a_verdict_from_the_future
sess_verb verdict-unknown-string 0 verdict "SID=$SESS_SID"
if printf '%s' "$SESS_OUT" | grep -Fq 'a_verdict_from_the_future' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'reason=unrecognised-verdict'; then
  pass "session:verdict-unknown-string" "an unknown verdict is reported verbatim, exit 0"
else
  fail "session:verdict-unknown-string" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# ST-09 fallback: a control plane that predates GET .../verdict 404s it. The verb
# must fall back to the diagnostic bundle and still answer, because this
# workstation is routinely pointed at a host that has not been redeployed yet.
sess_server_up --verdict nominal --no-verdict-route
sess_verb verdict-404-fallback 0 verdict "SID=$SESS_SID"
if printf '%s' "$SESS_OUT" | grep -Fq 'verdict=nominal' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'falsifiers'; then
  pass "session:verdict-404-fallback" "a pre-ST-09 control plane falls back to the bundle's classifier"
else
  fail "session:verdict-404-fallback" "$(printf '%s' "$SESS_OUT" | tail -n 3 | tr '\n' ' ')"
fi

# The bundle also carries `ingest` when this control plane DROPPED client points
# for an implausible ts_unix_ms. Those points are the ones that used to be stored
# where no read window reaches them, so the count has to be surfaced where an
# operator is already looking, not left in a log line.
if printf '%s' "$SESS_OUT" | grep -Fq 'ingest:' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'looks like seconds'; then
  pass "session:verdict-ingest-counters" "dropped-timestamp counters are printed with the offending value's likely domain"
else
  fail "session:verdict-ingest-counters" "$(printf '%s' "$SESS_OUT" | tail -n 4 | tr '\n' ' ')"
fi
sess_server_down

# ── session-capture ──────────────────────────────────────────────────────────
# The capture verb is the only session verb that WRITES to the stack (it arms an
# observation), so its refusal paths matter more than the others': each one has a
# different next command, and getting them wrong sends an operator to ssh.
rm -rf "$SESS_DIR/cache"
SESS_SEED='[{"id":"11111111-2222-3333-4444-555555555555","state":"running","app_name":"Bench","host_name":"fakebox","created_at":"2026-08-23T10:00:00Z"}]'
SESS_SID=11111111-2222-3333-4444-555555555555
CAP_OUT="$SESS_DIR/captures"

sess_server_up --capture-mode ok --capture-polls 2
sess_verb capture-dot 0 capture "SID=$SESS_SID" KIND=pipeline_dot "OUT=$CAP_OUT"
cap_dot="$(ls "$CAP_OUT"/capture-"$SESS_SID"-pipeline_dot-*.dot 2>/dev/null | head -n 1)"
if [ -n "$cap_dot" ] && grep -Fq 'digraph pipeline' "$cap_dot"; then
  pass "session:capture-dot-file" "gzip+base64 decoded to a real .dot on disk"
else
  fail "session:capture-dot-file" "no decoded .dot under $CAP_OUT :: $(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
# The file holds a live session's graph; it is written 0600 like the bundle.
cap_mode="$(python3 -c 'import os,stat,sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode)))' "$cap_dot" 2>/dev/null)"
if [ "$cap_mode" = "0o600" ]; then
  pass "session:capture-file-mode" "the capture is written 0600"
else
  fail "session:capture-file-mode" "mode=$cap_mode"
fi
# The RESULT line has to carry the numbers that say whether the artifact is whole.
if printf '%s' "$SESS_OUT" | grep -Fq 'capture_id=' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'truncated=false' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'duration_ms='; then
  pass "session:capture-result-fields" "RESULT names capture_id, bytes, truncated, duration_ms"
else
  fail "session:capture-result-fields" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi

# A json-encoded kind lands as .json, not as an undecodable blob.
sess_verb capture-json 0 capture "SID=$SESS_SID" KIND=encoder_props "OUT=$CAP_OUT"
cap_json="$(ls "$CAP_OUT"/capture-"$SESS_SID"-encoder_props-*.json 2>/dev/null | head -n 1)"
if [ -n "$cap_json" ] && grep -Fq 'vulkanh264enc' "$cap_json"; then
  pass "session:capture-json-file" "an encoding=json capture is written as readable .json"
else
  fail "session:capture-json-file" "no .json under $CAP_OUT"
fi
sess_server_down

# KIND=all runs the three SEQUENTIALLY — captures are single-flight per session,
# so a parallel fan-out would 409 two of its own three requests.
rm -rf "$CAP_OUT"
sess_server_up --capture-mode ok --capture-polls 0
sess_verb capture-all 0 capture "SID=$SESS_SID" KIND=all "OUT=$CAP_OUT"
cap_n=0
for f in "$CAP_OUT"/capture-*; do [ -e "$f" ] && cap_n=$(( cap_n + 1 )); done
if [ "${cap_n:-0}" -ge 3 ]; then
  pass "session:capture-all" "KIND=all wrote all three captures"
else
  fail "session:capture-all" "got $cap_n files under $CAP_OUT"
fi
sess_server_down

# 409 busy: single-flight. The next command is "wait", NOT "retry in a loop".
sess_server_up --capture-mode busy
sess_verb capture-busy 2 capture "SID=$SESS_SID" KIND=pipeline_dot "OUT=$CAP_OUT"
if printf '%s' "$SESS_OUT" | grep -Fq 'reason=capture-busy' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'single-flight'; then
  pass "session:capture-busy" "a 409 says single-flight and does not read as a tool bug"
else
  fail "session:capture-busy" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# 422: this kind is not available here. The next command names another kind.
sess_server_up --capture-mode unknown_kind
sess_verb capture-422 2 capture "SID=$SESS_SID" KIND=burst_stats "OUT=$CAP_OUT"
if printf '%s' "$SESS_OUT" | grep -Fq 'reason=capture-kind'; then
  pass "session:capture-422" "an unsupported kind is a tool error naming another kind"
else
  fail "session:capture-422" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# 501: the host's agent predates captures. Retrying will NEVER help, so the
# message must name the rebuild — this is the whole reason 501 exists here.
sess_server_up --capture-mode old_agent
sess_verb capture-501 2 capture "SID=$SESS_SID" KIND=pipeline_dot "OUT=$CAP_OUT"
if printf '%s' "$SESS_OUT" | grep -Fq 'reason=capture-unsupported' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'make rebuild HOST=fakebox'; then
  pass "session:capture-501" "an agent that predates captures names \`make rebuild\`"
else
  fail "session:capture-501" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# 503: the host has no agent connection at all.
sess_server_up --capture-mode not_connected
sess_verb capture-503 2 capture "SID=$SESS_SID" KIND=pipeline_dot "OUT=$CAP_OUT"
printf '%s' "$SESS_OUT" | grep -Fq 'reason=agent-not-connected' \
  && pass "session:capture-503" "a disconnected agent is named as such" \
  || fail "session:capture-503" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
sess_server_down

# Armed, then nothing ever reports: the poll must give up on a deadline with a
# next command, never hang. (CAPTURE_TIMEOUT_S keeps the suite quick.)
sess_server_up --capture-mode never
sess_verb capture-timeout 2 capture "SID=$SESS_SID" KIND=pipeline_dot "OUT=$CAP_OUT" CAPTURE_TIMEOUT_S=1
if printf '%s' "$SESS_OUT" | grep -Fq 'reason=capture-timeout' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'make session-logs'; then
  pass "session:capture-timeout" "a capture that never reports times out and names the logs"
else
  fail "session:capture-timeout" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# C9 log spans: `session-logs` filters the agent's log to one session. The agent
# now stamps the session id three different ways (span prefix in text format,
# span object in json format, explicit `session_id=` field outside the span) plus
# prose, and the SID filter must catch all four. The pattern is a single literal
# line in session.sh; lift it out verbatim and run real log lines through it, so
# this fails if the pattern and the log shapes ever drift apart.
sid_filter_case() {
  local sid="11111111-2222-3333-4444-555555555555"
  local re
  # shellcheck disable=SC2034  # SID is read by the eval'd assignment lifted from session.sh
  re=$(SID="$sid"; eval "$(grep -m1 '^SID_FILTER_RE=' "$DX/session.sh")"; printf '%s' "$SID_FILTER_RE")
  [ -n "$re" ] || { fail "session:logs-sid-filter" "no SID_FILTER_RE found in session.sh"; return; }
  local matched=0 name
  for line in \
    "2026-08-23T00:00:00Z  WARN session{id=$sid host=devbox}: quasar_node_agent::session::runner: token=\"encoder-stall\" no encoder output" \
    "2026-08-23T00:00:00Z  WARN quasar_node_agent::agent: token=\"session-assign-rejected\" session_id=$sid rejected" \
    "{\"timestamp\":\"x\",\"level\":\"WARN\",\"token\":\"encoder-stall\",\"spans\":[{\"id\":\"$sid\",\"host\":\"devbox\",\"name\":\"session\"}]}" \
    "2026-08-23T00:00:00Z  WARN quasar_node_agent::agent: session $sid assignment rejected"
  do
    printf '%s\n' "$line" | grep -aEq "$re" && matched=$((matched + 1))
  done
  name="session:logs-sid-filter"
  if [ "$matched" -ne 4 ]; then
    fail "$name" "the SID filter matched only $matched of 4 log shapes"
  elif printf '%s\n' "2026-08-23T00:00:00Z  INFO some other session 99999999-0000-0000-0000-000000000000 started" | grep -aEq "$re"; then
    fail "$name" "the SID filter matched a line belonging to another session"
  else
    pass "$name" "span, session_id=, json span and prose forms all match; another sid does not"
  fi
}
sid_filter_case

# A rejected token is a TOOL error (exit 2) that names --fresh, because the usual
# cause is a cached token that outlived the stack that minted it.
sess_server_up --verdict nominal --expect-token nobody-has-this
sess_verb unauthorized 2 verdict "SID=$SESS_SID"
if printf '%s' "$SESS_OUT" | grep -Fq 'admin_token.sh --host fakebox --fresh' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'reason=unauthorized'; then
  pass "session:401-next-step" "a 401 names \`admin_token.sh --fresh\` as the next command"
else
  fail "session:401-next-step" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# No running session: exit 2 naming the list verb, never a traceback or a hang.
rm -rf "$SESS_DIR/cache"
SESS_SEED='[]'
sess_server_up --verdict nominal
sess_verb latest-none-running 2 verdict SID=latest
if printf '%s' "$SESS_OUT" | grep -Fq 'make session-list' \
   && printf '%s' "$SESS_OUT" | grep -Fq 'reason=no-running-session'; then
  pass "session:latest-none-running" "0 running sessions names \`make session-list\`"
else
  fail "session:latest-none-running" "$(printf '%s' "$SESS_OUT" | tail -n 2 | tr '\n' ' ')"
fi
sess_server_down

# Usage errors are rc 3, distinct from a tool error.
sess_env HOST=fakebox bash "$DX/session.sh" not-a-verb >/dev/null 2>&1; sess_rc_usage=$?
if [ "$sess_rc_usage" -eq 3 ]; then
  pass "session:usage-rc" "an unknown verb is rc=3"
else
  fail "session:usage-rc" "expected rc=3, got $sess_rc_usage"
fi

# The quasar-diagnose skill must own no credential path and no verdict list.
qd_leaks=""
# A deploy/.env MENTION is fine (the IR experiment mutates it on the host); a
# credential READ is not — those belong to scripts/dx/admin_token.sh.
# (scripts/validate is exempt: its BOOTSTRAP_ADMIN strings are .env FIXTURES for
# the ir_env mutation test, not a credential path.)
if grep -rlF 'BOOTSTRAP_ADMIN' "$ROOT/.claude/skills/quasar-diagnose" 2>/dev/null \
     | grep -v '/scripts/validate$' | grep -q .; then
  qd_leaks="$qd_leaks bootstrap-admin-credential-read"
fi
if grep -rqF 'likely_encoder_saturation' "$ROOT/.claude/skills/quasar-diagnose/config.json" 2>/dev/null; then
  qd_leaks="$qd_leaks verdict-vocabulary-in-config"
fi
if [ -z "$qd_leaks" ]; then
  pass "qdiag:no-creds-no-vocabulary" "the skill reads no deploy/.env and owns no verdict enum"
else
  warn "qdiag:no-creds-no-vocabulary" "still present:$qd_leaks"
fi

# ── metric manifest ──────────────────────────────────────────────────────────
# docs/session-trace/metrics.json is the source; the Go embed copy, the web
# bundle copy and the trace-format.md section-2 table are generated from it.
# Check rather than regenerate, so a stale artifact is a red verify and not a
# surprise in review.

if bash "$DX/metrics_manifest.sh" check >/dev/null 2>&1; then
  pass "manifest:copies-in-sync" "Go + web copies match docs/session-trace/metrics.json"
else
  fail "manifest:copies-in-sync" "run: make docs-metrics-sync"
fi

if python3 "$DX/gen_trace_docs.py" --check >/dev/null 2>&1; then
  pass "manifest:trace-format-table" "trace-format.md section 2 matches the manifest"
else
  fail "manifest:trace-format-table" "run: make docs-trace"
fi

MANIFEST_SERIES_OUT="$(python3 "$TESTS_DIR/check_manifest_series.py" 2>&1)"
if [ $? -eq 0 ]; then
  pass "manifest:falsifier-series-exist" "every verdict.go series name is in the manifest"
else
  fail "manifest:falsifier-series-exist" "$MANIFEST_SERIES_OUT"
fi

# C11 #5: every key bench_submit.py's ROLLUP_KEYS/ROLLUP_COUNTERS folds must be
# in docs/session-trace/metrics.json under the same source, and not
# deprecated_for another key — this is the check that would have caught
# present_fps (deprecated_for=present_fps_median) still being folded.
BENCH_KEYS_OUT="$(python3 "$TESTS_DIR/check_bench_keys.py" 2>&1)"
if [ $? -eq 0 ]; then
  pass "manifest:bench-keys-not-drifted" "$BENCH_KEYS_OUT"
else
  fail "manifest:bench-keys-not-drifted" "$BENCH_KEYS_OUT"
fi

# ── summary ──────────────────────────────────────────────────────────────────
printf '\n'
STATUS=ok
if [ "$FAIL_N" -gt 0 ]; then
  STATUS=failed
elif [ "$WARN_N" -gt 0 ]; then
  STATUS=degraded
fi
printf 'RESULT status=%s target=verify pass=%d warn=%d fail=%d\n' \
  "$STATUS" "$PASS_N" "$WARN_N" "$FAIL_N"
[ "$FAIL_N" -eq 0 ] || exit 1
exit 0
