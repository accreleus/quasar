# Screenshots

Every `<Shot>` in the site carries a real image. This file records where they came
from, how to retake them, and what is still worth adding.

## What is in the site

| Page | Asset | What it shows |
| --- | --- | --- |
| `index.mdx` (landing) | `library-home.png` | The library home: the rail of recent and newly added apps, then the tiles. |
| `install/first-run.mdx` | `setup-claim.png` | Step 1 of the wizard on an unclaimed instance. |
| `playing/library.mdx` | `library.png` | The full library, grouped by source. |
| `playing/in-session.mdx` | `session-drawer.png` | A live session, menu open on Controller and input, microphone on with its indicator. |
| `playing/in-session.mdx` | `session-stats.png` | The Performance stats pane of a live session. |
| `admin/overview.mdx` | `admin-overview.png` | Live sessions, needs attention, fleet capacity, recent activity. |
| `admin/hosts.mdx` | `admin-hosts.png` | The fleet host list, one host expanded. |
| `admin/console.mdx` | `admin-console.png` | The Local console page of a host with console mode on (direct-display settings, #455). |
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
- Cropped to the browser content area and the viewport. No OS chrome, no
  browser chrome.
- Quantized with `pngquant` (quality 80 to 95; 55 to 90 for the three shots full
  of game art) to keep the repository small. Astro converts them at build time.
- Dummy data throughout: fictional hosts (`gpu-host-01`, `gpu-host-02`,
  `living-room`, `den-pc`), demo accounts (`morgan`, `jordan`, `sam`, `riley`,
  `casey`) with `example.com` addresses, and seeded invites. No real host, address
  or person appears. Invite codes are the 8-character prefixes the UI itself
  shows; the full code is never retrievable after minting.
- Library art is SteamGridDB community art for real games, credited per image in
  "Game art in the screenshots" below. Nothing from Nintendo. The Desktop app
  keeps the default gradient tile.

## Game art in the screenshots

The library tiles and heroes in the screenshots are resized (heroes also
centre-cropped) copies of community uploads on [SteamGridDB](https://www.steamgriddb.com/),
used only as illustrative demo art. Credit to SteamGridDB and to each uploader
below. The game names, logos and artwork belong to their owners, the games'
developers and publishers; their appearance implies no affiliation with or
endorsement of Quasar. No image is covered by an open licence, so the source
files are not kept in this repository. The picture in the in-session shots is a
slow pan across the Forza Horizon 5 hero, streamed as video.

- Forza Horizon 5: grid [162633](https://www.steamgriddb.com/grid/162633) by berry, hero [42805](https://www.steamgriddb.com/hero/42805) by Yaestro
- Hades: grid [63955](https://www.steamgriddb.com/grid/63955) by thomwatson, hero [35720](https://www.steamgriddb.com/hero/35720) by ABH20
- Celeste: grid [40963](https://www.steamgriddb.com/grid/40963) by Gums, hero [25973](https://www.steamgriddb.com/hero/25973) by Greez
- Portal 2: grid [87946](https://www.steamgriddb.com/grid/87946) by MustafaMert, hero [552](https://www.steamgriddb.com/hero/552) by edco0328
- Sid Meier's Civilization VI: grid [69705](https://www.steamgriddb.com/grid/69705) by qkzn, hero [6351](https://www.steamgriddb.com/hero/6351) by klepp0906
- Stardew Valley: grid [89067](https://www.steamgriddb.com/grid/89067) by Jinx, hero [16046](https://www.steamgriddb.com/hero/16046) by Bun
- Cyberpunk 2077: grid [121395](https://www.steamgriddb.com/grid/121395) by CluckenDip, hero [97318](https://www.steamgriddb.com/hero/97318) by CluckenDip
- DOOM Eternal: grid [102232](https://www.steamgriddb.com/grid/102232) by HarrinorX, hero [9237](https://www.steamgriddb.com/hero/9237) by NightSkye
- Hollow Knight: grid [81639](https://www.steamgriddb.com/grid/81639) by anidais, hero [24916](https://www.steamgriddb.com/hero/24916) by mdante_ar
- Cities: Skylines: grid [36070](https://www.steamgriddb.com/grid/36070) by CaptainCero, hero [42020](https://www.steamgriddb.com/hero/42020) by CluckenDip
- Elden Ring: grid [744598](https://www.steamgriddb.com/grid/744598) by IamGlitch, hero [39097](https://www.steamgriddb.com/hero/39097) by CluckenDip
- Rocket League: grid [87950](https://www.steamgriddb.com/grid/87950) by Olympian, hero [12763](https://www.steamgriddb.com/hero/12763) by Olympian
- Subnautica: grid [77101](https://www.steamgriddb.com/grid/77101) by DBK, hero [5723](https://www.steamgriddb.com/hero/5723) by klepp0906
- Baldur's Gate 3: grid [120739](https://www.steamgriddb.com/grid/120739) by Soop, hero [129727](https://www.steamgriddb.com/hero/129727) by Elendil

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
