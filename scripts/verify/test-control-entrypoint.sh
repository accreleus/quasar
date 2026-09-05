#!/usr/bin/env bash
# Exercise identity preparation against the runtime base, with ephemeral tmpfs only.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
image=${QUASAR_ENTRYPOINT_TEST_IMAGE:-ghcr.io/accreleus/quasar-base:latest}
common=(--rm --network none --mount "type=bind,src=$root/deploy/control-entrypoint.sh,dst=/entry.sh,readonly")
docker run "${common[@]}" --user 0:0 --tmpfs /var/lib/quasar-control:uid=99,gid=100 --tmpfs /run/quasar --entrypoint /entry.sh "$image" sh -ec '
  test "$(id -u)" = 99
  test "$(id -g)" = 100
  test "$$" = 1
  test -w /var/lib/quasar-control
  test -w /run/quasar
'
docker run "${common[@]}" --user 1000:1000 --tmpfs /var/lib/quasar-control:uid=1000,gid=1000 --tmpfs /run/quasar:uid=1000,gid=1000,mode=0700 --entrypoint /entry.sh "$image" sh -ec '
  test "$(id -u)" = 1000
  test "$$" = 1
  test -w /var/lib/quasar-control
'
docker run "${common[@]}" --user 0:0 --tmpfs /var/lib/quasar-control:uid=99,gid=100 --tmpfs /run/quasar --entrypoint sh "$image" -ec '
  echo state > /var/lib/quasar-control/old-state
  echo private > /run/quasar/old-key
  chmod 0600 /run/quasar/old-key
  chown 1000:1000 /var/lib/quasar-control/old-state /run/quasar/old-key
  exec /entry.sh sh -ec '\''test "$(id -u)" = 99; test -w /var/lib/quasar-control/old-state; test -r /run/quasar/old-key; test "$$" = 1'\''
'
if docker run "${common[@]}" --user 1000:1000 --tmpfs /var/lib/quasar-control:uid=99,gid=100,mode=0700 --tmpfs /run/quasar:uid=1000,gid=1000 --entrypoint /entry.sh "$image" true; then
  echo 'FAIL: mismatched non-root state ownership was silently accepted' >&2
  exit 1
fi
for invalid_uid in 0 00 000 99:100; do
  if docker run "${common[@]}" --user 0:0 -e "QUASAR_CONTROL_UID=$invalid_uid" --tmpfs /var/lib/quasar-control --tmpfs /run/quasar --entrypoint /usr/bin/timeout "$image" 5s /entry.sh true; then
    echo "FAIL: invalid control UID was accepted: $invalid_uid" >&2
    exit 1
  else
    status=$?
    if [ "$status" -ne 1 ]; then
      echo "FAIL: invalid UID must be rejected immediately (exit=$status)" >&2
      exit 1
    fi
  fi
done
echo 'PASS: control entrypoint prepares ownership, drops privileges, preserves PID 1 and rejects inaccessible state'
