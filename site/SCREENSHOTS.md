# Screenshots

Every `<Shot>` in the site now carries a real image. This file records where
they came from, how to retake them, and what is still worth adding.

## What is in the site

| Page | Asset | What it shows |
| --- | --- | --- |
| `index.mdx` (landing) | `library-home.png` | The library home, hero rail and cover tiles. |
| `install/first-run.mdx` | `setup-claim.png` | Step 1 of the wizard on an unclaimed instance. |
| `install/first-run.mdx` | `setup-hosts.png` | Step 3, the host and GPU check, with a pass and a failure. |
| `playing/library.mdx` | `library.png` | The full library, grouped by source. |
| `playing/in-session.mdx` | `session-drawer.png` | A live session with the menu open on Controller and input. |
| `admin/overview.mdx` | `admin-overview.png` | Live sessions, needs attention, fleet capacity, recent activity. |
| `admin/hosts.mdx` | `admin-hosts.png` | The fleet host list with the row expanded. |
| `admin/sessions.mdx` | `admin-sessions.png` | Live and recent sessions. |
| `admin/images.mdx` | `admin-images.png` | The image catalog and per-host rollout state. |
| `admin/apps.mdx` | `admin-apps.png` | The app catalog. |
| `admin/steam.mdx` | `admin-sources.png` | The Steam source and the artwork provider. |
| `admin/profiles.mdx` | `admin-profiles.png` | Launch profiles and their ordered rungs. |
| `admin/users.mdx` | `admin-invites.png` | Registration mode and outstanding invites. |

`start/how-it-works.mdx` is not a screenshot. It renders
`src/components/ArchDiagram.astro`, an enlarged version of the landing page's
inline diagram drawn against the product tokens, so it stays sharp at any size
and follows the theme toggle.

## Capture settings

- Chrome for Testing, 1440x900 viewport, 2x device pixel ratio, dark theme,
  reduced motion.
- Cropped to the browser content area. No OS chrome, no browser chrome.
- Viewport-cropped, except `setup-hosts.png`, where the wizard card is taller
  than the viewport and a crop would cut the failing check off.
- Seeded demo data throughout: invented game titles with generated cover art,
  demo accounts (`jordan`, `sam`, `riley`), and seeded invites. No real user
  names or email addresses appear.
- Invite codes in `admin-invites.png` are the 8-character prefixes the UI
  itself shows. The full code is never retrievable after minting, so there is
  nothing to redact.

## How they were taken

Against a live single-host deployment, not a mock. The scripts that drove it
are throwaway and are not committed; the shape of the run was:

1. Seed a demo catalog through the admin API, uploading generated 2:3 cover art
   to `POST /v1/admin/apps/{id}/artwork/upload?crop=tile`.
2. Register demo users, mint a few invites, and run short sessions under
   different accounts so the sessions list and the home rail have real history.
3. Launch a KDE Desktop session and drive it with Chrome for Testing so the
   in-session shots show a genuinely decoding stream.
4. Photograph the admin surfaces while that session is live.

The first-run wizard needs an instance nobody has claimed, so those two shots
came from a separate throwaway stack on its own compose project, ports and
database, running the same control-plane image with a node agent pointed at it.

## Still worth doing

- **A second host.** `admin-hosts.png` shows a single-host fleet, which leaves
  the page looking emptier than it does in a real deployment. Two hosts, ideally
  one NVIDIA and one AMD, would make the point the page is making.
- **A populated Steam scan.** `admin-sources.png` shows the Steam source with
  its scan controls but nothing discovered, because discovery needs a Steam
  container that has been signed into and has games installed.
- **Light mode.** Every shot is dark. The site defaults to dark and the theme
  toggle works, so light variants can wait.
- Session detail with the metrics time series, on `admin/sessions.mdx`.
- A diagnostic bundle verdict, on `operations/diagnostics.mdx`.
- The account Stream quality page, on `playing/storage.mdx`.
- The in-session performance overlay, on `troubleshooting/quality.mdx`.

## Open question: the prose does not match the navigation

The screenshots are of the current web UI, whose admin navigation is grouped:
Overview, Sessions, Fleet, Library, Streaming, People, Audit log, Settings.
The prose across the site still refers to the older flat navigation, so a
reader comparing the two will find that several of these paths no longer exist
as written:

| Prose says | The UI now has |
| --- | --- |
| Admin, Hosts | Admin, Fleet, Hosts |
| Admin, Storage | Admin, Fleet, Storage |
| Admin, Jobs | Admin, Fleet, Jobs |
| Admin, Apps | Admin, Library, Apps |
| Admin, Images | Admin, Library, Images |
| Admin, Steam | Admin, Library, Sources |
| Admin, Users | Admin, People, Users |
| Admin, Invites | Admin, People, Invites |
| Admin, Audit log | Admin, Audit log (unchanged) |
| Admin, Sessions | Admin, Sessions (unchanged) |
| Admin, Settings | Admin, Settings (unchanged) |

Affected files: `admin/{apps,hosts,images,sessions,steam,users,jobs}.mdx`,
`troubleshooting/{launching,connecting,quality,av}.mdx`,
`operations/{diagnostics,uninstall,upgrading}.mdx`.

This was left alone deliberately. Renaming navigation throughout the site is a
copy decision rather than a screenshot one, and it depends on whether the site
should describe the UI on `feat/ui-v3` before that branch reaches `main`.
