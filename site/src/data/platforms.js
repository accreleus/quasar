/**
 * Where Quasar is being deployed, and what that changes.
 *
 * This is not cosmetic. Unraid runs its root filesystem from a ramdisk and is
 * already root at its shell, so it gets its own script (no sudo, no host steps)
 * and its own paths and save-data owner. The systemd distributions differ from
 * each other only in ways this wizard does not touch (package managers, which we
 * never invoke), so they share one behaviour and differ only in their note.
 */

const SYSTEMD = {
  sudo: 'sudo ',
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
  ubuntu: {
    ...SYSTEMD,
    label: 'Ubuntu 24.04',
    note: 'Use Docker Engine from Docker\'s own repository, or Podman from Ubuntu\'s. The older docker.io packages may predate what Quasar needs.',
  },
  debian: {
    ...SYSTEMD,
    label: 'Debian',
    note: 'Use Docker Engine from Docker\'s own repository. The Debian-packaged docker.io may predate what Quasar needs.',
  },
  arch: {
    ...SYSTEMD,
    label: 'Arch',
    note: '',
  },
  other: {
    ...SYSTEMD,
    label: 'Another systemd Linux',
    note: 'Any systemd distribution with Docker or Podman should work. If yours is not systemd, the host steps below will need adjusting.',
  },
  unraid: {
    sudo: '',
    defaultUid: 99,
    defaultGid: 100,
    defaultBasePath: '/mnt/cache/appdata/quasar',
    ownerLabel: 'uid 99 and gid 100, the Unraid convention',
    label: 'Unraid',
    note:
      'Commands run without sudo because the Unraid shell is already root. Unraid runs / from a ramdisk: in Dockge, keep the stacks directory under /mnt/user/appdata so the stack survives a reboot.',
  },
};

/** Never return undefined for an unknown id; the wizard is user-driven. */
export function platform(id) {
  return PLATFORMS[id] ?? PLATFORMS.fedora;
}
