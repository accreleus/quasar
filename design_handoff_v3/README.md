# Handoff: Quasar v3 — Client + Admin Console

## Overview
Quasar is a premium self-hosted cloud-gaming platform. This package covers the **v3 design pass** — a redesigned visual language (the "Quasar V2 reference tokens") applied across five surfaces:

- **Login** (`login-v3.html`) — sign-in for the streaming client
- **Home** (`home.html`) — the user-area game library / launch surface
- **Loading** (`loading-v3.html`) + **stream handoff** (`loading-to-stream-v3.html`) — connection-establishment states
- **Session overlay** (`session-overlay-v3.html`) — the in-stream HUD, a docked morphing pill/shelf
- **Admin console** (`admin-console-v3.html`) — the full operations console (activity, fleet, library, app editor, people, account)

This supersedes the earlier `design_handoff_quasar` package. Where the two differ, **v3 wins**: IBM Plex type instead of Space Grotesk/Hanken, Michroma for the wordmark, oklch tokens, squarer radii, denser chrome.

## About the Design Files
The files in `screens/` are **design references built in HTML/CSS/vanilla JS** — prototypes showing intended look and behavior, not production code to ship. The task is to **recreate these designs in the target codebase's existing environment** (its framework, component primitives, router, state layer). If no environment exists yet, choose an appropriate stack and implement the designs there. The HTML has no build step and no framework — port the class-based components onto real components; port the token blocks into the target's token system.

**Always reference the actual HTML/CSS for exact markup and values** — this README covers tokens, structure, and behavior; the files are the source of truth.

## Fidelity
**High-fidelity.** Colors, typography, spacing, radii, elevation, and interaction states are final. Recreate pixel-faithfully.

## Files
```
design_handoff_v3/
├── README.md
├── brand/                       ← Quasar accretion-disc logo (SVG, + transparent + favicon)
└── screens/
    ├── login-v3.html            ← self-contained (tokens inlined)
    ├── loading-v3.html          ← self-contained
    ├── loading-to-stream-v3.html← self-contained
    ├── session-overlay-v3.html  ← uses assets/console-v3.css
    ├── home.html                ← uses assets/quasar.css + home-v3.css + quasar.js
    ├── admin-console-v3.html    ← uses assets/console-v3.css + data.js + ui.js + pages-*.js
    └── assets/
        ├── console-v3.css       ← v3 token contract + console/HUD component styles (source of truth)
        ├── home-v3.css          ← v3 reskin layered over quasar.css for the home page
        ├── quasar.css           ← legacy v1 component layer home.html still builds on
        ├── quasar.js            ← theme/density/user-menu helpers + SVG sparklines
        ├── data.js              ← mock fleet/session/user data for the console
        ├── ui.js                ← console shell: rail, topbar, drawer, command palette
        └── pages-*.js           ← one renderer per console section
```

## Design Tokens (v3 contract)
Defined in `console-v3.css` `:root` (console + overlay), inlined in login/loading, and remapped in `home-v3.css`. All colors are **oklch**. Port these wholesale; never hard-code derived values.

### Surfaces (ink scale)
| Token | Value |
|---|---|
| `--core` / `--ink-0` | `oklch(0.105 0.01 267)` — deepest, inset wells |
| `--bg` / `--ink-1` | `oklch(0.1449 0.0115 266.85)` — app background |
| `--surface` / `--ink-2` | `oklch(0.1869 0.0151 266.77)` — panel |
| `--surface-raised` / `--ink-3` | `oklch(0.224 0.018 263)` — card |
| `--surface-active` / `--ink-4` | `oklch(0.272 0.022 260)` — hover/elevated |
| `--ink-5` | `oklch(0.315 0.026 260)` — strong fill / input bg |

(home.html uses a slightly darker parallel scale, `--surf-*` in `home-v3.css`.)

