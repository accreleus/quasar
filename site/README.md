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
      Placeholder.astro     the screenshot placeholder panel
      Shot.astro            figure wrapper used in docs pages
    styles/theme.css        Starlight variable overrides, product tokens
    assets/quasar-mark.svg  the brand mark
  SCREENSHOTS.md            checklist of screenshots still to capture
```

## Deployment

`.github/workflows/pages.yml` builds `site/` and publishes `site/dist` to
GitHub Pages. Pages must be enabled on the repository with source "GitHub
Actions" before the first run.

The workflow is `workflow_dispatch` only, which is this project's convention for
every workflow except `leak-scan`. A Pages deploy triggered on push to `main`
would be the natural trigger for a docs site and should be a deliberate decision
rather than an accident. Manual dispatch also fits the "`main` is production"
rule reasonably well, since publishing then stays an explicit act.

The build reads `protocol/openapi.yaml` for the generated API reference, so the
workflow checks out submodules.

### The URL

`astro.config.mjs` sets:

```js
const SITE = 'https://accreleus.github.io';
const BASE = '/quasar';
```

Those two constants are the only thing that changes if the repository moves
organisation or the site gets a custom domain.

## Writing conventions

**No em-dashes.** Use periods, commas or parentheses. There is a check for this
under Maintenance below.

**Every command a reader might run goes in a fenced code block**, tagged `bash`,
one command per block where they are meant to be run separately.

**Do not document what has not shipped.** Several design specs under
`docs/design/plans/` describe work that is proposed rather than built. Where this
site describes something as not yet available, that is deliberate and was checked
against the code.

**Say the limit in the same breath as the feature.** The audience is
self-hosters who will find the limit themselves within an hour. Saying it up
front is what makes the rest credible.

**Screenshots are placeholders.** Use the `Shot` component and add a row to
`SCREENSHOTS.md`. Never ship a mocked-up or invented screenshot.

## Design

The site uses the product's own design tokens. The source of truth is
`web/src/styles/tokens.css`, which is itself generated from
`design_handoff_quasar/screens/assets/quasar.css`. Those tokens are mirrored as
`--q-*` custom properties at the top of `src/styles/theme.css` and mapped onto
Starlight's `--sl-*` variables below that.

Do not invent colours, radii or shadows. If a value is missing, add it to the
product tokens first and mirror it here.

The brand gradient appears at most once per viewport height: the nav wordmark,
one line of the hero, and the media path in the architecture diagram. Buttons are
flat violet.

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
exist.
