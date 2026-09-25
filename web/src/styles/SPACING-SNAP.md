# Spacing snap: review checklist

Every padding, margin and gap in `web/src/**/*.css` that was off the 4px token
scale (`--s1`…`--s10` in `tokens.css`), and what it became. This is the owner's
visual review list for the `feat/web-design-lint` branch; delete it once the
branch lands.

How values were snapped:

- Nearest token. `rem` converted at 16px/rem first.
- Ties (6, 10, 14, 18, 22px…) round **up**, because text sat too tight against
  borders. They round **down** only in dense rows where rounding up would widen
  or overflow: table cells (horizontal only; row height is `--row-h`), the
  table-row chips, the in-stream HUD pill, and the rail. Those rows say why.
- Values under 4px became `var(--s1)`: each was a small gap or nudge whose intent
  is "some space", and 0 would have closed it.
- Values already on the scale (e.g. `12px`) became their token, with no visual
  change.
- `96px` is above the scale and became `calc(var(--s10) + var(--s7))`, the same
  96px.
- Kept literals carry a `design-lint-allow spacing-scale: <reason>` comment in
  the stylesheet; they are listed here as "kept".

| file | selector | before | after | rounded down? why |
|---|---|---|---|---|
| `components/layout.css` | `.login-error` | `padding: 10px 14px` | `padding: var(--s3) var(--s4)` |  |
| `pages/app/SessionLoader.css` | `.sl-lockup` | `gap: 0.85rem` | `gap: var(--s3)` |  |
| `pages/app/SessionLoader.css` | `.sl-status-foot` | `margin-top: 1.6rem` | `margin-top: var(--s6)` |  |
| `pages/app/SessionLoader.css` | `.sl-rail` | `gap: 0.5rem` | `gap: var(--s2)` |  |
| `pages/app/SessionLoader.css` | `.sl-stall` | `margin-top: 1.75rem` | `margin-top: var(--s7)` |  |
| `pages/app/SessionLoader.css` | `.sl-stall-title` | `margin: 0 0 0.25rem` | `margin: 0 0 var(--s1)` |  |
| `pages/styleguide/styleguide.css` | `.sg-hero` | `padding: 52px 44px` | `padding: var(--s9) var(--s9)` |  |
| `pages/styleguide/styleguide.css` | `.sg-hero .sg-qmark` | `margin-bottom: 22px` | `margin-bottom: var(--s6)` |  |
| `pages/styleguide/styleguide.css` | `.sg-hero .sg-lede` | `margin-top: 12px` | `margin-top: var(--s3)` |  |
| `pages/styleguide/styleguide.css` | `.sg-block > h2` | `margin-bottom: 6px` | `margin-bottom: var(--s2)` |  |
| `pages/styleguide/styleguide.css` | `.sg-comp-label` | `margin-bottom: 14px` | `margin-bottom: var(--s4)` |  |
| `pages/styleguide/styleguide.css` | `.sg-sw .sg-meta` | `padding: 10px 12px` | `padding: var(--s3) var(--s3)` |  |
| `pages/styleguide/styleguide.css` | `.sg-sw .sg-tok` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `pages/styleguide/styleguide.css` | `.sg-sw .sg-hex` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `pages/styleguide/styleguide.css` | `.sg-type-row` | `gap: 20px` | `gap: var(--s5)` |  |
| `pages/styleguide/styleguide.css` | `.sg-type-row` | `padding: 14px 0` | `padding: var(--s4) 0` |  |
| `pages/styleguide/styleguide.css` | `.sg-scale-cell` | `gap: 8px` | `gap: var(--s2)` |  |
| `pages/styleguide/styleguide.css` | `.sg-lbl-sm` | `margin-top: 10px` | `margin-top: var(--s3)` |  |
| `styles.css` | `.diag-table td` | `padding: 3px var(--s4)` | `padding: var(--s1) var(--s4)` |  |
| `styles.css` | `.session-summary dd` | `margin: .25rem 0 0` | `margin: var(--s1) 0 0` |  |
| `styles/account.css` | `.dev-key` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/account.css` | `.dev-trust` | `padding: 10px 0` | `padding: var(--s3) 0` |  |
| `styles/account.css` | `.ac-group` | `margin-bottom: 6px` | `margin-bottom: var(--s2)` |  |
| `styles/account.css` | `.ac-sw` | `padding: 7px 0` | `padding: var(--s2) 0` |  |
| `styles/account.css` | `.ac-stat` | `margin-top: 5px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `.alert-row` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/admin.css` | `.alert-row` | `padding: 13px var(--card-pad)` | `padding: var(--s3) var(--card-pad)` |  |
| `styles/admin.css` | `.alert-detail` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `.alert-act` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/admin.css` | `.alert-foot` | `padding: 11px var(--card-pad)` | `padding: var(--s3) var(--card-pad)` |  |
| `styles/admin.css` | `.act-list` | `padding: 6px 0` | `padding: var(--s2) 0` |  |
| `styles/admin.css` | `.act-row` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/admin.css` | `.act-row` | `padding: 9px var(--card-pad)` | `padding: var(--s2) var(--card-pad)` |  |
| `styles/admin.css` | `.ov-cap-cell .bar-row + .bar-row` | `margin-top: 5px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `.sd-latest-head` | `margin-bottom: 14px` | `margin-bottom: var(--s4)` |  |
| `styles/admin.css` | `.sd-lr` | `padding: 11px 14px` | `padding: var(--s3) var(--s4)` |  |
| `styles/admin.css` | `.sd-chart-card` | `padding: 16px` | `padding: var(--s4)` |  |
| `styles/admin.css` | `.sd-chart-top` | `margin-bottom: 10px` | `margin-bottom: var(--s3)` |  |
| `styles/admin.css` | `.sd-chart-top` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/admin.css` | `.sd-legend` | `gap: 4px` | `gap: var(--s1)` |  |
| `styles/admin.css` | `.drawer-user-head` | `gap: 14px` | `gap: var(--s4)` |  |
| `styles/admin.css` | `.ufact` | `padding: 10px 0` | `padding: var(--s3) 0` |  |
| `styles/admin.css` | `.hist-item` | `gap: 13px` | `gap: var(--s3)` |  |
| `styles/admin.css` | `.hist-item` | `padding: 13px 0` | `padding: var(--s3) 0` |  |
| `styles/admin.css` | `.hist-dot` | `margin-top: 5px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `.hist-item .hi-meta` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `@media (max-width: 760px) › .admin-users-page .qtable tbody td` | `padding: 12px 14px` | `padding: var(--s3) var(--s4)` |  |
| `styles/admin.css` | `@media (max-width: 760px) › .admin-users-page .qtable tbody td::before` | `margin-bottom: 6px` | `margin-bottom: var(--s2)` |  |
| `styles/admin.css` | `.trace-verdict-strip` | `padding: 9px var(--card-pad)` | `padding: var(--s2) var(--card-pad)` |  |
| `styles/admin.css` | `.trace-evidence` | `padding: 12px var(--card-pad)` | `padding: var(--s3) var(--card-pad)` |  |
| `styles/admin.css` | `.trace-evidence-list` | `margin: 0 0 6px` | `margin: 0 0 var(--s2)` |  |
| `styles/admin.css` | `.trace-evidence-list` | `padding-left: 16px` | `padding-left: var(--s4)` |  |
| `styles/admin.css` | `.trace-evidence-meta` | `margin-top: 6px` | `margin-top: var(--s2)` |  |
| `styles/admin.css` | `.trace-lane-row` | `padding: 9px 0 7px` | `padding: var(--s2) 0 var(--s2)` |  |
| `styles/admin.css` | `.trace-lane-scale-item` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/admin.css` | `.trace-lane-unit` | `margin-left: 3px` | `margin-left: var(--s1)` |  |
| `styles/admin.css` | `.trace-lane-plot` | `margin-top: 5px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `.trace-event-row` | `padding-top: 7px` | `padding-top: var(--s2)` |  |
| `styles/admin.css` | `.trace-legend` | `gap: 8px 22px` | `gap: var(--s2) var(--s6)` |  |
| `styles/admin.css` | `.trace-legend-item` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/admin.css` | `.trace-tooltip` | `padding: 8px 12px` | `padding: var(--s2) var(--s3)` |  |
| `styles/admin.css` | `.trace-tooltip-lane` | `margin-bottom: 4px` | `margin-bottom: var(--s1)` |  |
| `styles/admin.css` | `.trace-tooltip-row` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/admin.css` | `.trace-tooltip-event` | `margin-top: 4px` | `margin-top: var(--s1)` |  |
| `styles/admin.css` | `.cpair` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/admin.css` | `.used-by` | `gap: 4px` | `gap: var(--s1)` |  |
| `styles/admin.css` | `@media (max-width: 760px) › .image-detail-page .qtable td::before` | `margin-bottom: 6px` | `margin-bottom: var(--s2)` |  |
| `styles/admin/editor.css` | `.ae-crop figcaption` | `margin-top: 7px` | `margin-top: var(--s2)` |  |
| `styles/admin/editor.css` | `.ae-cands` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/admin/editor.css` | `.ae-cand` | `padding: 6px` | `padding: var(--s2)` |  |
| `styles/admin/editor.css` | `.ae-cand` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/admin/editor.css` | `.ae-item` | `padding: 9px 0` | `padding: var(--s2) 0` |  |
| `styles/admin/editor.css` | `.ae-item-m` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/admin/editor.css` | `.ae-rung` | `padding: 7px 0` | `padding: var(--s2) 0` |  |
| `styles/admin/fleet.css` | `.hosts-page .host-row-id` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/admin/fleet.css` | `.hosts-page .host-row-id` | `padding-left: 17px` | `padding-left: 17px` | kept: 17px: indents the id under the name, past the 9px status dot and its gap |
| `styles/admin/fleet.css` | `.qtable tbody tr.group-row > td` | `padding: 7px var(--s5)` | `padding: var(--s2) var(--s5)` |  |
| `styles/admin/fleet.css` | `.exp-row .note, .exp-row .form-error, .exp-row .exp-note` | `margin: var(--s4) var(--s5) var(--s4) 44px` | `margin: var(--s4) var(--s5) var(--s4) 44px` | kept: 44px: aligns with `.exp-in`'s 44px text column |
| `styles/admin/fleet.css` | `.exp-actions` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/admin/fleet.css` | `.exp-actions` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/admin/fleet.css` | `.hosts-page .exp-readiness` | `padding: 0 var(--s5) var(--s5) 44px` | `padding: 0 var(--s5) var(--s5) 44px` | kept: 44px: aligns with `.exp-in`'s 44px text column |
| `styles/admin/fleet.css` | `.hosts-page .exp-in .bar-row` | `padding: 5px 0` | `padding: var(--s1) 0` |  |
| `styles/admin/fleet.css` | `.jobs-page .exp-actions` | `margin-top: 9px` | `margin-top: var(--s2)` |  |
| `styles/admin/fleet.css` | `.jobs-page .exp-row .run-error` | `padding: var(--s3) 14px` | `padding: var(--s3) var(--s4)` |  |
| `styles/admin/fleet.css` | `.host-detail-page .cap-detail` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/admin/fleet.css` | `.enroll-fact` | `gap: 4px` | `gap: var(--s1)` |  |
| `styles/admin/fleet.css` | `.enroll-snippet` | `padding: 9px 11px` | `padding: var(--s2) var(--s3)` |  |
| `styles/admin/fleet.css` | `.rel-e > summary` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/admin/fleet.css` | `.rel-e > summary` | `padding: 9px var(--card-pad)` | `padding: var(--s2) var(--card-pad)` |  |
| `styles/admin/fleet.css` | `.rel-b` | `padding: 2px var(--card-pad) 13px calc(var(--card-pad) + 47px)` | `padding: var(--s1) var(--card-pad) var(--s3) calc(var(--card-pad) + 47px)` | kept: 47px: aligns the body under the summary text, past the chevron and version columns |
| `styles/admin/fleet.css` | `.rel-b code` | `padding: 1px 4px` | `padding: 1px var(--s1)` |  |
| `styles/admin/fleet.css` | `.rel-fact` | `padding: 7px 0` | `padding: var(--s2) 0` |  |
| `styles/admin/fleet.css` | `.rel-fact.stack` | `gap: 3px` | `gap: var(--s1)` |  |
| `styles/admin/fleet.css` | `.rel-holdout` | `padding: 4px 0` | `padding: var(--s1) 0` |  |
| `styles/components.css` | `.readiness-check` | `padding: 10px 12px` | `padding: var(--s3) var(--s3)` |  |
| `styles/components.css` | `.legend` | `gap: 14px` | `gap: var(--s4)` |  |
| `styles/components.css` | `.legend i` | `margin-right: 5px` | `margin-right: var(--s1)` |  |
| `styles/components.css` | `.apps-field-err` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/components.css` | `.failure-log-tail-pre` | `padding: 10px 12px` | `padding: var(--s3) var(--s3)` |  |
| `styles/home.css` | `.home-hero-sub` | `margin-top: 18px` | `margin-top: var(--s5)` |  |
| `styles/home.css` | `.home-rail-track` | `padding: 6px 6px var(--home-rail-gutter, 22px)` | `padding: 6px 6px var(--home-rail-gutter, var(--s6))` | kept: both 6px: paired with `.home-rail-fade { right: -6px }`, which the snap may not move (positioning is out of scope) |
| `styles/home.css` | `.home-rail-track` | `margin: -6px` | `margin: -6px` | kept: -6px: the negative twin of the track's 6px padding; see `.home-rail-fade` |
| `styles/home.css` | `.home-feat-play svg` | `margin-left: 3px` | `margin-left: 3px` | kept: 3px: optical centring of the play triangle in its circle |
| `styles/home.css` | `.home-feat-body` | `padding: 15px 17px 17px` | `padding: var(--s4) var(--s4) var(--s4)` |  |
| `styles/home.css` | `.home-feat-body` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/home.css` | `.home-feat-name` | `padding-top: 8px` | `padding-top: var(--s2)` |  |
| `styles/home.css` | `.home-lib` | `padding: 0 var(--page-pad) 96px` | `padding: 0 var(--page-pad) calc(var(--s10) + var(--s7))` |  |
| `styles/home.css` | `.lib-head` | `gap: 14px` | `gap: var(--s4)` |  |
| `styles/home.css` | `.lib-head .segmented` | `margin-left: 6px` | `margin-left: var(--s2)` |  |
| `styles/home.css` | `.home .segmented button` | `padding: 6px 14px` | `padding: var(--s2) var(--s4)` |  |
| `styles/home.css` | `.src-head` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/home.css` | `.src-head .count` | `padding: 2px 9px` | `padding: var(--s1) var(--s2)` |  |
| `styles/home.css` | `.lib-tile .fnm` | `padding: 0 10px` | `padding: 0 var(--s3)` |  |
| `styles/home.css` | `.lib-tile-play` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/home.css` | `.lib-tile-play svg` | `margin-left: 2px` | `margin-left: 2px` | kept: 2px: optical centring of the play triangle in its circle |
| `styles/home.css` | `.lib-tile-play[data-action="resume"]` | `padding: 0 16px 0 13px` | `padding: 0 var(--s4) 0 var(--s3)` |  |
| `styles/home.css` | `.detail .d-inner` | `padding: 38px 42px` | `padding: var(--s8) var(--s8)` |  |
| `styles/home.css` | `.detail .d-inner` | `gap: 16px` | `gap: var(--s4)` |  |
| `styles/home.css` | `.d-specs .sp` | `padding: 10px 16px` | `padding: var(--s3) var(--s4)` |  |
| `styles/home.css` | `.d-specs .sp .v` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.d-rec` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/home.css` | `.d-rec .btn` | `margin-left: 10px` | `margin-left: var(--s3)` |  |
| `styles/home.css` | `.d-actions` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/home.css` | `.d-actions` | `padding-top: 14px` | `padding-top: var(--s4)` |  |
| `styles/home.css` | `.detail .lib-why ul` | `margin: 6px 0 0` | `margin: var(--s2) 0 0` |  |
| `styles/home.css` | `.detail .lib-why ul` | `padding-left: 18px` | `padding-left: var(--s5)` |  |
| `styles/home.css` | `.detail .lib-why ul` | `gap: 4px` | `gap: var(--s1)` |  |
| `styles/home.css` | `.detail .lib-why p` | `margin: 7px 0 0` | `margin: var(--s2) 0 0` |  |
| `styles/home.css` | `.detail .lib-why .btn` | `margin-top: 10px` | `margin-top: var(--s3)` |  |
| `styles/home.css` | `.qp-head` | `gap: 26px` | `gap: var(--s6)` |  |
| `styles/home.css` | `.qp-head` | `padding: 20px 26px 15px` | `padding: var(--s5) var(--s6) var(--s4)` |  |
| `styles/home.css` | `.qp-game` | `margin-top: 4px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.qp-spec` | `gap: 24px` | `gap: var(--s6)` |  |
| `styles/home.css` | `.qp-spec` | `padding-right: 44px` | `padding-right: var(--s9)` |  |
| `styles/home.css` | `.qp-spec .sp .v` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.qp-col` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/home.css` | `.qp-col` | `padding: 14px 18px` | `padding: var(--s4) var(--s5)` |  |
| `styles/home.css` | `.qp-row` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/home.css` | `.qp-row` | `padding: 7px 12px` | `padding: var(--s2) var(--s3)` |  |
| `styles/home.css` | `.qp-row .qr-sub` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.qp-row .qr-why` | `margin-top: 3px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.qp-row .qr-side` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/home.css` | `.qp-tag` | `padding: 3px 8px` | `padding: var(--s1) var(--s2)` |  |
| `styles/home.css` | `.seg-hint` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.seg-hint + .seg-hint` | `margin-top: 4px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.qp-foot` | `gap: 24px` | `gap: var(--s6)` |  |
| `styles/home.css` | `.qp-foot` | `padding: 14px 26px` | `padding: var(--s4) var(--s6)` |  |
| `styles/home.css` | `.qp-verdict` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/home.css` | `.qp-verdict .dot` | `margin-top: 5px` | `margin-top: var(--s1)` |  |
| `styles/home.css` | `.qp-acts` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/home.css` | `@media (max-width: 900px) › .detail .d-inner` | `padding: 26px 20px` | `padding: var(--s6) var(--s5)` |  |
| `styles/home.css` | `@media (max-width: 900px) › .qp-head` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/home.css` | `@media (max-width: 900px) › .qp-spec` | `gap: 14px` | `gap: var(--s4)` |  |
| `styles/home.css` | `@media (max-width: 900px) › .qp-foot` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.hud-bar` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.hud-bar` | `padding: 0 14px` | `padding: 0 var(--s3)` | down: 14px → 12px: HUD pill: the in-stream pill's fixed extent |
| `styles/hud.css` | `.hud-read` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.hud .metrics` | `gap: 14px` | `gap: var(--s3)` | down: 14px → 12px: HUD pill: the in-stream pill's fixed extent |
| `styles/hud.css` | `.hud .m b u` | `margin-left: 2px` | `margin-left: var(--s1)` |  |
| `styles/hud.css` | `.hud .codec-chip` | `padding: 2px 7px` | `padding: var(--s1) var(--s2)` |  |
| `styles/hud.css` | `.hud .summon kbd` | `padding: 1px 5px` | `padding: 1px var(--s1)` |  |
| `styles/hud.css` | `.hud .summon kbd` | `margin-left: 2px` | `margin-left: var(--s1)` |  |
| `styles/hud.css` | `.hud-ctl` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.hud-tabs` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.hud-ctl .div` | `margin: 0 2px` | `margin: 0 var(--s1)` |  |
| `styles/hud.css` | `.hud-root[data-axis="h"] .ib.danger` | `margin-left: 12px` | `margin-left: var(--s3)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .ib.danger` | `margin-top: 12px` | `margin-top: var(--s3)` |  |
| `styles/hud.css` | `.hud .swapping` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/hud.css` | `.pane-head` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.pane-head` | `padding: 11px 18px 0` | `padding: var(--s3) var(--s5) 0` |  |
| `styles/hud.css` | `.pane-head .segmented button` | `padding: 4px 10px` | `padding: var(--s1) var(--s3)` |  |
| `styles/hud.css` | `.qs-rail` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.qs-rail` | `padding: 8px 18px 14px` | `padding: var(--s2) var(--s5) var(--s4)` |  |
| `styles/hud.css` | `.qs .nm` | `padding: 7px 10px 8px` | `padding: var(--s2) var(--s3) var(--s2)` |  |
| `styles/hud.css` | `.qs-badge` | `padding: 2px 5px` | `padding: var(--s1) var(--s1)` |  |
| `styles/hud.css` | `.cols` | `gap: 26px` | `gap: var(--s6)` |  |
| `styles/hud.css` | `.cols` | `padding: 8px 18px 14px` | `padding: var(--s2) var(--s5) var(--s4)` |  |
| `styles/hud.css` | `.capture-cta` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.capture-cta` | `padding: 12px 14px` | `padding: var(--s3) var(--s4)` |  |
| `styles/hud.css` | `.ov-kv` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.ov-kv` | `padding: 6px 0` | `padding: var(--s2) 0` |  |
| `styles/hud.css` | `.combo` | `gap: 3px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.combo kbd` | `padding: 2px 6px` | `padding: var(--s1) var(--s2)` |  |
| `styles/hud.css` | `.col-note` | `margin-top: 4px` | `margin-top: var(--s1)` |  |
| `styles/hud.css` | `.col-lb` | `padding-bottom: 4px` | `padding-bottom: var(--s1)` |  |
| `styles/hud.css` | `.ctl-row` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.ctl-row` | `padding: 6px 0` | `padding: var(--s2) 0` |  |
| `styles/hud.css` | `.ctl-row .segmented button` | `padding: 5px 10px` | `padding: var(--s1) var(--s3)` |  |
| `styles/hud.css` | `.stat-card` | `padding: 9px 12px 6px` | `padding: var(--s2) var(--s3) var(--s2)` |  |
| `styles/hud.css` | `.stat-card` | `gap: 3px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.stat-card .vv u` | `margin-left: 3px` | `margin-left: var(--s1)` |  |
| `styles/hud.css` | `.stat-card svg` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/hud.css` | `.diag-grid` | `gap: 0 26px` | `gap: 0 var(--s6)` |  |
| `styles/hud.css` | `.diag-grid` | `padding: 8px 18px 14px` | `padding: var(--s2) var(--s5) var(--s4)` |  |
| `styles/hud.css` | `.diag-grid td` | `padding: 3px 0` | `padding: var(--s1) 0` |  |
| `styles/hud.css` | `.diag-extra` | `padding: 0 18px 14px` | `padding: 0 var(--s5) var(--s4)` |  |
| `styles/hud.css` | `.diag-extra .diag-table td` | `padding: 3px 0` | `padding: var(--s1) 0` |  |
| `styles/hud.css` | `.diag-advanced-toggle` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/hud.css` | `.hz-flag` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.hz-flag` | `padding: 2px 9px` | `padding: var(--s1) var(--s2)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .hud-bar` | `padding: 14px 0` | `padding: var(--s3) 0` | down: 14px → 12px: HUD pill: the in-stream pill's fixed extent |
| `styles/hud.css` | `.hud-root[data-axis="v"] .hud-bar` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .hud-read` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .metrics` | `gap: 10px` | `gap: var(--s2)` | down: 10px → 8px: HUD pill: the in-stream pill's fixed extent |
| `styles/hud.css` | `.hud-root[data-axis="v"] .hud-ctl` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .hud-ctl .div` | `margin: 2px 0` | `margin: var(--s1) 0` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .pane-head` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .pane-head` | `padding: 12px 14px 0` | `padding: var(--s3) var(--s4) 0` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .qs-rail` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .qs-rail` | `padding: 9px 14px 14px` | `padding: var(--s2) var(--s4) var(--s4)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .cols` | `gap: 14px` | `gap: var(--s4)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .cols` | `padding: 9px 14px 14px` | `padding: var(--s2) var(--s4) var(--s4)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .diag-grid` | `padding: 9px 14px 14px` | `padding: var(--s2) var(--s4) var(--s4)` |  |
| `styles/hud.css` | `.hud-root[data-axis="v"] .diag-extra` | `padding: 0 14px 14px` | `padding: 0 var(--s4) var(--s4)` |  |
| `styles/hud.css` | `.mic-hot` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/hud.css` | `.mic-hot` | `padding: 5px 11px 5px 9px` | `padding: var(--s1) var(--s3) var(--s1) var(--s2)` |  |
| `styles/hud.css` | `.ev-toast` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/hud.css` | `.ev-toast` | `padding: 11px 16px 11px 13px` | `padding: var(--s3) var(--s4) var(--s3) var(--s3)` |  |
| `styles/hud.css` | `.banner` | `gap: 16px` | `gap: var(--s4)` |  |
| `styles/hud.css` | `.banner` | `padding: 13px 20px` | `padding: var(--s3) var(--s5)` |  |
| `styles/hud.css` | `.banner .bt` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/hud.css` | `.banner .ba` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/login.css` | `.auth-scene` | `padding: clamp(1.5rem, 5vw, 3rem)` | `padding: clamp(var(--s6), 5vw, var(--s9))` |  |
| `styles/login.css` | `.auth-scene .card` | `padding: clamp(1.4rem, 3vw, 1.85rem)` | `padding: clamp(var(--s6), 3vw, var(--s7))` |  |
| `styles/login.css` | `.auth-scene .lockup` | `gap: .85rem` | `gap: var(--s3)` |  |
| `styles/login.css` | `.auth-scene .lockup` | `margin-bottom: 1.6rem` | `margin-bottom: var(--s6)` |  |
| `styles/login.css` | `.auth-scene form` | `gap: 1.05rem` | `gap: var(--s4)` |  |
| `styles/login.css` | `.auth-scene .field` | `gap: .45rem` | `gap: var(--s2)` |  |
| `styles/login.css` | `.auth-scene .input` | `padding: 0 .8rem` | `padding: 0 var(--s3)` |  |
| `styles/login.css` | `.auth-scene .pw .input` | `padding-right: 4.1rem` | `padding-right: var(--s10)` |  |
| `styles/login.css` | `.auth-scene .reveal` | `padding: .3rem .5rem` | `padding: var(--s1) var(--s2)` |  |
| `styles/login.css` | `.auth-scene .auth-note` | `padding: .5rem .7rem` | `padding: var(--s2) var(--s3)` |  |
| `styles/login.css` | `.auth-scene .check` | `gap: .75rem` | `gap: var(--s3)` |  |
| `styles/login.css` | `.auth-scene .step-indicator` | `margin-bottom: 1.3rem` | `margin-bottom: var(--s5)` |  |
| `styles/primitives.css` | `.page-head .sub` | `margin: 5px 0 0` | `margin: var(--s1) 0 0` |  |
| `styles/primitives.css` | `.crumbs` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.crumbs` | `margin-bottom: 10px` | `margin-bottom: var(--s3)` |  |
| `styles/primitives.css` | `.ae-fact` | `padding: 7px 0` | `padding: var(--s2) 0` |  |
| `styles/primitives.css` | `.stat-cards` | `gap: 12px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.stat-cards` | `padding: 8px 18px 14px` | `padding: var(--s2) var(--s5) var(--s4)` |  |
| `styles/primitives.css` | `[data-axis="v"] .stat-cards` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `[data-axis="v"] .stat-cards` | `padding: 9px 14px 14px` | `padding: var(--s2) var(--s4) var(--s4)` |  |
| `styles/primitives.css` | `.tabs` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.tab` | `padding: 0 14px` | `padding: 0 var(--s4)` |  |
| `styles/primitives.css` | `.tab .cnt` | `margin-left: 6px` | `margin-left: var(--s2)` |  |
| `styles/primitives.css` | `.empty h3` | `margin-bottom: 6px` | `margin-bottom: var(--s2)` |  |
| `styles/primitives.css` | `.btn` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.btn` | `padding: 0 13px` | `padding: 0 var(--s3)` |  |
| `styles/primitives.css` | `.btn-sm` | `padding: 0 12px` | `padding: 0 var(--s3)` |  |
| `styles/primitives.css` | `.btn-lg` | `padding: 0 24px` | `padding: 0 var(--s6)` |  |
| `styles/primitives.css` | `.gbar` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.sec-head .desc` | `margin-top: 5px` | `margin-top: var(--s1)` |  |
| `styles/primitives.css` | `.chip` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.chip` | `padding: 0 8px` | `padding: 0 var(--s2)` |  |
| `styles/primitives.css` | `.chip-sm` | `padding: 0 6px` | `padding: 0 var(--s1)` | down: 6px → 4px: chip: the 18px table-row chip |
| `styles/primitives.css` | `.shdr-title` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.tier` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.tier` | `padding: 0 11px` | `padding: 0 var(--s3)` |  |
| `styles/primitives.css` | `.caps` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.cap` | `padding: 3px 7px` | `padding: var(--s1) var(--s2)` |  |
| `styles/primitives.css` | `.field` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.input, .textarea, .select` | `padding: 0 13px` | `padding: 0 var(--s3)` |  |
| `styles/primitives.css` | `.textarea, textarea.input` | `padding: 9px 13px` | `padding: var(--s2) var(--s3)` |  |
| `styles/primitives.css` | `.select` | `padding-right: 32px` | `padding-right: var(--s7)` |  |
| `styles/primitives.css` | `.search` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.search` | `padding: 0 14px` | `padding: 0 var(--s4)` |  |
| `styles/primitives.css` | `.check` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.segmented` | `padding: 3px` | `padding: var(--s1)` |  |
| `styles/primitives.css` | `.segmented` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.segmented button` | `padding: 0 12px` | `padding: 0 var(--s3)` |  |
| `styles/primitives.css` | `.segmented button` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.kv` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.kv-row` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.qtable thead th` | `padding: 0 14px` | `padding: 0 var(--s3)` | down: 14px → 12px: table cell: horizontal padding on nowrap cells widens every table |
| `styles/primitives.css` | `.qtable tbody td` | `padding: 10px 14px` | `padding: var(--s3) var(--s3)` | down: 14px → 12px: table cell: horizontal padding on nowrap cells widens every table |
| `styles/primitives.css` | `[data-density="dense"] .qtable tbody td` | `padding: 5px 14px` | `padding: var(--s1) var(--s3)` | down: 14px → 12px: table cell: horizontal padding on nowrap cells widens every table |
| `styles/primitives.css` | `.cell-id` | `padding: 2px 6px` | `padding: var(--s1) var(--s1)` | down: 6px → 4px: chip: the id chip inside table cells |
| `styles/primitives.css` | `.rowflex` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.th-sort-btn` | `gap: 4px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.exp-in` | `padding: var(--s5) var(--s5) var(--s5) 44px` | `padding: var(--s5) var(--s5) var(--s5) 44px` | kept: 44px: the expansion's 44px text column, which the fleet rows align to |
| `styles/primitives.css` | `.exp-in .eyebrow` | `margin-bottom: 7px` | `margin-bottom: var(--s2)` |  |
| `styles/primitives.css` | `.exp-fact` | `padding: 5px 0` | `padding: var(--s1) 0` |  |
| `styles/primitives.css` | `.bar-row` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.signal` | `gap: 2.5px` | `gap: 2.5px` | kept: 2.5px: glyph geometry: four 3.5px bars drawn in a 16px box |
| `styles/primitives.css` | `.u2` | `gap: 5px 12px` | `gap: var(--s1) var(--s3)` |  |
| `styles/primitives.css` | `.u2 .bar-row` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.note` | `padding: 11px 13px` | `padding: var(--s3) var(--s3)` |  |
| `styles/primitives.css` | `.note svg` | `margin-right: 7px` | `margin-right: var(--s2)` |  |
| `styles/primitives.css` | `.menu, .row-menu-pop` | `padding: 5px` | `padding: var(--s1)` |  |
| `styles/primitives.css` | `.menu button, .row-menu-item` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.menu button, .row-menu-item` | `padding: 7px 10px` | `padding: var(--s2) var(--s3)` |  |
| `styles/primitives.css` | `.menu hr, .row-menu-pop hr` | `margin: 5px 0` | `margin: var(--s1) 0` |  |
| `styles/primitives.css` | `.row-menu-pop hr` | `margin: 4px 0` | `margin: var(--s1) 0` |  |
| `styles/primitives.css` | `.pal-in` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.pal-in` | `padding: 0 16px` | `padding: 0 var(--s4)` |  |
| `styles/primitives.css` | `.pal-list` | `padding: 6px` | `padding: var(--s2)` |  |
| `styles/primitives.css` | `.pal-sec` | `padding: 10px 12px 5px` | `padding: var(--s3) var(--s3) var(--s1)` |  |
| `styles/primitives.css` | `.pal-item` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.pal-item` | `padding: 8px 12px` | `padding: var(--s2) var(--s3)` |  |
| `styles/primitives.css` | `.pal-foot` | `padding: 9px 16px` | `padding: var(--s2) var(--s4)` |  |
| `styles/primitives.css` | `.pal-foot` | `gap: 16px` | `gap: var(--s4)` |  |
| `styles/primitives.css` | `.toast .t-body` | `margin-top: 2px` | `margin-top: var(--s1)` |  |
| `styles/primitives.css` | `.kpi-row` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.kpi-row` | `margin-top: 9px` | `margin-top: var(--s2)` |  |
| `styles/primitives.css` | `.kpi-unit` | `margin-left: 4px` | `margin-left: var(--s1)` |  |
| `styles/primitives.css` | `.kpi-trend` | `margin-bottom: 3px` | `margin-bottom: var(--s1)` |  |
| `styles/primitives.css` | `.kpi-meta` | `margin-top: 7px` | `margin-top: var(--s2)` |  |
| `styles/primitives.css` | `.stat` | `gap: 6px` | `gap: var(--s2)` |  |
| `styles/primitives.css` | `.stat .v small` | `margin-left: 4px` | `margin-left: var(--s1)` |  |
| `styles/primitives.css` | `.cset p` | `margin: 4px 0 0` | `margin: var(--s1) 0 0` |  |
| `styles/primitives.css` | `.fsec .fs-label p` | `margin: 6px 0 0` | `margin: var(--s2) 0 0` |  |
| `styles/primitives.css` | `.rung` | `gap: 10px` | `gap: var(--s3)` |  |
| `styles/primitives.css` | `.rung` | `padding: 9px 0` | `padding: var(--s2) 0` |  |
| `styles/primitives.css` | `.rung .ctl` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.spec-pill > span, .spec-pill .sp` | `gap: 5px` | `gap: var(--s1)` |  |
| `styles/primitives.css` | `.spec-pill > span, .spec-pill .sp` | `padding: 5px 11px` | `padding: var(--s1) var(--s3)` |  |
| `styles/session.css` | `.switcher` | `gap: 18px` | `gap: var(--s5)` |  |
| `styles/shell.css` | `.brand` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/shell.css` | `.cmdk` | `gap: 9px` | `gap: var(--s2)` |  |
| `styles/shell.css` | `.cmdk` | `padding: 0 14px` | `padding: 0 var(--s4)` |  |
| `styles/shell.css` | `.cmdk kbd` | `padding: 1px 5px` | `padding: 1px var(--s1)` |  |
| `styles/shell.css` | `.user-btn` | `gap: 8px` | `gap: var(--s2)` |  |
| `styles/shell.css` | `.user-btn` | `padding: 0 8px 0 6px` | `padding: 0 var(--s2) 0 var(--s2)` |  |
| `styles/shell.css` | `.user-pop` | `padding: 6px` | `padding: var(--s2)` |  |
| `styles/shell.css` | `.up-head` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/shell.css` | `.up-head` | `padding: 10px 10px 12px` | `padding: var(--s3) var(--s3) var(--s3)` |  |
| `styles/shell.css` | `.up-head` | `margin-bottom: 6px` | `margin-bottom: var(--s2)` |  |
| `styles/shell.css` | `.up-item` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/shell.css` | `.up-item` | `padding: 9px 10px` | `padding: var(--s2) var(--s3)` |  |
| `styles/shell.css` | `.up-div` | `margin: 6px 4px` | `margin: var(--s2) var(--s1)` |  |
| `styles/shell.css` | `.rail` | `padding: var(--s3) 10px` | `padding: var(--s3) var(--s2)` | down: 10px → 8px: rail: the collapsed rail is 60px wide |
| `styles/shell.css` | `.rail` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/shell.css` | `.rail-item` | `gap: 11px` | `gap: var(--s3)` |  |
| `styles/shell.css` | `.rail-item` | `padding: 0 10px` | `padding: 0 var(--s2)` | down: 10px → 8px: rail: the collapsed rail is 60px wide |
| `styles/shell.css` | `.mk` | `padding: 1px 6px` | `padding: 1px var(--s1)` | down: 6px → 4px: rail: the collapsed rail is 60px wide |
| `styles/shell.css` | `.rail-sec` | `padding: var(--s4) 10px var(--s2)` | `padding: var(--s4) var(--s2) var(--s2)` | down: 10px → 8px: rail: the collapsed rail is 60px wide |
| `styles/shell.css` | `.nav` | `gap: 2px` | `gap: var(--s1)` |  |
| `styles/shell.css` | `.nav a` | `padding: 8px 13px` | `padding: var(--s2) var(--s3)` |  |
| `styles/shell.css` | `@media (max-width: 820px) › .tabbar a` | `gap: 3px` | `gap: var(--s1)` |  |
| `styles/shell.css` | `@media (max-width: 760px) › .user-btn` | `gap: 7px` | `gap: var(--s2)` |  |
| `styles/shell.css` | `@media (max-width: 760px) › .user-btn` | `padding-inline: 6px` | `padding-inline: var(--s2)` |  |
