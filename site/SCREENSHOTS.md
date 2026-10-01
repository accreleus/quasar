# Screenshots

Every `<Shot>` in the site carries a real image. This file records where they came
from, how to retake them, and what is still worth adding.

## What is in the site

| Page | Asset | What it shows |
| --- | --- | --- |
| `index.mdx` (landing) | `library-home.png` | The library home: the rail of recent and newly added apps, then the tiles. |
| `start/quickstart.mdx` | `quickstart-engine.png` | The quick start's engine step for Fedora: rootful Docker and Podman with their profile badges, and the pointer to rootless. Retaken 2026-10-01. |
| `install/first-run.mdx` | `setup-claim.png` | Step 1 of the wizard on an unclaimed instance. |
| `install/first-run.mdx` | `setup-hosts.png` | Step 3, the host check, cropped to a host whose input checks fail and block launches. |
| `playing/library.mdx` | `library.png` | The full library, grouped by source. |
| `playing/in-session.mdx` | `session-drawer.png` | A live session, menu open on Controller and input, microphone on with its indicator. |
| `playing/in-session.mdx` | `session-stats.png` | The Performance stats pane of a live session. |
| `admin/overview.mdx` | `admin-overview.png` | Live sessions, needs attention, fleet capacity, recent activity. |
| `admin/hosts.mdx` | `admin-hosts.png` | The fleet host list, one host expanded. |
| `admin/console.mdx` | `admin-console.png` | The Local console page of a host with console mode on and PipeWire audio. |
| `troubleshooting/readiness.mdx` | `host-readiness.png` | Part of a rootless Docker host's readiness card: storage, network, engine and console checks. |
| `admin/sessions.mdx` | `admin-sessions.png` | Live and recent sessions. |
| `admin/images.mdx` | `admin-images.png` | The image catalog and per-host rollout state. |
| `admin/apps.mdx` | `admin-apps.png` | The app catalog. |
| `admin/steam.mdx` | `admin-sources.png` | The Steam source and the artwork provider. Not retaken in the 2026-09-30 refresh; see below. |
| `admin/profiles.mdx` | `admin-profiles.png` | Launch profiles and their ordered rungs. |
| `admin/users.mdx` | `admin-invites.png` | Registration mode and outstanding invites. |
| `admin/releases.mdx` | `admin-releases.png` | The Releases tab on the edge channel, on a control plane built from source. |

`start/how-it-works.mdx` is not a screenshot. It renders
`src/components/ArchDiagram.astro`, an enlarged version of the landing page's
inline diagram drawn against the product tokens, so it stays sharp at any size
and follows the theme toggle.

## Capture settings

- Chrome, 1440x900 viewport, 2x device pixel ratio, dark theme, reduced motion.
- Cropped to the browser content area. No OS chrome, no browser chrome.
- Viewport-cropped, except `setup-hosts.png`, cropped from the full wizard page
  to the failing host, and `quickstart-engine.png`, the quick start element alone.
- Quantized with `pngquant` (quality 80 to 95; 55 to 90 for the three shots full
  of game art) to keep the repository small. Astro converts them at build time.
- Dummy data throughout: fictional hosts (`gpu-host-01`, `gpu-host-02`,
  `living-room`, `den-pc`), demo accounts (`morgan`, `jordan`, `sam`, `riley`,
  `casey`) with `example.com` addresses, and seeded invites. No real host, address
  or person appears. Invite codes are the 8-character prefixes the UI itself
  shows; the full code is never retrievable after minting.
- Library art is SteamGridDB community art for real games, credited per image in
  `src/assets/shots/demo-art/CREDITS.md`, where the files themselves are kept.
  Nothing from Nintendo. The Desktop app keeps the default gradient tile.

## How they were taken (2026-09-30)

Against a local demo stack, not a deployment. The scripts that drove it were
throwaway and are not committed; the shape of the run was:

1. `make up` for a local control plane and Postgres (the agentless local
   stack), with the web client built into `web/dist`.
2. Three scripted node agents, written for the run, registered over the real
   agent WebSocket with enrollment tokens. Each reported fictional hardware,
   engine facts (Podman rootless, Docker rootful, Docker rootless), a readiness
   card and, for `living-room`, console capabilities with PipeWire sinks. The
   readiness wording follows the agent's own strings.
3. Seeded through the admin API: settings, 14 games plus a desktop, artwork
   uploaded with `POST /v1/admin/apps/{id}/artwork/upload?crop=tile|hero`, four
   users registered with invites, three more invites, the image catalog synced,
   console mode set on `living-room`.
4. Sessions launched and stopped through the API for history, and a few left
   running for the admin pages.
5. The in-session shots come from a real session launched from the library in
   Chrome. The scripted agent answered WebRTC signaling and streamed a looping
   H.264 pan across the game's hero art over a real peer connection, with an
   audio connection that received the browser's (fake) microphone. The picture is
   that art, not a game running.
6. The first-run shots came from a second, unclaimed control plane on its own
   compose project and database, with two scripted agents against it.
7. The quick start shot is the built documentation site itself.

## Still worth doing

- **A populated Steam scan, and the Sources tab retaken.** Discovery needs a Steam
  container that has been signed into and has games installed, and the scripted
  agents cannot report Steam preparation, so `admin-sources.png` is still the
  earlier capture.
- **A release to show.** `admin-releases.png` is a control plane built from
  source, which is never offered a release, so the Available section is empty
  and the control-plane target reads not ready. An owned install with a release
  newer than it would show the page as an operator sees it.
- **Light mode.** Every shot is dark.
- Session detail with the metrics time series, on `admin/sessions.mdx`.
- A diagnostic bundle verdict, on `operations/diagnostics.mdx`.
- The account Stream quality page, on `playing/storage.mdx`.
