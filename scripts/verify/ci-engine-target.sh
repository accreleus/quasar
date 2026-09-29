#!/usr/bin/env bash
# Brings up one engine mode on a GitHub-hosted Ubuntu runner for the engine suite (#408,
# .github/workflows/ci.yml job `engines`) and writes QUASAR_ENGINE_SUITE_TARGETS (and
# ENGINE_SUITE_AS_ROOT for a root-owned socket) to $GITHUB_ENV.
#
# It does, on a throwaway VM, the parts of host preparation (deploy/prepare-host.sh) an
# engine mode needs: subordinate ids and lingering for the runner user. Nothing here grants
# a container more than the suite asks for.
set -euo pipefail

target="${1:?usage: ci-engine-target.sh <docker-rootful|docker-rootless|podman-rootful|podman-rootless>}"
env_file="${GITHUB_ENV:?not on a GitHub runner}"
user="$(id -un)"
uid="$(id -u)"

wait_for_socket() {
  for _ in $(seq 1 60); do
    [ -S "$1" ] && return 0
    sleep 1
  done
  echo "no engine socket at $1" >&2
  return 1
}

# A rootless engine runs under the runner user's own systemd manager, as the Quasar user's
# does after host preparation: lingering, and subordinate ids.
user_session() {
  if ! grep -q "^$user:" /etc/subuid; then
    sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$user"
  fi
  sudo loginctl enable-linger "$user"
  for _ in $(seq 1 30); do
    [ -S "/run/user/$uid/bus" ] && break
    sleep 1
  done
  export XDG_RUNTIME_DIR="/run/user/$uid"
  export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
}

case "$target" in
  docker-rootful)
    socket=/var/run/docker.sock
    docker version --format 'Docker {{.Server.Version}}'
    ;;
  podman-rootful)
    sudo systemctl start podman.socket
    socket=/run/podman/podman.sock
    wait_for_socket "$socket"
    sudo podman version --format 'Podman {{.Server.Version}}'
    echo "ENGINE_SUITE_AS_ROOT=1" >>"$env_file"
    ;;
  podman-rootless)
    user_session
    systemctl --user start podman.socket
    socket="$XDG_RUNTIME_DIR/podman/podman.sock"
    wait_for_socket "$socket"
    podman version --format 'Podman {{.Server.Version}}'
    ;;
  docker-rootless)
    user_session
    # The rootless helpers of the runner's own Docker release, from Docker's static bundle.
    version="$(docker version --format '{{.Server.Version}}')"
    extras="$HOME/.local/docker-rootless-extras"
    mkdir -p "$extras"
    curl -fsSL "https://download.docker.com/linux/static/stable/$(uname -m)/docker-rootless-extras-${version}.tgz" \
      | tar -xz -C "$extras" --strip-components=1
    # Ubuntu 24.04 confines unprivileged user namespaces through AppArmor; a rootlesskit
    # outside /usr needs its own profile allowing them, as Docker's rootless docs describe.
    if [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)" = 1 ]; then
      printf 'abi <abi/4.0>,\ninclude <tunables/global>\nprofile quasar-ci-rootlesskit %s flags=(unconfined) {\n  userns,\n}\n' \
        "$extras/rootlesskit" | sudo tee /etc/apparmor.d/quasar-ci-rootlesskit >/dev/null
      sudo apparmor_parser -r /etc/apparmor.d/quasar-ci-rootlesskit
    fi
    PATH="$extras:$PATH" nohup dockerd-rootless.sh >"$RUNNER_TEMP/dockerd-rootless.log" 2>&1 &
    socket="$XDG_RUNTIME_DIR/docker.sock"
    if ! wait_for_socket "$socket"; then
      tail -n 40 "$RUNNER_TEMP/dockerd-rootless.log" >&2
      exit 1
    fi
    DOCKER_HOST="unix://$socket" docker version --format 'Docker {{.Server.Version}} (rootless)'
    ;;
  *)
    echo "unknown target $target" >&2
    exit 2
    ;;
esac
echo "QUASAR_ENGINE_SUITE_TARGETS=$target=$socket" >>"$env_file"
