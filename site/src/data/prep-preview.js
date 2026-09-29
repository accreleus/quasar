/**
 * An illustrative preview of what `deploy/prepare-host.sh` prints, in its own
 * wording, for the "What it changes" disclosure in the quick start's Result
 * step (RH07-14, #406). This is NOT live output — a static site cannot run a
 * script on the reader's machine — so it is captioned as an example in the
 * component and never claims to be what a given machine will actually see
 * (GPU vendor, existing state and prior runs all change the real transcript).
 * The wording mirrors the script's own `say()` lines (deploy/prepare-host.sh)
 * closely enough to be recognisable when the reader runs it for real.
 */
import { homePath } from './stack-template.js';

function row(state, item, why) {
  return `  ${state.padEnd(8)} ${item}${why ? ` — ${why}` : ''}`;
}

/**
 * @param {object} a wizard answers (engine, mode, console)
 * @param {{rerun?: boolean}} [opts] rerun=true shows the "nothing left to
 *   change" transcript: every line reads `ok`, with no explanation.
 */
export function prepPreviewLines(a, { rerun = false } = {}) {
  const rootless = a.mode === 'rootless';
  const podman = a.engine === 'podman';
  const state = rerun ? 'ok' : 'changed';
  const why = (text) => (rerun ? undefined : text);

  const lines = [`Quasar host preparation (${a.mode})`, `  note     engine: ${a.engine}`];

  if (rootless) {
    lines.push(row(state, 'account quasar', why('a rootless install runs everything under this one unprivileged account')));
    lines.push(row(state, '/etc/subuid', why("subordinate uid range for quasar: the containers' own users map into it")));
    lines.push(row(state, '/etc/subgid', why("subordinate gid range for quasar: the containers' own users map into it")));
    lines.push(row(state, 'lingering for quasar', why('its engine and Quasar keep running with nobody logged in, and start at boot')));
  } else {
    lines.push(row(state, 'group quasar', why("owns the device access Quasar's containers use, and nothing else")));
  }

  lines.push(row(state, '/etc/udev/rules.d/70-quasar.rules', why('give the quasar group the devices Quasar uses, and only those')));
  lines.push(row(state, '/etc/modules-load.d/quasar.conf', why(`load uinput${a.console ? ' i2c-dev' : ''} at boot`)));
  lines.push(row('ok', 'kernel module uinput loaded'));
  if (a.console) lines.push(row('ok', 'kernel module i2c-dev loaded'));
  lines.push(row(state, '/etc/sysctl.d/99-quasar.conf', why('kernel settings Quasar needs, applied at every boot')));
  lines.push(row(state, 'net.core.wmem_default=2097152', rerun ? '(running kernel)' : 'now — it was 212992'));
  lines.push(row('note', 'GPU fault messages stay hidden from Quasar; --allow-kernel-log enables that optional diagnostic'));

  if (podman) {
    lines.push(row(state, 'podman-restart.service enabled for quasar', why("Podman has no daemon, so this is what starts Quasar's containers at boot")));
    lines.push(row(state, 'podman.socket enabled for quasar', why('the engine socket the Quasar seed and recovery actor talk to')));
  } else if (!rootless) {
    lines.push(row(state, 'docker.service enabled', why("the daemon, and with it Quasar's containers, start at boot")));
  } else {
    lines.push(row('note', 'rootless Docker: install it for quasar with dockerd-rootless-setuptool.sh and enable its docker.service; lingering (above) keeps it running'));
  }

  if (rootless) lines.push(row(state, `homes root ${homePath(a)}`, why("where each user's game saves and settings live")));

  lines.push('Host preparation is complete. Run it again at any time: it changes only what is missing.');
  return lines.join('\n');
}
