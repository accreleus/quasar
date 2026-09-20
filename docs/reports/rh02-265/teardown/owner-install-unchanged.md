# The owner's existing install on the Unraid host is unchanged

Captured either side of this session's run D work (the agent-recreate identity check and
the teardown of the `quasar-rh02d` stack). The owner's install is the compose project
`deploy` on the Unraid host's own engine; it was stopped throughout and was never a target.

- Before: [`owner-install-before.txt`](owner-install-before.txt), 2026-09-20T13:52:02Z
- After: [`owner-install-after.txt`](owner-install-after.txt), 2026-09-20T14:04:45Z

## What was compared

For each of the four `deploy` containers: name, state, status, image, creation time, the
full container id, the image id, and `StartedAt` / `FinishedAt` / `RestartCount` /
`ExitCode`. For each of the five `deploy` volumes: creation time, mountpoint, file count,
byte size, and a sha256 over the sorted `path size` listing of every file in it — including
the 1.56 GB NVIDIA driver volume. Plus the `deploy_default` network's id and creation time,
and the shared host path `/run/quasar-agent`.

## Result

`diff` over the two captures reports exactly two lines, and neither is a change to the
install:

1. **The capture timestamp**, 13:52:02Z against 14:04:45Z. That is the header this script
   prints, not a property of the install.
2. **The mtime of the directory `/run/quasar-agent`**, `Sep 20 22:43` against
   `Sep 20 23:53` (host local time). This host path is shared between every install on the
   engine, which is the whole point of finding #279. Run D's agent created and then removed
   its own entry under it during the recreate and the teardown, which moves the parent
   directory's mtime. **The directory is empty in both captures** — `total 0`, `.` and `..`
   only — and its owner and mode are unchanged (`drwx------ root root`). Nothing of the
   owner's was in it before and nothing is in it now.

Everything else is identical, character for character:

- All four containers still `exited`, with the same container ids, the same image ids, the
  same `FinishedAt` of 2026-09-15T10:35:35Z, `RestartCount` 0 and the same exit codes
  (143 for the node agent, 0 for the other three). They were not started, not recreated and
  not restarted.
- All five volumes have the same creation time, the same file count, the same byte size and
  the same listing digest. In particular `deploy_quasar-postgres-data` is unchanged
  (1585 files, 74840326 bytes, `77f64c4a…`), so the owner's database was not written to,
  and `deploy_quasar-nvidia-driver` is unchanged (85 files, 1559854538 bytes, `b233d3ff…`).
- `deploy_default` has the same network id and creation time.

## Scope note

Run D's own stack directory, the clone and its managed homes, is still on disk under the
appdata path it was installed to (88 MB). It is not a container, a volume or a network, and
it holds the managed home whose marker file is the evidence for the agent-recreate check, so
it was deliberately left in place rather than deleted. Removing it is a one-line `rm -rf` the
owner can run whenever they want the disk back.
