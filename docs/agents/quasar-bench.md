# quasar-bench for agents without skill support

This is the quasar-bench server's own paste-in guide (its `AGENTS.md`, from the
`skills.tar.gz` a bench server publishes, qbench 1.7.0), reproduced verbatim below
the line. Harnesses that load Agent Skills get the same content as the
`quasar-bench*` skills instead — install them with the server's `install.sh`. For
WHEN this repo requires bench evidence (the landing gate, sprint reports, review),
see [`AGENTS.md`](../../AGENTS.md) "Performance evidence (quasar-bench)".

Two repo specifics on top of it:

- `make bench-check` is `qbench check` run from the repo root with the exit code
  passed through; `make bench-status` is `qbench sprint status` / the review list.
- The server is "your bench server (`$BENCH_URL`)". Nothing in this repository names
  it: qbench reads `BENCH_URL`, else `~/.config/qbench/url`. Never write its
  address into a commit, an issue or a doc; cite reports by path
  ("bench sprint accreleus/quasar c15").

---

<!-- Paste this section into AGENTS.md (or CLAUDE.md, GEMINI.md, …) for a harness
     that does not load Agent Skills. Harnesses that do should install the skills
     next to this file instead: curl -fsSL "$BENCH_URL/install.sh" | sh -->

## quasar-bench (Quasar performance benchmarks)

quasar-bench (the server at `$BENCH_URL`, also recorded in `~/.config/qbench/url`) stores Quasar harness runs, judges
them, and keeps the sprint reports Michael reviews. Talk to it with `qbench`.
Every command takes `--json`, and `--help` lists the flags. Setup: run
`qbench doctor`. If it's missing, install it with
`curl -fsSL "$BENCH_URL/install.sh" | sh`. The key lives in
`BENCH_KEY` or `~/.config/qbench/key` (mode 600). Never print the key. On exit
code 5, ask Michael for a key.

- **Did my change regress anything?** Run `qbench check` in the Quasar
  checkout (add `--window impaired` for impairment experiments). Exit 0 means
  clean, 3 regressed, 4 nothing comparable (not a pass). For each regressed
  scenario, read `qbench runs summary RUN` for validity and intended-vs-actual
  mismatches before you blame the code.
- **Sprint report:** write the `sections` JSON (goals, changes, decisions,
  tests, metrics, risks, follow_ups; the shape is in
  `client/examples/sprint.json`), then
  `qbench sprint put --repo accreleus/quasar --sprint SLUG --title … --sections F --run BEFORE --run AFTER`
  and attach evidence with `qbench sprint attach`. For review, check
  `qbench sprint status …` once when you resume work. On `changes_requested`:
  fix each anchored comment, re-run the same `sprint put`, reply with
  `qbench sprint comments … --anchor A --add "…"`, then run
  `qbench sprint resolve … ID…`. Review status is Michael's to set. Never
  approve your own report.
- **Posting a run:** use
  `qbench run new --suite S --scenario SC --repo accreleus/quasar --commit SHA --tag k=v`,
  then `run samples` (add `--replace --expect src=N` for re-folds),
  `run event --type netem.impair|netem.clear`, `run mark RUN PHASE start|end`,
  `run capture RUN FILE --role video --started-at-ms MS` and `run finish`.
  Check it with `qbench runs summary`, and mark bad runs
  `qbench run validity RUN contaminated --reason …` instead of deleting them.
- **Roadmap evidence:** `qbench evidence gaps` lists board items that no
  approved report cites yet (`uncited`, or `cited_unapproved`, meaning it's
  waiting on review). Cite issues in the report. Bench never writes to
  GitHub. To post a snippet when asked, pipe `qbench sprint snippet …` into
  `gh issue comment N -F -`.

Things that trip agents up: whole-run numbers dilute an impairment experiment,
so scope with `--window`. Non-valid runs are excluded from every verdict and
named in it. Bitrate/setpoint and unregistered metrics are neutral and never
flagged. A run without `--commit` is invisible to `check`. The full API is
at `/agents.md` and `/openapi.yaml` on the server.
