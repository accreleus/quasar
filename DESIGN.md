# DESIGN.md: the Quasar UI spec

This file is the design spec for `web/`. It holds the rules the UI follows and the
places where Quasar deliberately departs from the v3 mocks. Read it before touching
anything a user sees.

## Precedence

1. **This file.** Where it states a rule, the rule wins, including over a mock.
2. **The v3 mocks** (`design_handoff_v3/screens/*.html`) for composition: what a
   screen contains, how it is laid out, and how it behaves. The mocks were produced by
   a design tool and carry flaws that were baked in; the known ones are listed under
   [Where we override v3](#where-we-override-v3). The handoff README's instruction to
   "recreate pixel-faithfully" is superseded by this file.
3. **Nothing else.** Do not restyle from taste. If neither this file nor a mock covers
   the surface you are changing, say so and ask before styling.

`web/src/styles/tokens.css` holds the values. This file names roles and rules and does
not repeat values, so the two cannot drift. Where the handoff README's token tables
disagree with `tokens.css`, `tokens.css` wins: it carries the later "palette pass"
values the mocks settled on.

## Where we override v3

Each entry is a flaw in the mocks and the rule that replaces it. When you find a new
one, add it here in the same change that fixes it, so the next person does not copy the
mock back in.

| # | The mock does | The rule is | Enforced by |
|---|---|---|---|
| 1 | Spacing values off the 4px scale (5, 6, 7, 9, 10, 11, 13, 14px…) copied between components | All padding, margin and gap use `--s1`…`--s10`. Ties round **up**, because text was too tight against borders; they round down only where rounding up widens or overflows a dense row (table cells horizontally, table-row chips, the HUD pill, the rail). | `spacing-scale` lint rule |
| 2 | The v3 home mock's library section: a title row, one segmented kind filter, a flat grid and no way to move through 1,500 titles | The library section is the redesign the operator approved from the UX lab (2026-10-16, #714): one toolbar row (title and count, the kind tabs, then a removable genre chip, Genre and Sort menus once the control-api carries that data), the A–Z index as a column in the right gutter over letter sections of 60+ titles, a caption strip under the art for the recency sorts, and ranked, typo-tolerant game search in the topbar popover (`.fz-*`) and the Ctrl+K palette (same matcher, no styles of its own). Genre and the played/added sorts need data the control-api does not carry yet (`pages/app/library/libraryExtras.ts`), so they stay hidden until it does. Styles: `styles/library-new.css`, `styles/search-fuzzy.css`; overrides of shipped classes are scoped to `html[data-design="new"]`, and Classic layout keeps the v3 section. | review against the UX lab, `useNewDesign()` |
| 3 | Opens a game in an inline detail band under its tile | Operator-approved redesign from the UX lab (2026-10-16, #714): a game opens its own page, `/app/game/:id` (`web/src/pages/app/game/`), with Play, a stream line that says what Play asks for, and history below the fold. The band stays for Classic layout. | `useNewDesign()`; `GamePage.test.tsx` |
| 4 | Has no shelves above the library grid | Operator-approved redesign from the UX lab (2026-10-16, #714): Home shows a Favourites shelf and a Jump back in shelf (`web/src/pages/app/home/Shelves.tsx`). | `useNewDesign()`; `AppHomeNext.newDesign.test.tsx` |
| 5 | Shows favourite and Play only on hover, and no title under the art | Operator-approved redesign from the UX lab (2026-10-16, #714): every grid tile has a resting favourite heart and a one-line title strip; `F` on a focused grid tile toggles the favourite (`TileHeart.tsx`). | `useNewDesign()`; `LibraryTile.test.tsx` |
| 6 | Has no route from search to a game | Operator-approved redesign from the UX lab (2026-10-16, #714): choosing a game in the top-bar search or the command palette opens its page with focus on Play, and the top-bar search stays on that page (`searchToPage.ts`). | `useNewDesign()`; `AppLayout.test.tsx` |
| 7 | A tabbed account area: three rail rows, each with its own tab row. | **New design only, operator-approved from the UX lab (2026-10-16, #714):** one scrolling account page (`pages/app/account/onepage/`) with a rail row and a phone chip per section, sections editing in place, and devices grouped in use / not used in 90 days. The Classic layout switch has its own Appearance section. Classic layout keeps the tabs. | `AccountLayout` test |
| 8 | A top-bar search that finds games and fleet objects only. | **New design only, operator-approved from the UX lab (2026-10-16, #714):** the palette also finds account settings, devices and, for admins in the console, admin pages and settings, current place first; choosing one lands on it focused and highlighted (`components/shell/settingsSearch/`). The console pages themselves are not redesigned; only the search follows the layout choice there (`useNewSearch`). Classic layout keeps the previous palette. | `settingsIndex` and `CommandPalette` tests |
| 9 | Has no release-input hint: after capture the HUD strip shows for 4 s (`evaluateHud`) and the release combo lives only in the Input pane's Keys rows | Operator decision (#647): when capture engages, the strip carries one extra line naming the release actions that work on this device, worded by `releaseHint` in `pages/app/hud/panes/InputPane.tsx` (the same inputs as the pane's Esc row). It lasts about 5 s and fades with the strip. Type is the menu hint's size (`--t-xs`), brighter (`--text-2`); a vertical dock has no room and shows none; a top-docked strip steps down out of Chromium's top-centre Esc bubble by the error banner's offset. The refused-capture fallback is a "Click to play" primary button on `--modal-scrim`, under the HUD. No new tokens. Styles: `.hud-hint` (`hud.css`), `.session-play` (`session.css`). | review, `Hud.test.tsx` |
| 10 | The Region and Input mock lists "Latin American" as a variant of the Spanish keyboard layout. | xkeyboard-config has no such variant (`es` has no variant for it): Latin American is its own layout, `latam`, so the keyboard pickers list "Spanish (Latin American)" as a layout of its own (`lib/region/options.ts`). | review |
| 11 | Sets the world map caption ("Click your area on the map…") in `--text-4` at `--t-xs`: 4.3:1 on the dark map well and 2.9:1 on the light one, below the 4.5:1 body-text floor | The caption is the map's instruction, so it is muted, not faint: `--text-3` (`.tz-map-cap`). | axe `color-contrast` (autoresearch harness) |
| 12 | Keeps the selected timezone row's country · zone and offset at `--text-3` on `--accent-soft`: 3.95:1 in the light theme | On the selected row they step up to `--text-2` (`.tz-opt[aria-selected="true"]`), the foreground side of the role ladder, never a grey of their own. | axe `color-contrast` (autoresearch harness) |

*Owner to add: the unpolished v3 elements already identified. Each needs the mock
behaviour, the replacement rule, and whether a lint rule can enforce it.*

## Tokens

Use a token for every colour, spacing step, radius, shadow, font and type size. Never
hard-code a value a token already names, and never derive a new value inline.

### Colour roles

| Role | Tokens | Rule |
|---|---|---|
| Surfaces | `--surf-canvas` < `--surf-chrome` < `--surf-panel` < `--surf-raised` < `--surf-control`; `--surf-inset` recessed; `--core` deepest | One hue family, stepped by lightness. Chrome (topbar, rail) is its own material. Panels are cards and drawers, raised is menus and table heads, control is buttons, inset is inputs and wells. |
| Text | `--text` > `--text-2` > `--text-3` > `--text-4` | Primary, secondary, muted, faint. `.sec` is text-2 and `.muted` is text-3. |
| Lines | `--line`, `--line-2`, `--line-3` | Hover strengthens a border one step (`--line-2` → `--line-3`). |
| Accent | `--accent`, `--action` (filled buttons), `--accent-hover`/`-press`, `--accent-text` (links), `--accent-soft`/`-2` | **Violet is reserved for action and selection.** One accent hue; the v1 violet-to-cyan spectrum is retired. |
| Capacity | `--teal`, `--teal-soft`, `--info*` | Capacity and informational state read teal, never violet. |
| Chart second hue | `--lavender` | Only where a chart needs a second series colour the accent cannot supply. |
| State | `--success`, `--warning`, `--danger`, each with `-text`, `-bg`, `-line` | Text on a tinted state background uses the matching `-text`; the border uses `-line`. |
| Glass | `--glass-*`, `--pop-bg` | Floating surfaces (topbar, HUD, menus, auth card). |

Light appearance re-points the same names (`[data-theme="light"]`). A rule written
against tokens follows both themes for free. **Login, loaders and the in-stream HUD are
dark-locked** regardless of the theme setting.

Raw colours (hex, `rgb()`, `oklch()`, named colours) live only in `tokens.css`. The
exceptions, each marked with a reasoned allow comment: the black letterbox behind the
video, the brand mark's own colours (`components/QuasarMark.tsx`), and computed chart
colours.

### Spacing

The scale is `--s1`…`--s10`: 4, 8, 12, 16, 20, 24, 32, 40, 48, 64px. Nothing else.

- **Inside a control or chip:** `--s1`/`--s2` vertical, `--s2`/`--s3` horizontal.
- **Between related items** (label and value, icon and text, chips in a row): `--s1`–`--s3`.
- **Inside a card or panel:** `--card-pad` (20px, 16px dense).
- **Between cards and sections:** `--s4`–`--s6`.
- **Page gutter:** `--page-pad` (32px, 24px dense, 16px on phones).
- `1px` hairlines are the only spacing literal allowed without comment.
- A value that must stay off the scale (optical centring of a glyph, alignment to a
  column another component fixes) keeps its literal with a
  `design-lint-allow spacing-scale: <reason>` comment.

### Type

| Token | Family | Use |
|---|---|---|
| `--font-brand` | Michroma | The wordmark **only**: uppercase, `.2em` tracking, small sizes, weight 400. |
| `--font-ui` / `--font-display` | IBM Plex Sans | Headings and body. Headings weight 600, `-.01em` tracking. |
| `--font-mono` | IBM Plex Mono | Metrics, IDs, digests, `kbd`, tier and chip labels. |

Sizes come from `--t-display`, `--t-h1`…`--t-h3`, `--t-lg`, `--t-base`, `--t-sm`,
`--t-xs`. Dense mode shrinks the headings and body; do not compensate by hand.

### Radii, elevation, focus

- Radii are square: `--r-control` (4px) for controls, `--r-panel` (8px) for cards and
  panels, `--r-feature` (12px) for hero surfaces, `--r-pill` for pills and chips.
- Shadows carry the glass top highlight: `--shadow-sm|md|lg`, `--inset-top`.
- **Every interactive element shows a visible focus ring** on `:focus-visible`
  (`--glow-accent`, or a 2px accent outline offset 3px).

### Layout rhythm

`--row-h`, `--control-h`, `--page-pad`, `--card-pad`, `--rail-w`, `--topbar-h` and
`--tabbar-h` set the console's rhythm and respond to `[data-density="dense"]` and
`[data-rail="collapsed"]`. Size rows and controls from these, not from padding.

## Density

Comfortable is the default; dense is a desk setting. Both must work on every console
page.

- Tables, chips, the HUD pill and the rail are the dense rows. They take the smaller
  spacing step and never wrap.
- A touch target is never smaller than 44px, dense or not. A small visual control gets
  a padded hit area (the HUD's 28px buttons inside a 36px pill are the model).
- The bottom tab bar keeps its height in both densities.

## Components and where styles live

Reach for these in order:

1. **A React component** in `web/src/components/` (`Button`, `Chip`, `StatusChip`,
   `Table`, `Card`, `Modal`, `Drawer`, `SegmentedControl`, `TextField`, `Stat`, `Bar`…).
   `Table` columns take `align: "right"` for numeric columns.
2. **A primitive class** from `styles/primitives.css` (`.btn`, `.card`, `.chip`, `.tabs`,
   `.qtable`, `.note`, `.kv`, `.field`, `.menu`…). One owner per class: a selector in
   primitives is not restyled elsewhere.
3. **A utility** from the utilities block in `styles/components.css`: layout (`.row`,
   `.col`, `.between`, `.wrap`, `.grow`, `.center`), gaps (`.gap1`–`.gap7`), margins
   (`.m0`, `.mb0`, `.mt1`–`.mt5`, `.mb3`–`.mb6`, `.ml-auto`), text (`.muted`, `.sec`,
   `.right`, `.nowrap`, `.tone-warning`, `.tone-info`). Add a missing step here rather
   than inline. `.stack` is **not** a layout helper: it is the two-line table cell in
   primitives.
4. **A page class** in the stylesheet that already owns that page (`styles/admin.css`,
   `styles/admin/*.css`, `styles/home.css`, `styles/hud.css`, `styles/shell.css`,
   `styles/login.css`, `components/layout.css` for the setup wizard), written with
   tokens.

**Inline `style={{…}}` is for values a stylesheet cannot know in advance:** sizes and
offsets computed at runtime, transforms, opacity, `gridTemplateColumns`, and custom
properties handed to CSS (`"--bar": pct`). Everything else goes through the four steps
above.

`/styleguide` (public route) renders the live tokens and components. Check a change
there as well as on the page it targets.

## Region and Input settings (#844)

No v3 mock covered these. The composition, with its direction approved by the owner, is
`design_handoff_v3/screens/region-input-v3.html` (account sections, then the admin
defaults). Rules:

| Control | Rule |
|---|---|
| Timezone | A search field over a listbox (`role="listbox"`, 44px rows, inset well like `.input`). The first row is pinned: "Follow my browser" with the detected zone, and it is the default. The selected row is `--accent-soft` with an accent inset ring. A "Now: zone · time · offset" line sits under the field. |
| World map | Beside the list, below it under 1100px, absent at 600px and below (phones get the list only). Inline SVG dot map on an inset well; land dots `--line-3`, the zones sharing the hovered offset `--text-2`, the selected offset `--accent`; city dots `--text`; the pick is a `--text` dot ringed in `--accent` with a `--pop-bg` label. Pointer only: the list is the accessible path. Map data is public domain (Natural Earth land, tz `zone.tab`). |
| Language | `SelectField`; default "Follow my browser (<detected>)". Region format defaults to "Same as language (<language>)", or "Install default (<format>)" when the install sets a region format, and its hint shows a live mono example. |
| 3-way choice | "Match cursor to my window" (Auto / Always on / Off) is the existing `SegmentedControl`, as wide as its options, with a hint that changes with the choice. |
| Range slider | New: track `--surf-inset` with a `--line-3` border, filled in `--accent` to the value (`--fill` custom property), `--text` thumb with `--shadow-sm`, `--glow-accent` on focus, 44px hit area. Mono value readout on the right; mono end and centre ticks underneath. Sensitivity is log-scaled so 1× sits at the centre. |
| Unavailable toggle | A `Switch` disabled with its hint saying why ("Not available in this browser"), never hidden. |
| Rail icons | Region is a globe, Input a keyboard, drawn in the shell glyph style (16px box, 1.5 stroke, round caps). |
| "Applies from your next launch" | Said once per section, in the section's sub-line, not under every field. The status line under the card confirms each save, and says when idle Steam was closed to pick the change up (never a session in use). |

Admin defaults use the Settings page's `SettingRow`s; the admin timezone default is a
grouped `<select>`, not the map.

## Motion and behaviour

- Transitions `.12s`–`.24s ease`. Buttons lift `-.5px` on hover and settle `+.5px` when
  pressed.
- Hover brightens a surface one step and strengthens its border one step.
- Links use `--accent-text` and brighten on hover.
- `prefers-reduced-motion` stops every looping or pulsing animation (live dots, the
  accretion loop, scanlines).
- Keep the mocks' accessibility affordances: `aria-selected`, `aria-expanded`,
  `aria-invalid`, focus rings, 44px hit targets.

## Enforcement

`npm run lint:design` (in `web/`) checks the machine-checkable rules above and names
the fix in every failure:

| Rule | Checks |
|---|---|
| `spacing-scale` | padding, margin and gap in CSS are on the scale |
| `raw-colour-css` | no raw colour outside `tokens.css` |
| `inline-style` | inline styles use only the runtime-value keys above |
| `raw-colour-tsx` | no colour literal in a component |

An exception takes `/* design-lint-allow <rule>: <reason> */` (or `//` in TSX) on the
same or the preceding line. A per-file baseline records older debt and only goes down.
Everything else in this file (colour roles, density, component order) is enforced by
review against this file.

## Visual verification

A change to anything a user sees is checked by eye against the matching mock **and**
this file before it is called done: both themes, both densities, and a phone width for
user-area surfaces. Where the result follows this file and differs from the mock, say
which override entry it follows.
