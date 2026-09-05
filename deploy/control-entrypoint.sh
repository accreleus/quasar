#!/bin/sh
# Prepare only Quasar-owned state, then leave Go in charge of PID 1/signals.
set -eu

fail() { printf 'Quasar startup: %s\n' "$*" >&2; exit 1; }
state=/var/lib/quasar-control
runtime=/run/quasar
if [ -L "$state" ] || [ -L "$runtime" ]; then
    fail 'state/runtime directory must not be a symlink'
fi

if [ "$(id -u)" = 0 ]; then
    mkdir -p "$state" "$runtime" || fail 'cannot create state/runtime directories; check mount permissions'
    owner=$(stat -c %u "$state")
    group=$(stat -c %g "$state")
    # A new root-owned bind is not a request to run the control plane as root.
    [ "$owner" != 0 ] || owner=1000
    [ "$group" != 0 ] || group=1000
    uid=${QUASAR_CONTROL_UID:-$owner}
    gid=${QUASAR_CONTROL_GID:-$group}
    case "$uid" in ''|*[!0-9]*) fail 'control UID must be numeric' ;; esac
    case "$gid" in ''|*[!0-9]*) fail 'control GID must be numeric' ;; esac
    [ "$uid" -ne 0 ] || fail 'control plane must run as a non-root UID'
    # Migrate files owned by the image's previous identity, never arbitrary
    # host trees or symlink targets. Do not cross nested mounts.
    find "$state" "$runtime" -xdev -uid 1000 -exec chown -h "$uid:$gid" {} + || fail 'cannot prepare existing control-plane state'
    chown "$uid:$gid" "$state" "$runtime" || fail 'cannot set state/runtime ownership; check read-only mounts or root-squashed storage'
    chmod 0700 "$runtime" || fail 'cannot secure private runtime directory'
    exec setpriv --reuid="$uid" --regid="$gid" --clear-groups --no-new-privs "$0" "$@"
fi

[ -z "${QUASAR_CONTROL_UID:-}" ] || [ "$QUASAR_CONTROL_UID" = "$(id -u)" ] ||
    fail 'QUASAR_CONTROL_UID differs from the running user; bind-storage preparation must start with user: "0:0" so the entrypoint can prepare ownership and drop privileges'
[ -z "${QUASAR_CONTROL_GID:-}" ] || [ "$QUASAR_CONTROL_GID" = "$(id -g)" ] ||
    fail 'QUASAR_CONTROL_GID differs from the running group; use the bind-storage preparation entrypoint as root'

for dir in "$state" "$runtime"; do
    if [ ! -d "$dir" ] || [ ! -w "$dir" ] || [ ! -x "$dir" ]; then
        fail "$dir is not writable by $(id -u):$(id -g); prepare this mount for that identity or use root preparation for a bind mount"
    fi
done

if [ "$#" -gt 0 ]; then exec "$@"; fi
exec /usr/local/bin/quasar-control
