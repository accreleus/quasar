/**
 * A fake container-engine PATH for exercising generated install scripts in tests.
 *
 * SAFETY. A generated script's own preflight only ever calls `docker`/`podman`
 * through PATH, so a fake `docker`/`podman` placed ahead of the real ones on
 * PATH used to look like enough isolation. It is not: the rootful scripts run
 * `sudo …`, and `sudo` does not honour the PATH we hand a child process — it
 * re-execs through its own `secure_path`/PAM environment, which can still find
 * a REAL `docker` or `podman` on the machine running the test. On this host
 * that is exactly what happened once: a native `npm test` run left a real
 * Quasar install behind.
 *
 * So this harness:
 *   - never inherits the calling process's environment (no stray PATH, no
 *     inherited docker/podman env vars) — every test env is built from
 *     scratch by `fakeEnv()`;
 *   - intercepts `sudo` itself (never the real setuid binary): the shim logs
 *     the call and re-execs its argument directly, unprivileged, through the
 *     same fake PATH, so a `sudo docker …` or `sudo podman …` still lands on
 *     the fakes below, not a real engine;
 *   - intercepts `systemctl` (Quadlet's `daemon-reload`/`start`, and the
 *     podman-restart.service check) so nothing here ever asks a real init
 *     system anything; `restartOff` makes `is-enabled` answer "disabled";
 *   - intercepts `systemd-tmpfiles`, whose `--cat-config` lists the agent's
 *     runtime directory unless `runDirOff`;
 *   - intercepts `curl`, which never contacts the network;
 *   - answers `podman info --format '{{.Version.Version}}'` with `podmanVersion`.
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
export function fakeEngineDir({ legacy = false, existing = false, portTaken = false, restartOff = false, runDirOff = false, podmanVersion = '5.8.4' } = {}) {
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
  inspect) echo healthy ;;
esac`);

  shim(dir, 'podman', `echo "podman $*" >> ${JSON.stringify(log)}
case "$1" in
  info) case "$*" in *Version.Version*) echo ${JSON.stringify(podmanVersion)} ;; esac; exit 0 ;;
  container) ${existing ? 'exit 0' : 'exit 1'} ;;
  pull) exit 0 ;;
  image) ref="\${@: -1}"; echo "\${ref%:*}@sha256:0123abcd" ;;
  secret) case "$2" in exists) exit ${existing ? 0 : 1} ;; create) echo fakesecret ;; esac ;;
  exec) exit 0 ;;
  inspect) echo healthy ;;
esac`);

  // Never the real, setuid `sudo`: strip the name and run the rest directly,
  // unprivileged, through this same fake PATH.
  shim(dir, 'sudo', `echo "sudo $*" >> ${JSON.stringify(log)}
exec "$@"`);

  shim(dir, 'systemctl', `echo "systemctl $*" >> ${JSON.stringify(log)}
case "$*" in
  *is-enabled*podman-restart*) exit ${restartOff ? 1 : 0} ;;
esac
exit 0`);

  shim(dir, 'systemd-tmpfiles', `echo "systemd-tmpfiles $*" >> ${JSON.stringify(log)}
${runDirOff ? '' : "echo 'd /run/quasar-agent 0755 root root -'"}
exit 0`);

  shim(dir, 'curl', `echo "curl $*" >> ${JSON.stringify(log)}
# A control plane answers /health only once this script has started the seed:
# before that the port is free (the scripts refuse to start on a taken port).
case "$*" in
  */health*) ${portTaken ? ':' : `grep -q -e 'run -d --name quasar-seed' -e 'start quasar-seed' ${JSON.stringify(log)} || exit 7`} ;;
esac
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
 * coreutils (bash, mkdir, tee, id, seq, sleep, grep, awk…) still resolve from
 * /usr/bin and /bin after it.
 */
export function fakeEnv(dir, extra = {}) {
  return {
    PATH: `${dir}:/usr/bin:/bin`,
    HOME: dir,
    // The rootful Podman script writes its Quadlet unit here, never /etc.
    QUASAR_QUADLET_DIR: `${dir}/quadlet`,
    ...extra,
  };
}

/**
 * Runs a generated script's text against the fake engine, returns the result
 * plus the call log. `cwd` is the fake bin dir itself, so anything a script
 * writes to a relative path lands there and is cleaned up with it, never in
 * the site's own working tree.
 */
export function runScript(script, { engine = fakeEngineDir(), extraEnv = {} } = {}) {
  try {
    const r = spawnSync('bash', ['-c', script], {
      encoding: 'utf8',
      cwd: engine.dir,
      env: fakeEnv(engine.dir, extraEnv),
    });
    return { ...r, calls: engine.read() };
  } finally {
    engine.cleanup();
  }
}
