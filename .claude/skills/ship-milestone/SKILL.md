---
name: ship-milestone
description: Drive any GitHub milestone to merged in one re-runnable pass — plan the issue DAG, dispatch model-per-Tier subagents to implement + verify + visually inspect, run each issue's done-gate, open stacked PRs, resolve review comments under a 3-strike rule, merge via a ready-for-review/ready-to-merge label handshake, and emit a single self-contained HTML completion report. Triggers — "ship the milestone", "run the UI overhaul loop", "work the milestone issues", "/ship-milestone". Reusable across milestones (issues must carry the Tier/Depends/Scope/Acceptance header).
arguments:
  - name: milestone
    description: The exact GitHub milestone title (e.g. "Web UI Overhaul"). If absent, list open milestones and ask the user to pick one.
    required: true
  - name: mode
    description: "Execution mode: (none) = full pass · plan = plan + ready set only · status = status table only · report = (re)generate the HTML completion report only · <ISSUE-KEY>/#NNN = just that issue."
    required: false
---

# ship-milestone — generic milestone orchestrator

**Run me on a high model (opus).** You are the **orchestrator** for a GitHub milestone in `accreleus/quasar`. You **delegate** all implementation, verification, visual inspection, and review-resolution to subagents via the **Agent tool**, choosing each subagent's model from the issue's declared `Tier:`. **You do not write feature code yourself** — you plan, dispatch, gate, escalate, and report.

Run **exactly one pass**, then stop and print the status table + report path. The operator re-runs the skill after the reviewer acts (or wraps it in `/loop`). Never idle-poll for human review inside a pass.

## Arguments
- First token = the **milestone title**, quoted (e.g. `"Web UI Overhaul"`). Required. If absent, use the **AskUserQuestion** tool: fetch open milestones with `gh milestone list --repo accreleus/quasar --state open --json title,number` and present them as **multiple-choice options** (not free-text) so the user can pick by number.
- Optional mode after it: *(none)* → full pass · `plan` → plan + ready set only · `status` → status table only · `report` → (re)generate the HTML completion report only · `<ISSUE-KEY>`/`#NNN` → just that issue. If mode is also absent, use the **AskUserQuestion** tool to present the five mode choices as a labeled multiple-choice list before proceeding.

---

## The issue contract (what makes a milestone shippable here)
Data-driven; any milestone works if its issues carry this header (see the UI-overhaul issues for the reference shape):
- **`Tier:`** `Haiku|Sonnet|Opus` *(+ `human sign-off`)* — model + frozen-contract flag.
- **`Depends on:` / `Blocks:`** — the dependency DAG (= stacking + ordering).
- **`### Scope`** — deliverables. **`### Acceptance`** — the **done-gate**: concrete runnable checks (source of truth for "tests green"); some bullets are human-sign-off gates.
- *Optional* **`Ship-with: <KEY>`** — co-ship tightly-coupled tickets in one branch/PR.

Missing header → treat as `Tier: Sonnet`, no deps, best-effort gate; flag as `NON-CONFORMING` in the report.

## PR granularity — **one PR per issue (default)**
Issues are atomic, independently-gated tickets, so one PR per issue gives small reviewable diffs, per-issue evidence, and merge-when-ready without blocking the milestone. **Never** open a single mega-PR — it's unreviewable. The **HTML completion report is the operator's consolidated single-surface review**, so granular PRs cost nothing in review overhead. Only collapse issues that declare `Ship-with:`.

---

## Config (resolve in preflight)
Canonical values (repo slug, branch prefix, label names and meanings, tier→model map, done-gate commands per area, strike limit, and output paths) live in **`.claude/skills/ship-milestone/config.json`** — that file is the single source of truth. Read it at the start of every pass; do not duplicate the literal values here.

Summary of the handshake semantics (see `config.json` for the exact strings):
- **Branch:** `ship/<KEY>` (`UI-01`, `CP-01`, … or `issue-<NNN>`). PR body contains `Closes #<n>`.
- **Model map:** derived from `tier_model_map` in `config.json`. Subagents: `executor` (implement/resolve), `verifier`/`code-reviewer` (verify), `designer` (visual inspection).
- **Labels** (ensure they exist; `gh label create` if missing):
  - **`ready-for-review`** — *you* apply it only after implement + verify + green gate (+ visual PASS for UI). The reviewer (Alice) reviews only PRs that carry it.
  - **`ready-to-merge`** — *Alice* applies it when good to merge. **This label is your only merge trigger.**
  - **`needs-operator`** — *you* apply it on a 3-strike escalation (below).
