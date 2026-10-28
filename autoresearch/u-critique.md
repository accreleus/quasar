⚠️ DEGRADED: single-context (operator rule for this run: no sub-agents. Assessment A was written from source and screenshots before the detector output was read.)

# impeccable critique: Account ▸ Region / Input, Admin ▸ Settings (Region / Input defaults)

Target: `web/src/pages/app/account/onepage/{RegionSection,InputSection,TimezonePicker,WorldMap}.tsx`,
`web/src/styles/account-onepage.css`, `web/src/pages/admin/settings/{RegionDefaultRows,InputDefaultRows,SettingRow}.tsx`.
Build: kept head after exp 12. Mode: **Operate** (settings). Authority: DESIGN.md §"Region and
Input settings" first, then `design_handoff_v3/screens/region-input-v3.html`. No PRODUCT.md
(`impeccable context`: NO_PRODUCT_MD, scoped refinement allowed on the incumbent system).

## Assessment A: design review

**Design specificity.** The page is specific to this product. The dot-map timezone picker, the
"Now: city · time · offset" line, the live mono format example and the "applies from your next
launch" sub-line all come from what a streamed session needs. The composition follows the mock
closely.

**Heuristics (0–4).**

| Heuristic | Score | Note |
|---|---|---|
| Visibility of system status | 4 | The status line confirms each save |
| Match with the real world | 4 | — |
| User control and freedom | 3 | "Follow my browser" is the way back |
| Consistency and standards | 3 | Account uses visible labels; admin uses `aria-label`, with different words |
| Error prevention | 3 | Controls lock while a save runs |
| Recognition over recall | 4 | — |
| Flexibility | 3 | The map and the list are two ways in |
| Aesthetic and minimalist | 3 | — |
| Error recovery | 3 | — |
| Help and docs | 3 | — |

**Cognitive load.** Low. Each field carries one decision, and the language select is the only
choice with more than four options, which is inherent to it.

**Priority issues (from reading the source):**

1. **The Input section's switches have no programmatic name** (`components/TextField.tsx`
   `Switch`). The inner `<label class="switch">` is nested inside the row's `<label>`. Nested
   labels are invalid HTML, and the input binds to the inner label, which has no text, so a
   screen reader announces "checkbox" for Raw mouse input, Flip scroll direction and Gamepad
   Nintendo layout. The defect is in the shared primitive, so every labelled `Switch` in the
   app has it.
2. **The map caption is faint text at 11 px** (`.tz-map-cap`, `--text-4`): it is the
   instruction for using the map, set in the faint role.
3. **The selected timezone row's secondary text** (country · zone, offset) stays `--text-3` on
   `--accent-soft`. That is legible in dark and weak in light.
4. Minor: the admin defaults rows label their selects with `aria-label` ("Default timezone"),
   not by tying the visible row label to the control, so the visible and spoken names differ.
   They still pass label-in-name.

Strengths: one save path per section, the status line, and honest copy for an unavailable
toggle.

## Assessment B: detector and browser evidence (harness `polish` run, exp 12 build)

**axe-core** (scoped to `#set-region`, `#set-input` and the admin Region/Input defaults cards;
2 pages × 2 themes × 2 widths; 28 nodes in total):

- `label` + `label-title-only`: `#raw-mouse`, `#flip-scroll`, `#nintendo-layout`. That is 5
  nodes on each of the 4 account scans, 20 in total.
- `color-contrast` `.tz-map-cap > span`: 4.32:1 in dark and 2.9:1 in light, at 1280 only (the
  map is absent on phones). 4 nodes.
- `color-contrast` `.tz-opt[aria-selected] .tz-sub` / `.tz-off`: 3.95:1, light theme only.
  4 nodes.
- Admin Settings: 0.

**impeccable detect** (175 findings over 8 URL scans, whole page). `tiny-text` makes up 151 of
them: the `.hint` / `.field-hint` / `--t-xs` type role that DESIGN.md sanctions. Changing it
would restyle the type scale against the spec, so it is out of bounds. Outside the scope:
`low-contrast` and `nested-cards` on the overlay preview stage, `undersized-ui-text` on the
Storage table headers, and `side-tab` / `nested-cards` / `line-length` on the shared
`.note.warn` callout. False positives for this surface: `tiny-text` on the hint role.

## Synthesis: ranked one-change hypotheses for Track U

| # | Finding | Change | Expected primary Δ | Authority |
|---|---|---|---|---|
| U1 | axe `label`/`label-title-only` (A1) | `Switch`: the inner `label.switch` becomes a `span`, so the input is named by the row label | −20 | HTML validity; no visual change |
| U2 | axe `color-contrast` `.tz-map-cap` (A2) | caption `--text-4` → `--text-3` (the muted role); new DESIGN.md override row | −4 | DESIGN.md "Where we override v3" process |
| U3 | axe `color-contrast` on the selected row (A3) | selected row's `.tz-sub`/`.tz-off` → `--text-2`; new override row | −4 | same |

After U3 the scoped axe count is 0. What the detector still reports in scope is the sanctioned
hint size, so the remaining U budget has nothing the metric can see. A4 (admin row labelling)
is a consistency issue the metric does not score.

Questions skipped: autonomous overnight run; the operator is not present. The experiments and
these findings go to the lead in the morning report.
