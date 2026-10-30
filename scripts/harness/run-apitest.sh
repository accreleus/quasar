#!/usr/bin/env bash
# Ephemeral API-conformance harness: boots a throwaway Postgres + control-plane
# (via `go run`, no images needed), then runs the quasar-apitest validator which
# drives the /v1 surface and validates every response against protocol/openapi.yaml.
#
# Requires only Docker. Nothing touches the aux host or the lab host. Self-cleans on exit.
#
#   scripts/harness/run-apitest.sh            # boot, test, teardown
#   KEEP=1 scripts/harness/run-apitest.sh     # leave the stack up for debugging
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
NET=quasar-apitest-net
PG=quasar-apitest-pg
CP=quasar-apitest-cp
# The control plane's self-signed cert, shared with the validator: sign-in is
# refused over plain HTTP (#604), so the validator talks to the TLS port.
TLSVOL=quasar-apitest-tls
GO=golang:1.26
ADMIN_EMAIL=admin@quasar.local
# Must not contain the username or the email local-part — the control plane refuses
# such a bootstrap password and exits before serving (`fatal: bootstrap admin: password
# must not contain your username or email`), which presents as "control-plane never
# came up". Keep this free of "admin" and "quasar".
ADMIN_PASS=throwaway-conformance-pw-7431
RESULTS="${RESULTS_JSON:-$REPO/scripts/harness/apitest/results.json}"

cleanup() {
  [ "${KEEP:-0}" = "1" ] && { echo "KEEP=1 — leaving stack up ($PG,$CP on $NET)"; return; }
  # -v so the postgres data volume does not outlive the run (#205)
  docker rm -fv "$CP" "$PG" >/dev/null 2>&1 || true
  docker volume rm "$TLSVOL" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== [1/4] network + postgres ==="
docker network create "$NET" >/dev/null 2>&1 || true
docker rm -fv "$PG" "$CP" >/dev/null 2>&1 || true
docker volume rm "$TLSVOL" >/dev/null 2>&1 || true
docker run -d --name "$PG" --network "$NET" \
  -e POSTGRES_USER=quasar -e POSTGRES_PASSWORD=quasar -e POSTGRES_DB=quasar \
  postgres:16 >/dev/null
echo "waiting for postgres..."
for i in $(seq 1 30); do
  # TCP probe (-h): the official image's temporary init server listens on the
  # Unix socket only, where a plain pg_isready answers too (#270).
  if docker exec "$PG" pg_isready -h 127.0.0.1 -U quasar >/dev/null 2>&1; then echo "pg ready"; break; fi
  sleep 1
done

echo "=== [2/4] control-plane (go run) ==="
# Warm the module cache + start the server. go run stays in the foreground of the
# container; we detach the container and poll /health.
docker run -d --name "$CP" --network "$NET" \
  -v "$REPO/control-plane":/w -w /w -v "$TLSVOL":/var/lib/quasar-control/tls \
  -e DATABASE_URL="postgres://quasar:quasar@$PG:5432/quasar?sslmode=disable" \
  -e BOOTSTRAP_ADMIN_EMAIL="$ADMIN_EMAIL" \
  -e BOOTSTRAP_ADMIN_USERNAME=admin \
  -e BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PASS" \
  -e REGISTRATION_MODE=open \
  -e LISTEN_ADDR=":8080" \
  -e QUASAR_TLS_HOSTS="$CP" \
  "$GO" sh -c 'go run ./cmd/quasar-control' >/dev/null

echo "waiting for control-plane /health (compiling)..."
UP=0
for i in $(seq 1 90); do
  if docker run --rm --network "$NET" "$GO" \
       sh -c "wget -qO- http://$CP:8080/health >/dev/null 2>&1"; then UP=1; echo "control-plane up"; break; fi
  sleep 2
done
if [ "$UP" != "1" ]; then
  echo "!! control-plane never came up — logs:"; docker logs "$CP" 2>&1 | tail -40; exit 1
fi

echo "=== [3/4] run conformance validator ==="
set +e
docker run --rm --network "$NET" \
  -v "$REPO/scripts/harness/apitest":/w -v "$REPO/protocol":/protocol -w /w -v "$TLSVOL":/tls:ro \
  -e QUASAR_URL="https://$CP:8443" -e SSL_CERT_FILE=/tls/cert.pem \
  -e ADMIN_EMAIL="$ADMIN_EMAIL" -e ADMIN_PASSWORD="$ADMIN_PASS" \
  -e SPEC=/protocol/openapi.yaml \
  -e RESULTS_JSON=/w/results.json \
  "$GO" sh -c 'go run .'
RC=$?
set -e

echo "=== [4/4] done (rc=$RC) ==="
[ -f "$REPO/scripts/harness/apitest/results.json" ] && echo "results: $REPO/scripts/harness/apitest/results.json"
exit $RC