- **Done-gate:** per-area commands are in `config.json` → `done_gate`. The three areas are `web` (Node ≥ 22 + npm; docker fallback), `control_plane` (`go-test-db`, requires live Postgres), and `node_agent` (cargo inside dev container).
- **Visual evidence:** UI issues capture screenshots → `ui-evidence/<KEY>/` (also committed on the branch for GitHub linking).
- **Report:** self-contained HTML at `ui-evidence/<milestone-slug>-report.html` (gitignored). Template: `.claude/skills/ship-milestone/assets/report-template.html`.
- **Strike limit:** **3** Alice review rounds → escalate to operator.
- **DONE (enforced):** complete only when **(a)** every Scope deliverable is implemented, **(b)** the Acceptance gate is green with captured evidence, **(c)** UI work passed visual inspection, **(d)** the PR carries `ready-to-merge`, and **(e)** any human sign-off is recorded. "It builds" ≠ done; an unrun gate ≠ done.

---

## Phase 1 — Plan
Run `.claude/skills/ship-milestone/scripts/ms-status "<milestone title>"` first — it queries `gh` and prints a `KEY | TIER | STATE | PR | PR-STATE | DEPS | LABELS` table in one shot, saving repeated `gh` calls. Then fetch full issue bodies for DAG parsing: `gh issue list --milestone "<title>" --state open --json number,title,body,labels`. Parse `Tier:` / `Depends on:` headers; build the DAG. An issue is **ready** when deps are merged to `develop` (or don't block code it touches). Skip umbrella/tracker issues. Print the ordered ready set (`KEY · Tier · deps · state`). `plan` mode stops here.

## Phase 2 — Advance each ready issue
Respect the DAG; independent ready issues may run concurrently (WIP ~3). Per issue (or `Ship-with:` group):
1. **Branch base (stacking):** dep PR open → base on its branch (true stacked PR); deps merged/none → base on **`develop`** (never `main` — `main` is production and only a human-signed-off promotion goes there). Keep stacks shallow; restack as deps merge. Create `ship/<KEY>` in an isolated worktree (`.claude/worktrees/<KEY>`).
2. **Implement** — `executor` Agent at the issue's `Tier:`. Give it the issue body, design refs + its `Read first`, touched paths, worktree+branch, exact gate commands. It implements all of `Scope`, adds/adjusts tests, **runs the gate to green itself**, returns DONE only with the gate's real output (or BLOCKED w/ specifics). Tell it: use codebase-memory-mcp (`search_graph`/`trace_path`/`get_code_snippet`) for code exploration; honor conventions (web=node:22+npm, Rust fmt/clippy, Go fmt/vet, conventional commits); **never touch frozen `protocol/` contracts** unless the issue authorizes an additive amendment. Commit + push.
3. **Independent verify** — a **separate** `verifier`/`code-reviewer` Agent re-runs the gate from clean and confirms every Scope + Acceptance bullet (no self-approval). Gaps → bounce to step 2.
4. **Visual inspection (UI issues only** — touches a rendering path or Acceptance names a visible surface): a `designer` Agent builds + previews the SPA, drives a headless browser (Playwright MCP or a node Playwright script) to the affected routes, captures screenshots in **light + dark** (+ dense if relevant) to `ui-evidence/<KEY>/`, and runs the `visual-verdict` skill against the matching `design_handoff_v3/screens/` reference → PASS/FAIL + notes. A visual FAIL bounces to step 2. (Backend/no-surface issues skip this.)
   - Steps 2↔(3,4) loop at most **3** cycles, then stop and report `BLOCKED` for the operator.
5. **Open the PR + evidence** — once verify (and visual, if UI) pass: construct the PR body by filling **`_shared/pr-body.tmpl`** (closes, what-changed bullets, done-gate evidence, decisions/escalations, stacked-on). Then: `gh pr create --base <base> --head ship/<KEY> --title "<KEY> — <summary>" --body "$(cat <filled-body>)"`; carry the milestone label; **post the screenshots** as a PR/issue comment (commit them under `ui-evidence/<KEY>/`, embed via the pushed raw URLs) with the visual verdict; then **apply `ready-for-review`** and request Alice.
6. **Sign-off gate** — `Tier: + human sign-off` or frozen-contract issue (e.g. CP-01 → `protocol/control-api.md`): open the PR **draft**, do **not** apply `ready-for-review`, comment that it needs Opus review + operator sign-off, **hard-stop**.

## Phase 3 — Review pickup + 3-strike rule
For each open PR with **CHANGES_REQUESTED** / unresolved threads (Alice reviewed a `ready-for-review` PR):
1. **Count strikes** = number of Alice `CHANGES_REQUESTED` reviews on this PR (`gh pr view <n> --json reviews`).
2. **If strikes ≥ 3:** stop iterating — remove `ready-for-review`, apply **`needs-operator`**, and post **one decision-request comment**: (a) what you changed across the rounds, (b) the specific unresolved point(s), (c) the exact decision/options you need from the operator and *why you can't resolve it autonomously* (conflicting feedback, ambiguous spec, scope/frozen-interface question). Leave the PR open; surface under `NEEDS-OPERATOR`; **do not touch it again** until the operator responds.
3. **Else (strikes < 3):** remove `ready-for-review`; collect feedback (`gh pr view --json reviewDecision,reviews,comments` + inline `gh api repos/accreleus/quasar/pulls/<n>/comments`); dispatch a resolution `executor` Agent at the issue's `Tier:` — address each comment, **re-run the gate green**, re-do visual inspection if UI changed, reply to each thread referencing the fix, push; re-verify; **re-apply `ready-for-review`** + re-request Alice. (Reply to threads; don't resolve on Alice's behalf.)

## Phase 4 — Merge on `ready-to-merge` + restack
All milestone PRs target **`develop`**. Merge **only when** the PR carries **`ready-to-merge`** (Alice's signal) **AND** its gate is green **AND** sign-off (if required) is recorded. **Promoting `develop` → `main` is out of scope for this skill** — it is production and needs explicit human sign-off; never open or merge that PR autonomously. Then: merge **bottom-up** (`gh pr merge <n> --squash --delete-branch`); **restack dependents** (rebase onto `develop`, force-push, set PR `--base develop`, re-run gate green, re-apply `ready-for-review` if applicable). Never merge a draft/sign-off-gated PR; never merge on green or a bare approval — only `ready-to-merge`.

## Phase 5 — Report (status, every pass)
Table: `KEY | Tier | branch | PR# | labels | strikes | state | gate | visual | next-action`. States: `PENDING-DEPS → READY → IN-PROGRESS → GATE-GREEN → VISUAL-PASS → REVIEW-REQUESTED → CHANGES-REQUESTED → re-REVIEW → READY-TO-MERGE → MERGED` (+ `SIGN-OFF-HOLD`, `NEEDS-OPERATOR`, `BLOCKED`, `NON-CONFORMING`).

## Phase 6 — HTML completion report
After the pass (and on `report` mode), generate/refresh a **self-contained** HTML report at `ui-evidence/<milestone-slug>-report.html`. A `writer`/`designer` Agent fills **`.claude/skills/ship-milestone/assets/report-template.html`** — copy the template, then substitute every `{{placeholder}}` and expand each `<!-- REPEAT:... -->` block for the actual issues. Do not rebuild the structure from scratch.

Fields to fill per issue: `{{issue_key}}`, `{{issue_title}}`, `{{issue_url}}`, `{{issue_number}}`, `{{pr_url}}`, `{{pr_number}}`, `{{tier}}`, `{{state_label}}`, `{{state_css}}` (badge CSS suffix), `{{scope_bullet}}` (one `<li>` per deliverable), `{{gate_evidence}}` (exact command output), `{{implementation_notes}}`, and for UI issues `{{screenshot_b64}}` / `{{reference_b64}}` / `{{visual_notes}}`. Summary counts at the top: `{{count_merged}}`, `{{count_waiting}}`, `{{count_needs_operator}}`, `{{count_blocked}}`, `{{count_remaining}}`.

The report is self-contained (inline CSS, base64 images — no external dependencies) so it is one portable file. This is the operator's single review surface — end the pass by printing its path and offering to open/deliver it.

---

## Guardrails
- **One pass, then stop.** Idempotent — re-detect state from branches/PRs/labels each run; never duplicate work.
- **DONE = deliverables + evidenced green gate + visual PASS (UI) + `ready-to-merge`.** Never report done on "it builds" or an unrun gate.
- **Separation of duties:** implement / verify / visual are distinct agents; never self-approve.
- **Label handshake is the contract:** you apply `ready-for-review` (post verify+green+visual); Alice applies `ready-to-merge` (the only merge trigger). Sign-off/frozen issues hard-stop for operator + Opus.
- **3-strike rule:** after 3 Alice review rounds, park the PR with a precise decision-request comment + `needs-operator`; don't grind further.
- **Model thrift:** cheapest model per `Tier:`; orchestrator reasoning stays here, heavy work goes to subagents.
- **Isolation & shallow stacks:** worktree per issue; respect the DAG; stack only along hard deps; restack as deps land.
- **Repo invariants:** keep the control-plane/node-agent split and the single-SPA server-enforced auth; don't touch frozen `protocol/` contracts without an authorizing issue; escalate frozen-interface/latency/security ambiguity to the operator.
