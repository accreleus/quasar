### Fixed

- **Release detection follows the one redirect GitHub and GHCR answer with (#110, #111).**
  The stable channel downloaded `platform-release-manifest.json` from the GitHub Release
  and got a `302` to `release-assets.githubusercontent.com`; the edge channel read an
  image config from GHCR and got a `307` to `pkg-containers.githubusercontent.com`. The
  hardened outbound client refuses redirects, so stable stored nothing and edge failed.
  Both now take exactly one validated hop through a shared helper (https only, redirect
  host allowlisted, no `Authorization` on the hop); `release-assets.githubusercontent.com`
  joins the default `QUASAR_PLATFORM_RELEASE_ASSET_HOSTS`, and `ghcr.io` implies its blob
  host. Found live against the first published prerelease.
- **The compose file forwards the release-detection knobs to the control plane (#110).**
  `QUASAR_PLATFORM_RELEASE_REPO/_API/_ASSET_HOSTS/_TOKEN/_DETECT_INTERVAL`,
  `QUASAR_PLATFORM_REGISTRY` and `QUASAR_IMAGE_REGISTRY_HOSTS` set in `deploy/.env` now
  reach the process on a registry install; the public site's stack template matches. An
  empty `QUASAR_PLATFORM_RELEASE_REPO` means the default; `off`/`none`/`disabled` turns
  detection off.
- **The "Quasar was updated" toast is dismissible and only appears when a reload would
  fetch a different bundle (#117).** It compared the bundle's baked commit with the
  control plane's, which differ on any source-built stack whose web bundle and control
  plane were built separately, so it never went away. It now compares the served
  `index.html`'s bundle hash with the loaded one, has a close control, and remembers a
  dismissal per bundle.
- **Update Quasar is disabled when the control plane is not eligible (#117)**, with the
  reason in its tooltip, instead of answering a `409` after the click.
- **A source-built control plane is never offered a registry image (#117)**, and both
  control-plane images run as uid 1000 so a volume created by either is usable by the
  other; `redeploy.sh` fixes the TLS volume's ownership once (#115).
- **The leak scan no longer treats Unraid's generic appdata path as an operator
  fingerprint**; every push had been red on the tracker scan since an install-help
  comment used it.


## Install or upgrade

Pins for `deploy/.env` (the two digests are the ones `platform-release-manifest.json` names; the updater follows the version tag):

```
QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@sha256:7ebd29292e9cf26c7f27438848230f00a3777637b154feaeb8c8b606ded38797
QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@sha256:4e2651d455d83024624ebd012e602835e3eb18d9be5ec2d916c89c372dc11ecb
QUASAR_UPDATER_IMAGE=ghcr.io/accreleus/quasar/quasar-updater:0.2.0-rc.2
QUASAR_STACK_DIR=<absolute host path of your deploy directory>
```

Then, with the same `-f` list you installed with: `docker compose -f deploy/docker-compose.yml pull && docker compose -f deploy/docker-compose.yml up -d --force-recreate --no-deps quasar-control-plane quasar-updater quasar-node-agent` — or, on an instance that already runs the updater, apply it from Admin ▸ Fleet ▸ Releases. New install: https://accreleus.github.io/quasar/install/install/ · Upgrading: https://accreleus.github.io/quasar/operations/upgrading/

