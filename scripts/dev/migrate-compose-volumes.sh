#!/usr/bin/env bash
# migrate-compose-volumes.sh — print the .env lines + compose invocation for
# adopting pre-existing named volumes that don't match this repo's names.
#
# Run this ON the host, with the stack (old or new) still up so the volumes
# actually exist:
#
#   bash scripts/dev/migrate-compose-volumes.sh
#
# It only inspects `docker volume ls` and prints; it changes nothing itself.
# Background: docs/operations/compose-consolidation-migration.md — a host that used to
# run a forked compose file (its own volume names, e.g. quasar-nv-*) needs to
# tell the base compose file about the volumes it already has, or an in-place
# upgrade starts against an empty database and an unenrolled agent.
#
# As of #448 those overrides are NOT set directly on the base file (Compose v5
# rejects an empty `name:` default at `up`, and `docker compose config`
# silently drops it so nothing catches the breakage before then). They live in
# the opt-in overlay deploy/overlays/docker-compose.adopt-volumes.yml instead — apply it
# ONLY on a host that needs adoption, using the values this script prints.
set -euo pipefail

echo "Docker volumes with 'quasar' in the name on this host:"
echo
docker volume ls --format '{{.Name}}' | grep quasar || {
  echo "(none found — nothing to adopt; a fresh deploy needs no volume overrides)"
  exit 0
}
echo

# Best-effort guesses at which volume is which, based on the naming this repo
# and its predecessor forked compose file used. THESE ARE HINTS, NOT PROOF —
# a renamed or hand-created volume won't match; the operator must always
# confirm against the list above before pasting anything into deploy/.env.
guess() {
  # `|| true`: no match is an expected answer here (a host may have e.g. a
  # postgres volume but no control-tls one). Without it, `set -euo pipefail`
  # kills the whole script inside the command substitution below and the
  # operator never sees the placeholders or the compose invocation.
  docker volume ls --format '{{.Name}}' | grep quasar | grep -E "$1" | head -1 || true
}
PG_GUESS="$(guess 'postgres-data')"
AGENT_GUESS="$(guess 'agent-data')"
TLS_GUESS="$(guess 'control-tls')"

echo "Paste the following into deploy/.env, replacing any guess that is wrong"
echo "(confirm each name against the volume list above — do not paste blind):"
echo
echo "QUASAR_POSTGRES_VOLUME=${PG_GUESS:-<paste the postgres volume name>}"
echo "QUASAR_AGENT_VOLUME=${AGENT_GUESS:-<paste the agent volume name>}"
echo "QUASAR_CONTROL_VOLUME=${TLS_GUESS:-<paste the control-tls volume name>}"
echo
echo "deploy/redeploy.sh detects all three values in deploy/.env and adds the"
echo "adoption overlay automatically, so the normal redeploy path just works."
echo
echo "MANUAL compose invocations must add the overlay themselves — omitting it"
echo "means these three .env values are set but never read, and the deploy"
echo "silently starts against fresh, empty volumes instead of adopting the old:"
echo
echo "  docker compose -f deploy/docker-compose.yml \\"
echo "                 -f deploy/overlays/docker-compose.adopt-volumes.yml up -d"
echo
echo "(add any other overlay you normally use, e.g. -f deploy/docker-compose.nvidia.yml,"
echo "in the same -f chain — see deploy/redeploy.sh for the per-profile chains.)"
