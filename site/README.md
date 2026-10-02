# Quasar documentation site

The public site and user documentation for Quasar. Astro + Starlight, deployed to
GitHub Pages.

## Run it locally

```bash
cd site
npm ci
npm run dev
```

Then open <http://localhost:4321/quasar/>. Note the `/quasar/` path. The site is
built as a GitHub Pages project site, so it lives under a base path even in
development.

```bash
npm run build
```

```bash
npm run preview
```

`build` writes `dist/`. `preview` serves `dist/` the way it will be served in
production.

## Where things are

```
site/
  astro.config.mjs          site + base URL, sidebar, theme, expressive-code
  src/
    content/docs/           every documentation page, as .mdx
    components/
      Landing.astro         the landing page, sections and its own CSS
      ArchDiagram.astro     the architecture diagram on "How it works"
      QuickStart.astro      the quick-start install generator
      Placeholder.astro     the screenshot placeholder panel
      Shot.astro            figure wrapper used in docs pages
      VersionSelect.astro   the stable / edge switcher in the header
      SocialIcons.astro     Starlight slot override that renders the switcher
    data/
      stack-template.js     what the quick start generates: the seed stack and script
      stack-template.test.js  its tests (npm test)
      platforms.js          per-platform defaults (paths, owner, sudo) for the script
      proxy-configs.js      the reverse-proxy snippets
      write-fixtures.mjs    writes every generated script out for shellcheck
      docs-version.js       which version a build is (stable / edge), from the environment
      base-links.js         moves content links under the build's base (edge)
    route-data.ts           corrects the titles starlight-openapi generates
    styles/theme.css        Starlight variable overrides, product tokens
    assets/                 the brand mark and the screenshots
  scripts/check-versions.mjs  checks links in the assembled two-version artifact
  SCREENSHOTS.md            checklist of screenshots still to capture
```

The quick start configures an all-in-one GPU server with a managed database.
It shows Docker Compose YAML or a Podman Quadlet with Copy and Download, early
GPU preparation and storage ownership. The generator also supports advanced
roles used by the installation reference pages. The instance-generated enrollment
command remains the primary way to add a GPU host.

This local preview uses the generator from #440 commit
`5c5f2d94e9fa3c858bf33db25ffbc7516c451589` on `fix/440-latest-bootstrap`.
The preview's backend base remains `develop` at `4b3e9c7d`: no backend code was
merged. Configuration uses channel tags; the fix resolves them internally to
digests on first install and preserves installed state on reruns. The preview
carries a visible dependency notice until compatible published images are verified.

## Local review scope (2026-10-01)

The documentation pass preserves developer documentation and runtime contracts.
In addition to the reviewed quick start, Requirements, NVIDIA/Unraid and Getting
started pages, user docs now use shorter task steps and shared references:
admin overview, Steam, images and jobs; browser/codec compatibility; encoder,
bitrate and audio settings; diagnostics and bounded logs; direct networking,
proxy examples and troubleshooting. Advanced tuning and backup/recovery detail
remain available. Container engines now lives in Requirements with an old-URL
redirect. Archify v3.0 authoring guidance informed the embedded diagram; its CLI
and JSON validation workflow were not used.

Local site tests, builds and link checks are the applicable validation. Full
`make verify` also ran but failed in existing DX tests (Bash parse error,
host-contract fixtures, missing local session tooling and sandbox TCP fixtures).
No streaming/hardware tests or deployment were performed.

## Before publishing

The site describes installs owned by Quasar (the seed and the recovery actor) on
the **stable** channel. The quick start and Install Quasar resolve each image's
`latest` tag, which `images.yml` moves onto the newest stable release (or `main`).
The `pages` workflow builds the stable docs from the latest release tag itself
(see Deployment), so dispatch it **after** the release's Images run has succeeded,
and not before: until then `latest` names a
build from before owned installs, which the seed refuses. The published site must
never describe an install the published images cannot make. To preview the
quick start against edge builds, build with `QUASAR_IMAGE_TAG=o2-develop`.

- **Every unfinished part carries a hidden marker**, an MDX comment such as
  `{/* TODO(#361): … */}`, or `TODO(open, …)` for a question no ticket owns yet.
  They render nothing, so the build cannot tell a draft from a finished page.

The `pages` workflow enforces this for the stable docs: it fails, listing every
marker, while any `TODO(#` or `TODO(open` remains under `src/content` or
`src/components` of the release tag it builds. The edge docs are `develop` as it
is, so there the markers are listed as a warning. The CI build does not check, so
drafts can land on branches. To see what is left:

```bash
grep -rnE 'TODO\((#|open)' site/src/content site/src/components
```

## Deployment

`.github/workflows/pages.yml` publishes two versions of the docs in one GitHub
Pages artifact:

| Version | URL | Built from |
|---|---|---|
| Stable | `/quasar/` | the latest release tag (newest non-draft, non-prerelease release, looked up at run time) |
| Edge | `/quasar/edge/` | the tip of `develop` |

It checks out both trees, builds each with its own `site/` (packages, config and
all), copies the edge build into `edge/` of the stable one, checks the links with
`scripts/check-versions.mjs`, and uploads the result. Nothing is hard-coded to a
version: cutting a release and dispatching `pages` again moves stable.

