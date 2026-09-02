---
name: quasar-ui-audit
description: Use when you need automated evidence for a UI change or sweep — visual audit, UI regression check, before/after evidence for a UI change, "did my CSS change break anything", scoped page audit, #459-style sweep.
---

# Quasar UI audit

The mechanism is three DX verbs (`scripts/dx/uiaudit.sh capture|report|ab`,
also reachable via `make ui-audit` / `ui-audit-routes` / `ui-audit-ab`) —
deterministic screenshot + DOM-metric capture and self-contained HTML
rendering. **Your job is the judgment layer on top**: reading the metrics and
screenshots, comparing against `design_handoff_v3/` mockups, and writing
`findings.json`. Never re-implement the capture/scan logic inline — it
already exists in `scripts/dx/uiaudit/capture.mjs`.

## Prerequisite: dev-agent auth

`capture` mints admin/user sessions via `scripts/dx/agentcreds.sh`, which
needs `QUASAR_DEV_AGENT_AUTH=1` on the target stack and a per-boot key. Fetch
the key over the persistent ssh control socket rather than guessing:

```
qhost --host gpu-test sh \
  'sudo docker exec deploy-quasar-control-plane-1 cat /run/quasar/dev-agent-key'
```

`qhost` resolves the role, the ssh alias/key and the persistent control socket from
`_shared/hosts.json` — never hand-type an address or a key path here.

Pass it with `--key`/`KEY=` or export `QUASAR_DEV_AGENT_KEY`. Never echo the
key value in chat or logs.

## Coverage-only sweep

```
make ui-audit URL=<the gpu-test host's api_external from hosts.json> KEY=<dev-agent-key>
```

or scope it: `make ui-audit-routes URL=... ROUTES=admin-images,admin-users`
(route ids are in `scripts/dx/uiaudit/routes.json`). Writes evidence under
`.uiaudit/<timestamp>/` and a `report.html` — every surface listed CLEAN
unless its `*.metrics.json` flags something (page/element overflow, a form
control missing the `.select`/`.input` design-system class, console errors,
or a redirect). That report is a starting point, not the final word — it
can't see "looks wrong" against a mockup.

## Judgment pass -> findings.json

After capture, open the screenshots (`.uiaudit/<ts>/<routeId>--<WxH>.png`)
and `*.metrics.json`, compared against the matching
`design_handoff_v3/screens/*.html` mockup (read the design handoff
README first — don't invent style rules). For anything worth flagging, write
a `findings.json` array:

```json
[{
  "id": "F-01",
  "route_id": "admin-users",
  "severity": "broken",           // broken | inconsistent | polish
  "surface": "Admin · Users",
  "route": "/admin/users",
  "title": "short one-line summary",
  "description": "what's wrong, reproduction, comparison to mockup",
  "component_file": "web/src/pages/admin/AdminUsers/index.tsx",
  "mockup": "design_handoff_v3/screens/admin-console-v3.html",
  "screenshot": "admin-users--1280x900.png"
}]
```

`screenshot` is a filename relative to the evidence dir (from
`manifest.json`'s `captured[]`). If you need to call out a specific region,
describe the bounding area in `description` rather than hand-editing images —
keep this simple.

Then: `scripts/dx/uiaudit.sh report --evidence .uiaudit/<ts> --findings findings.json`
(or `--out` to place it elsewhere). This produces the full audit report
(summary table + per-finding sections + coverage appendix), self-contained
HTML, no external assets.

## Before/after (regression check for a specific change)

```
bash scripts/dx/uiaudit.sh capture --url <base> --out .uiaudit/before ...
# apply the change, redeploy
bash scripts/dx/uiaudit.sh capture --url <base> --out .uiaudit/after ...
make ui-audit-ab BEFORE=.uiaudit/before AFTER=.uiaudit/after
```

The A/B report diffs common route/widths (overflow count, unstyled-control
count, console-error count, title/redirect changes) and calls out routes
present in only one run. Treat a `regression` verdict as blocking; read the
side-by-side screenshots before dismissing anything as noise.

## Notes

- `.uiaudit/` evidence dirs are never committed — attach the generated report
  HTML to share results.
- `capture` only sees deterministic signal (overflow, unstyled controls,
  console errors, redirects) — not color/spacing/typography drift from a
  mockup. That's why the judgment pass is required before a report is
  trustworthy as a "did we break anything" answer.
