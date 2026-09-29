/**
 * A fake container-engine PATH for exercising generated install scripts in tests.
 *
 * SAFETY. A generated script's own preflight only ever calls `docker`/`podman`
 * through PATH, so a fake `docker`/`podman` placed ahead of the real ones on
 * PATH used to look like enough isolation. It is not: the quick start's host
 * preparation step runs `sudo sh prepare-host.sh …`, and `sudo` does not honour
 * the PATH we hand a child process — it re-execs through its own
 * `secure_path`/PAM environment, which can still find a REAL `docker` or
 * `podman` on the machine running the test. On this host that is exactly what
 * happened once: a native `npm test` run left a real Quasar install behind.
 *
 * So this harness:
 *   - never inherits the calling process's environment (no stray PATH, no
 *     inherited docker/podman env vars) — every test env is built from
 *     scratch by `fakeEnv()`;
 *   - intercepts `sudo` itself (never the real setuid binary): the shim logs
 *     the call and re-execs its argument directly, unprivileged, through the
 *     same fake PATH, so a `sudo docker …` or `sudo podman …` nested inside
 *     `sudo sh prepare-host.sh` still lands on the fakes below, not a real
 *     engine;
 *   - intercepts `systemctl` (Quadlet's `daemon-reload`/`start`) so nothing
 *     here ever asks a real init system to start a unit;
 *   - intercepts `sha256sum -c` so the checksum step in the generated prep
 *     block always resolves without needing to fetch the real
 *     `deploy/prepare-host.sh` over the network;
 *   - intercepts `curl`, which never contacts the network: a `-o FILE` request
 *     is answered with a harmless local no-op script, and every other call
 *     just succeeds.
 *
 * Nothing here ever calls a real `docker`, `podman` or `sudo` binary, by name
 * or by absolute path. The suite's last test, "no generated script ever
 * reaches a real container engine", is the check that failure of this
 * isolation would be caught rather than silently leaving a real container
 * behind again: it looks for a REAL engine on the machine running the tests
 * (outside this harness's PATH) and asserts no `quasar-*` container exists
 * there once every other test has run.
 *
 * Run these tests only in a container with no docker/podman socket reachable
 * — see site/src/data/stack-template.test.js's file header.
 */
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, chmodSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

function shim(dir, name, body) {
  const path = join(dir, name);
  writeFileSync(path, `#!/usr/bin/env bash\n${body}\n`);
  chmodSync(path, 0o755);
}

/**
 * Builds a fake bin directory on a fresh temp dir and returns it plus a
 * `read()` helper for the call log every shim appends to. `docker`/`podman`
 * report `existing` for `container inspect` / `secret exists` and `legacy`
 * for a Compose-labelled control plane, matching the flags the generated
 * script checks for before it will start the seed.
 */
export function fakeEngineDir({ legacy = false, existing = false } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'quasar-qs-'));
  const log = join(dir, 'calls');
  writeFileSync(log, '');

  shim(dir, 'docker', `echo "docker $*" >> ${JSON.stringify(log)}
case "$1" in
  info) exit 0 ;;
  ps) case "$*" in *service=quasar-control-plane*) ${legacy ? 'echo deadbeef' : ':'} ;; esac ;;
  container) ${existing ? 'exit 0' : 'exit 1'} ;;
  pull) exit 0 ;;
  image) ref="\${@: -1}"; echo "\${ref%:*}@sha256:0123abcd" ;;
  run) echo cafe ;;
  exec) exit 0 ;;
esac`);

  shim(dir, 'podman', `echo "podman $*" >> ${JSON.stringify(log)}
case "$1" in
  info) exit 0 ;;
  container) ${existing ? 'exit 0' : 'exit 1'} ;;
  pull) exit 0 ;;
  image) ref="\${@: -1}"; echo "\${ref%:*}@sha256:0123abcd" ;;
  secret) case "$2" in exists) exit ${existing ? 0 : 1} ;; create) echo fakesecret ;; esac ;;
  exec) exit 0 ;;
esac`);

  // Never the real, setuid `sudo`: strip the name and run the rest directly,
  // unprivileged, through this same fake PATH.
  shim(dir, 'sudo', `echo "sudo $*" >> ${JSON.stringify(log)}
exec "$@"`);

  shim(dir, 'systemctl', `echo "systemctl $*" >> ${JSON.stringify(log)}
exit 0`);

  shim(dir, 'sha256sum', `echo "sha256sum $*" >> ${JSON.stringify(log)}
case "$*" in
  *-c*)
    # The fake curl below never fetches the real prepare-host.sh, so a real
    # checksum can never match here. This only proves the script calls
    # \`sha256sum -c\`, not that a real mismatch would be caught.
    cat >/dev/null
    exit 0
    ;;
  *) exec /usr/bin/sha256sum "$@" ;;
esac`);

  shim(dir, 'curl', `echo "curl $*" >> ${JSON.stringify(log)}
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
[ -z "$out" ] || printf '#!/bin/sh\\nexit 0\\n' > "$out"
exit 0`);

  return {
    dir,
    read: () => {
      try {
        return readFileSync(log, 'utf8');
      } catch {
        return '';
      }
    },
    cleanup: () => rmSync(dir, { recursive: true, force: true }),
  };
}

/**
 * A from-scratch environment for a generated script: no inherited PATH, no
 * inherited docker/podman/systemd env vars. The fake bin dir goes first; real
 * coreutils (bash, mkdir, tee, id, seq, sleep, grep, awk, sha256sum for the
 * non `-c` case…) still resolve from /usr/bin and /bin after it.
 */
export function fakeEnv(dir, extra = {}) {
  return {
    PATH: `${dir}:/usr/bin:/bin`,
    HOME: dir,
    ...extra,
  };
}

/** Runs a generated script's text against the fake engine, returns the result plus the call log. */
export function runScript(script, { engine = fakeEngineDir(), extraEnv = {} } = {}) {
  try {
    const r = spawnSync('bash', ['-c', script], {
      encoding: 'utf8',
      env: fakeEnv(engine.dir, extraEnv),
    });
    return { ...r, calls: engine.read() };
  } finally {
    engine.cleanup();
  }
}