Each build learns which version it is from the environment, read by
`src/data/docs-version.js`:

| Variable | Meaning |
|---|---|
| `QUASAR_DOCS_CHANNEL` | `stable` or `edge`. Unset (a local build) means stable at `/quasar/` with no switcher. |
| `QUASAR_DOCS_BASE` | the base path, when not the channel's default (`/quasar`, `/quasar/edge`). |
| `QUASAR_DOCS_STABLE_LABEL` | the stable release, e.g. `v0.3.0`, shown in the switcher. |

With a channel set, the header carries a version switcher (Starlight's own select,
`src/components/VersionSelect.astro`, in the `SocialIcons` slot) offering
"Stable (vX.Y.Z)" and "Edge (develop)". Switching keeps the page if it exists in
the other version and otherwise opens that version's home. Edge pages carry
`<meta name="robots" content="noindex">` so searches land on stable, and their
"Edit page" link points at `develop`.

**A release tag cut before the switcher existed (v0.3.0 and earlier)** ignores
these variables: it builds at `/quasar/` from its own config, exactly as it was
published, with no switcher. Edge still links to it. The switcher appears on
stable from the first release that contains it.

Content links are written against the published root, `[Podman](/quasar/install/podman/)`.
`src/data/base-links.js` rewrites them to the build's base while rendering, so
edge pages link within edge, and the redirects in `astro.config.mjs` go through
the same rewrite. Components are not Markdown: build a link from
`import.meta.env.BASE_URL`, never a literal `/quasar/` (a test refuses that).

To build and check both versions locally (Pages serves the artifact at `/quasar/`):

```bash
cd site
QUASAR_DOCS_CHANNEL=edge npm run build && mv dist /tmp/edge
QUASAR_DOCS_CHANNEL=stable npm run build
mkdir -p /tmp/pages && cp -a dist /tmp/pages/quasar && cp -a /tmp/edge /tmp/pages/quasar/edge
node scripts/check-versions.mjs /tmp/pages/quasar
cd /tmp/pages && python3 -m http.server 8080   # then open http://localhost:8080/quasar/
```

Pages must be enabled on the repository with source "GitHub Actions" before the
first run.

The workflow is `workflow_dispatch` only, which is this project's convention for
every workflow except `leak-scan`. A Pages deploy triggered on push to `main`
would be the natural trigger for a docs site and should be a deliberate decision
rather than an accident. Manual dispatch also fits the "`main` is production"
rule reasonably well, since publishing then stays an explicit act.

The build reads `protocol/openapi.yaml` for the generated API reference, so the
workflow checks out submodules.

### The URL

`astro.config.mjs` sets `SITE = 'https://accreleus.github.io'`, and
`src/data/docs-version.js` sets `DOCS_ROOT = '/quasar'` (edge lives under it).
Those two constants are the only thing that changes if the repository moves
organisation or the site gets a custom domain.

## Writing conventions

**No em-dashes.** Use periods, commas or parentheses. There is a check for this
under Maintenance below.

**Every command a reader might run goes in a fenced code block**, tagged `bash`,
one command per block where they are meant to be run separately.

**Do not document what has not shipped.** Design records and open tickets
describe work that is proposed rather than built, and a merge into `develop` is
not a release. Where this site describes something as not yet available, that is
deliberate and was checked against the code.

**Say the limit in the same breath as the feature.** The audience is
self-hosters who will find the limit themselves within an hour. Saying it up
front is what makes the rest credible.

**Screenshots are placeholders.** Use the `Shot` component and add a row to
`SCREENSHOTS.md`. Never ship a mocked-up or invented screenshot.

## Design

The site uses the product's own design tokens. The source of truth is
`web/src/styles/tokens.css`, which is itself taken from
`design_handoff_v3/screens/assets/console-v3.css`. Those tokens are mirrored as
`--q-*` custom properties at the top of `src/styles/theme.css` and mapped onto
Starlight's `--sl-*` variables below that.

Do not invent colours, radii or shadows. If a value is missing, add it to the
product tokens first and mirror it here.

The brand gradient appears at most once per viewport height: the nav wordmark,
one line of the hero, and the media path in the architecture diagram. Since the v3
re-mirror it is single-hue, because v3 retired the violet-to-cyan gradient (see
the header of `theme.css`). Buttons are flat violet.

Dark is the default. A small script in `astro.config.mjs` sets it on a first
visit. The theme toggle still works and a stored preference always wins.

## Maintenance

Check for em-dashes before committing:

```bash
grep -rn "—" site/src/content site/src/components
```

Check that internal links resolve against the built output:

```bash
cd site && npm run build && grep -roh "(/quasar/[a-z-]*/[a-z-]*/)" src/content | tr -d '()' | sort -u > /tmp/links.txt && ls dist/*/*/index.html | sed 's|dist|/quasar|; s|/index.html|/|' | sort -u > /tmp/pages.txt && comm -23 /tmp/links.txt /tmp/pages.txt
```

Anything printed by that last command is a link pointing at a page that does not
exist. `scripts/check-versions.mjs` (see Deployment) checks every rendered link,
not only content ones, in both versions.