### Text
`--text: oklch(0.9618 0.0086 247.91)` · `--text-2: oklch(0.76 0.022 258)` · `--text-3: oklch(0.6605 0.0265 259.81)` · `--text-4: oklch(0.56 0.024 259)`. Login/loading use the aliases `--fg`, `--fg-strong: oklch(0.985 0.004 247)`, `--muted`, `--muted-strong`.

### Lines
`--line: oklch(0.9618 0.0086 247.91/.09)` · `--line-2: /.12` · `--line-3: oklch(0.6605 0.0265 259.81/.46)`.

### Accent (single-hue violet system)
| Token | Value |
|---|---|
| `--accent` | `oklch(0.5998 0.2179 273.21)` |
| `--action` (filled buttons) | `oklch(0.5 0.19 273.21)` |
| `--accent-hover` / `--accent-press` | `oklch(0.54 / 0.46 0.19 273.21)` |
| `--accent-text` (links on dark) | `oklch(0.78 0.13 273.21)` |
| `--accent-soft` / `--accent-soft-2` | accent at `.16` / `.26` alpha |

One accent hue only — the v1 violet→cyan spectrum is retired on v3 surfaces (home keeps a teal `--info` for informational chips).

### State
success `oklch(0.76 0.15 164)` · warning `oklch(0.82 0.14 85)` · danger `oklch(0.69 0.19 27)`; each with `-text`, `-bg` (~.12 alpha), `-line` (~.34 alpha) variants — exact values in `console-v3.css`.

### Glass
`--glass-panel: oklch(0.1869 0.0151 266.77/.46)` · `--glass-strong: oklch(0.224 0.018 263/.62)` · `--glass-control: oklch(0.272 0.022 260/.66)` · `--glass-border: oklch(0.9618 0.0086 247.91/.12)` · `--glass-highlight: …/.14` (inset top edge). Backdrop filters run `blur(14–28px) saturate(112–170%)` per component.

### Typography
| Token | Family | Use |
|---|---|---|
| `--font-brand` (`--display` in login/loading) | **Michroma** | wordmark ONLY — uppercase, `letter-spacing: .2em`, small sizes (.72–.9rem), weight 400 |
| `--font-display` / `--font-ui` | **IBM Plex Sans** 400/500/600/700 | headings + body |
| `--font-mono` | **IBM Plex Mono** 400/500/600 | metrics, IDs, kbd, tier labels |

Scale: `--t-h1:1.9rem · --t-h2:1.45rem · --t-h3:1.15rem · --t-lg:1.0625rem · --t-base:.9375rem · --t-sm:.8125rem · --t-xs:.6875rem`. Headings weight 600, `letter-spacing:-.01em`, `line-height:1.14`. Google Fonts in the mocks; self-host in production.

### Spacing, radii, elevation
- Spacing 4px base: `--s1:4 … --s10:64`.
- Radii are **squarer than v1**: `--r-control:4px · --r-panel:8px · --r-feature:12px` (aliases `--r-xs/sm:4 · --r-md/lg:8 · --r-xl:12 · --r-pill:999`).
- Shadows carry the glass top-highlight: `--shadow-md: inset 0 1px 0 var(--glass-highlight), 0 14px 36px -32px oklch(0.05 0.01 267/.68)`; `--shadow-lg` deeper. Focus ring everywhere: `0 0 0 3px var(--accent-soft)` or `outline:2px solid var(--accent); outline-offset:3px`.
- Layout rhythm (console): `--row-h:44px · --control-h:34px · --page-pad:32px · --card-pad:20px · --rail-w:216px · --topbar-h:56px`; dense mode via `[data-density="dense"]`, collapsed rail via `[data-rail="collapsed"]` (60px).

### Background field
Every v3 surface sits on the same field: base `--bg` plus fixed, very soft radial accent blooms (violet ~.07–.20 alpha) — subtle depth, never a gradient wash. Loaders/login add `linear-gradient(150deg, var(--bg), oklch(0.13 0.012 267) 58%, var(--core))`.

