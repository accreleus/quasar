/**
 * Where Quasar is being deployed, and what that changes.
 *
 * This is not cosmetic. Unraid runs its root filesystem from a ramdisk, so
 * anything written to /etc is gone after a reboot: the sysctl and the uinput
 * module have to be persisted in /boot/config/go instead. It is also Slackware
 * rather than systemd, so `systemctl restart docker` does not exist there. A
 * quick start that ignored either would produce an install that works until the
 * machine is rebooted and then quietly streams badly.
 *
 * The systemd distributions differ from each other only in ways this wizard
 * does not touch (package managers, which we never invoke), so they share one
 * behaviour and differ only in their note.
 */

const SYSCTL_LINE = 'net.core.wmem_default=2097152';

/** Persist a sysctl the systemd way: a drop-in file, then reload. */
function systemdSysctl(sudo) {
  return `echo '${SYSCTL_LINE}' | ${sudo}tee /etc/sysctl.d/99-quasar.conf >/dev/null
${sudo}sysctl --system >/dev/null`;
}

/** Persist a module the systemd way. */
function systemdModule(sudo) {
  return `${sudo}modprobe uinput
echo uinput | ${sudo}tee /etc/modules-load.d/uinput.conf >/dev/null`;
}

/**
 * Unraid's /etc is a ramdisk. /boot is the USB stick and survives, and
 * /boot/config/go runs at boot, so both settings are appended there. Both
 * appends are guarded so re-running the script does not duplicate lines.
 */
function unraidSysctl() {
  return `sysctl -w ${SYSCTL_LINE} >/dev/null
# /etc is a ramdisk on Unraid, so persist through the boot script instead.
grep -q 'wmem_default' /boot/config/go || echo 'sysctl -w ${SYSCTL_LINE}' >> /boot/config/go`;
}

function unraidModule() {
  return `modprobe uinput
grep -q 'modprobe uinput' /boot/config/go || echo 'modprobe uinput' >> /boot/config/go`;
}

const SYSTEMD = {
  sudo: 'sudo ',
  dockerRestart: 'sudo systemctl restart docker',
  sysctl: () => systemdSysctl('sudo '),
  module: () => systemdModule('sudo '),
  defaultUid: 1000,
  defaultGid: 1000,
  defaultBasePath: '/var/lib/quasar',
  ownerLabel: 'uid 1000, the container default',
};

export const PLATFORMS = {
  fedora: {
    ...SYSTEMD,
    label: 'Fedora',
    note: 'Fedora is what the install is verified on. SELinux can stay enforcing.',
  },
  debian: {
    ...SYSTEMD,
    label: 'Debian or Ubuntu',
    note: 'Check that your Docker is Engine with Compose v2.20 or newer, not the older docker.io packages.',
  },
  arch: {
    ...SYSTEMD,
    label: 'Arch',
    note: '',
  },
  other: {
    ...SYSTEMD,
    label: 'Another systemd Linux',
    note: 'Any systemd distribution with Docker should work. If yours is not systemd, the persistence steps below will need adjusting.',
  },
  unraid: {
    sudo: '',
    dockerRestart: '/etc/rc.d/rc.docker restart',
    sysctl: unraidSysctl,
    module: unraidModule,
    defaultUid: 99,
    defaultGid: 100,
    defaultBasePath: '/mnt/user/appdata/quasar',
    ownerLabel: 'uid 99 and gid 100, the Unraid convention',
    label: 'Unraid',
    note:
      'Unraid runs / from a ramdisk, so the sysctl and the uinput module are persisted in /boot/config/go rather than /etc. It is not systemd either, so Docker restarts through /etc/rc.d/rc.docker. The script handles all three. Commands run without sudo because the Unraid shell is already root.',
  },
};

/** Never return undefined for an unknown id; the wizard is user-driven. */
export function platform(id) {
  return PLATFORMS[id] ?? PLATFORMS.fedora;
}