## Screens

### Login (`login-v3.html`)
Centered glass card, `width: min(24.5rem, 100%)`, on the graphite gradient field with one accent bloom behind the card. **No backdrop animation ships** — the file contains two optional backdrops (rotating d3 globe, wireframe runner) behind a mock-only toggle; the decision was **plain** (`data-backdrop="plain"`). Do not implement the backdrops or the toggle.

- **Lockup is the heading**: accretion mark (3.25rem) above the Michroma wordmark (.9rem, .2em tracking, uppercase). No separate greeting text.
- **Card**: padding `clamp(1.4rem,3vw,1.85rem)`, `border-radius: var(--r-feature)` (12px), border `oklch(0.9618 0.0086 247.91/0.16)`, background `oklch(0.224 0.018 263/0.34)` (34% opacity), `backdrop-filter: blur(14px) saturate(130%)`, `--glass-shadow`.
- **Fields**: persistent labels above (.75rem, `--muted-strong`); inputs 2.75rem tall, 4px radius, bg `oklch(0.105 0.01 267/0.56)`; hover brightens border; focus = accent border + 3px soft ring. Password row has a text "Show/Hide" reveal button inside the field and a quiet "Forgot?" link beside the label.
- **Validation**: errors render only on submit (`aria-invalid` + .7rem danger text under the field) and clear as the user types. Empty error slots collapse (`:empty{display:none}`).
- **Submit**: full-width primary button on `--action`, hover `--action-hover`, pressed `--action-pressed`.

### Loading (`loading-v3.html`) and stream handoff (`loading-to-stream-v3.html`)
Full-bleed, dark-locked, no app chrome; lockup pinned top-left. Center: the animated **quasar accretion visual** (core + disc + orbits + jets, single accent hue, 4.8s loop). Bottom-center status block:

- **Headline**: "Establishing" holds still; the phase word below swaps with a 140ms fade-and-lift (`opacity` + `translateY(0.4rem)`), phase word in `--accent`.
- **Glyph rail** (replaces progress bars/labels): one glyph per phase — *signalling → video channel → input capture* (network, stream, gamepad sprites; padlock bespoke). States: pending `.38` opacity → active (accent, animated, connector scanning) → done (green `--success` inside a 1px ring, keeping its own glyph — not a generic tick).
- `loading-to-stream-v3.html` continues the sequence: the loader completes and apertures into the live stream with HUD chrome flashing on. Respect `prefers-reduced-motion` for the loops.

### Session overlay (`session-overlay-v3.html`)
The in-stream HUD is a **docked morphing object** over live video (mock uses a dark stand-in). Uses `console-v3.css`.

- **Rest state**: a 36px-tall pill docked to a screen edge, containing only: connection-signal glyph, frame-rate readout (mono), capture-input toggle, and — separated by a 12px safety gap, 14px from the edge — an exit button in danger colors, plus an expand chevron.
- **Expanded**: click morphs the pill into a **full-edge shelf** (spans the viewport edge, no corner radius on the screen-edge side; bar height constant). The shelf holds three tabs: **Games** (default), **Controller & input**, **Performance stats**. Tab switches swap content without resizing the bar.
- **Dock positions**: top, bottom, left, right all supported. Left/right docks render a single 236px-wide column (vertical layout); top/bottom are horizontal.
- Auto-hide when idle; reappear on pointer/gamepad activity. All controls meet 44px-class hit targets even inside the 36px pill (padded hit areas).

### Home (`home.html`)
User-area library. Structure comes from the legacy `quasar.css` component layer with `home-v3.css` layered after it as the v3 reskin (same class names — port the *computed* v3 look, not the two-file cascade).

- Glass topbar (Michroma brand, nav pills, pill search that widens on focus, theme/user controls).
- **Featured rail** (`.rail-track`): large cards with poster art, kicker line (e.g. "Session live · 24:18 on host-01" with pulsing live dot, "Most played this week"), title, spec metadata, and a launch affordance. Horizontally scrollable.
- Library grid below with cover tiles → detail (`openDetail`) → launch flow.
- Poster art in the mock hot-links Steam CDN images with lettered gradient fallbacks behind them (`.fb` + `.glyph`) — in production, serve real cover art (~2:3 portrait for rail posters) and keep the fallback treatment.
- Launch flow wiring: tile → launch/quality selection → loading → handoff → session overlay → exit returns home.

### Admin console (`admin-console-v3.html`)
Single-page console shell; each section rendered by its `pages-*.js`. Shell (in `ui.js` + `console-v3.css`):

- **Topbar** (56px glass): brand, centered command-palette trigger (`.cmdk`, pill, ⌘K kbd hint), icon buttons, user menu.
- **Left rail** (216px, collapsible to 60px icon rail): sectioned nav (`.rail-sec` uppercase 10px labels), items with 17px stroke icons, active = `--accent-soft` fill + 3px accent edge bar; live/fault count pills (`.mk-live`/`.mk-fault`).
- **Sections**: Activity (KPI stats + charts), Fleet (hosts/GPUs), Library (apps), App editor (runtime-spec form — this includes the **launch-options panel**), People, Account.
- **Primitives** (all in `console-v3.css`): pill `.btn` family (glass, primary on accent at 82% + glow, ghost, danger, sm/ico), `.card`/`.panel-head`, `.chip` states (21px, uppercase 10px), `.tabs` (36px, accent underline), `.qtable` rows at `--row-h`, drawer, command palette, gauges/bars, sparklines (`data-spark` via `quasar.js`-style inline SVG).
- Density toggle (`[data-density]`) and rail collapse persist to `localStorage`.

## Interactions & Behavior (cross-cutting)
- Transitions `.12–.24s ease`; buttons lift `translateY(-.5px)` on hover, `+.5px` on active.
- Focus: visible accent ring on every interactive element (`:focus-visible`).
- `prefers-reduced-motion` gates all looping/pulsing animation (live dots, accretion loop, scanline).
- Hover states brighten one ink step and strengthen the border (`--line-2` → `--line-3`).
- Links: `--accent-text` default, brighten on hover.

## State Management
- **Auth**: submit-time validation state per field; error → clear-on-input.
- **Session lifecycle**: `idle → signalling → video-channel → input-capture → streaming → ended/error`. The loaders visualize the middle three; the overlay owns `streaming` (fps, connection quality, capture-input on/off, dock position, expanded/collapsed, active tab).
- **Home**: library list, continue-playing/live-session status per title, search.
- **Console**: active section, fleet/session/user data (mocked in `data.js` — replace with real telemetry: WebSocket/SSE for live metrics, REST for CRUD), density + rail collapse (persisted), drawer + command-palette open state.

## Assets
- **Logo**: `brand/accretion.svg` (+ transparent and favicon variants) — the accretion-disc mark. Also inlined as `<svg class="mark">` in the screens.
- **Icons**: all inline SVG, stroke-based, `currentColor`, `stroke-width` 1.4–1.7, 14–17px. Map to the codebase's icon set at matching weight.
- **Fonts**: Michroma, IBM Plex Sans, IBM Plex Mono (Google Fonts in mocks; self-host in production).
- **Game art**: mock hot-links Steam CDN; supply licensed art in production.
- login-v3.html's d3/topojson script tags serve only the cut backdrop options — ignore them.

## Implementation Notes
- Port the `console-v3.css` token block first; build every component against tokens.
- The console's JS files are mock renderers over `data.js` — treat them as behavioral spec (what each section shows, what actions exist), not as code to port.
- Keep the accessibility affordances present in the mocks: `aria-selected`/`aria-expanded`/`aria-invalid`, focus-visible rings, reduced-motion guards, 44px hit targets.
- Dark-locked surfaces: login, loaders, and session overlay are always dark regardless of any app theme setting.
