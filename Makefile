# Quasar — developer-experience façade.
#
# This Makefile is deliberately THIN. Every target delegates to a script under
# scripts/dx/ or to an existing canonical script (scripts/verify.sh,
# deploy/build-images.sh, deploy/redeploy.sh). No logic lives here, so the same
# behaviour is available to CI, to an agent, and to a human typing the script
# path directly.
#
#   make            # same as `make help`
#   make help       # this list
#
# Two knobs are shared by many targets:
#   HOST=local|<role-or-host>  which stack to operate on. UNSET means local.
#                      A non-local HOST is a role (gpu-test, aux-infra) or host
#                      name resolved from .claude/skills/_shared/hosts.json
#                      (see hosts.example.json for the schema). It is accepted
#                      only by status/health/logs/logs-follow and the mutating
#                      verbs up/down/restart/rebuild/redeploy-cp — and every
#                      mutating verb refuses unless you TYPE HOST=<role-or-host>.
#                      `make reset` refuses any non-local HOST unconditionally.
#                      Example: HOST=gpu-test.
#   QUASAR_INSTANCE    compose project name. Defaults to a hash of this
#                      worktree's path, so two worktrees never collide on
#                      container names, volumes, or published ports.
#
# NOTE: HOST is intentionally NOT given a default here. An unset HOST is how the
# guards tell "the operator asked for a remote host" apart from "it was the
# default".

SHELL := /bin/bash
.DEFAULT_GOAL := help

# Repo constants, not knobs. `override` so `make clean WEB='x; rm -rf /'` cannot
# turn one of them into a command — they are the only variables still spliced
# into a recipe line (see the passthrough note below).
override DX  := scripts/dx
override CP  := control-plane
override WEB := web

# The file `help` greps. MAKEFILE_LIST is make-maintained, but a command-line
# assignment still beats make's own — `make help MAKEFILE_LIST='Makefile; whoami'`
# was the same hole as the knobs below, so `help` greps this instead. If this
# Makefile ever `include`s another, add it here.
override HELP_MAKEFILES := Makefile

# ── Passthrough knobs ───────────────────────────────────────────────────────
# NOTHING a caller can set is interpolated into a recipe line (#550). Make
# expands `$(ARGS)` into the recipe's command TEXT, which /bin/bash then parses,
# so `make bench-run ARGS='--secs 5; whoami'` used to run `whoami` at the MAKE
# layer, before any script existed to validate it — and `"$(SID)"` was no better,
# since a `"` closes the quotes and a backtick is live inside them. The realistic
# vector is a copy-pasted `make` line, not a hostile local user.
#
# So every knob below travels by ENVIRONMENT, which a shell never re-parses. The
# receiving script turns it back into arguments via dx_env_argv (scripts/dx/
# common.sh), which splits on whitespace only and shape-checks each token.
# HOST is deliberately absent from the export list — see the NOTE above.

# Log knobs: make logs S=quasar-control-plane N=500
S ?=
N ?= 200
export S N

# agent-creds passthrough: make agent-creds ARGS='--role admin --ttl 1h'
ARGS ?=
export ARGS

# session-* / soak / ladder: make session-display SID=<id> ARGS='--stream 1280x720'
SID ?=
export SID

# validate passthrough: make validate LEVEL=ui|api|session|all TARGET=local|<url> KEEP=1
LEVEL ?= ui
TARGET ?= local
KEEP ?= 0
export LEVEL TARGET KEEP

# bench passthrough: make bench-submit DIR=<run dir> ARGS='--suite ... --tag k=v'
DIR ?=
export DIR

# ui-audit passthrough: make ui-audit URL=https://host:8443 [KEY=<dev-agent-key>]
#   make ui-audit-routes URL=... ROUTES=admin-images,admin-users
#   make ui-audit-ab BEFORE=.uiaudit/<ts> AFTER=.uiaudit/<ts>
URL ?=
ROUTES ?= all
KEY ?=
BEFORE ?=
AFTER ?=
OUT ?=
RUN ?= latest
NAME ?=
export URL ROUTES KEY BEFORE AFTER OUT RUN NAME

.PHONY: help init doctor config-check verify docs-metrics-sync docs-trace \
	test test-go test-rust test-web \
        test-db preflight up down restart rebuild redeploy-cp status health logs logs-follow \
        dev-web dev-cp diagnose diagnose-bundle clean reset agent-creds validate \
        ui-audit ui-audit-routes ui-audit-ab session-display session-soak abr-ladder \
        bench-submit bench-retro bench-run bench-suite bench-budget bench-baseline \
        homes-gc \
        nightly-budget-install nightly-budget-run nightly-budget-status qa \
        admin-token session-list session-verdict session-metrics session-trace \
        session-bundle session-capture session-logs session-diagnose \
        report-publish report-attach report-url

## ── Getting started ─────────────────────────────────────────────────────────

help: ## Show this help (default target)
	@printf '\nQuasar make targets\n\n'
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(HELP_MAKEFILES) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2}'
	@printf '\nKnobs: HOST=local|<role-or-host> (e.g. gpu-test)  S=<service>  N=<lines>  CONFIRM=reset|reset-data  ARGS=<agent-creds args>\n'
	@printf '       REF=<branch|sha> (rebuild, redeploy-cp — defaults to the branch the host is ALREADY on, never main)\n'
	@printf '       LEVEL=ui|api|session|all  TARGET=local|<base-url>  KEEP=1 (validate — leave the local stack up)\n'
	@printf '       URL=<base-url>  ROUTES=all|id,id,...  KEY=<dev-agent-key>  BEFORE=<dir>  AFTER=<dir>  OUT=<dir> (ui-audit)\n'
	@printf '       RUN=<run-id>|latest  NAME=<suite/scenario> (bench-budget, bench-baseline)\n'
	@printf '       SID=<uuid>|latest  WINDOW=<from_ms>,<to_ms>  SINCE=10m  N=<rows>  GREP=<pat>  JSON=1 (session-*)\n'
	@printf '       KIND=pipeline_dot|encoder_props|burst_stats|all  WINDOWS=<n>  WINDOW_MS=<ms>  (session-capture)\n'
	@printf 'Instance: %s\n\n' "$$($(DX)/common.sh instance)"

init: ## Make a fresh clone/worktree workable (submodule, .env, devtools image, doctor)
	@bash $(DX)/init.sh

doctor: ## Check this machine can do Quasar work (remote reachability is advisory)
	@bash $(DX)/doctor.sh

config-check: ## Validate every compose file set; advise on deploy/.env drift
	@bash $(DX)/config_check.sh

## ── Verify & test ───────────────────────────────────────────────────────────

verify: ## Verify the DX tooling itself (shellcheck, guards, redaction goldens)
	@bash $(DX)/tests/run.sh

test: test-go test-rust test-web ## Run the go, rust and web suites (containerized)

test-go: ## Control-plane: gofmt, build, vet, unit tests (no DB — see test-db)
	@bash $(DX)/common.sh require-local test-go
	@bash scripts/verify.sh control

test-rust: ## Node-agent: fmt, clippy -D warnings, unit tests (in the dev container)
	@bash $(DX)/common.sh require-local test-rust
	@bash scripts/verify.sh agent

test-web: ## Web SPA: install, typecheck, unit tests, production build
	@bash $(DX)/common.sh require-local test-web
	@bash scripts/verify.sh web

test-db: ## Control-plane DB tests against a FRESH ephemeral Postgres (-p 1)
	@bash $(DX)/testdb.sh

docs-metrics-sync: ## Copy the metric manifest to its Go + web consumers (drift-tested both sides)
	@bash $(DX)/metrics_manifest.sh sync

docs-trace: docs-metrics-sync ## Regenerate the trace-format.md metric table from the manifest
	@python3 $(DX)/gen_trace_docs.py

preflight: doctor config-check verify ## Pre-push gate: doctor + config-check + verify
	@printf 'preflight complete — see the RESULT lines above\n'

## ── Stack ───────────────────────────────────────────────────────────────────

up: ## Start the stack (local: agentless compose; remote HOST must be explicit)
	@bash $(DX)/stack.sh up

down: ## Stop the stack, preserving volumes
	@bash $(DX)/stack.sh down

restart: ## Restart the stack in place
	@bash $(DX)/stack.sh restart

rebuild: ## Rebuild images then restart (remote delegates to build-images.sh + redeploy.sh)
	@bash $(DX)/stack.sh rebuild

redeploy-cp: ## Rebuild + recreate ONLY the Go control-plane (~1 min; no Rust/GStreamer)
	@bash $(DX)/stack.sh redeploy-cp

status: ## Per-container state: healthy | degraded | stopped | failed
	@bash $(DX)/stack.sh status

health: ## Probe the control-plane /health endpoint
	@bash $(DX)/stack.sh health

logs: ## Bounded logs — make logs S=<service> N=<lines>
	@bash $(DX)/stack.sh logs

logs-follow: ## Follow logs — make logs-follow S=<service>
	@bash $(DX)/stack.sh logs-follow

## ── Inner loop ──────────────────────────────────────────────────────────────

dev-web: ## Vite dev server for web/ (needs Node >= 22)
	@bash $(DX)/common.sh require-local dev-web
	@cd $(WEB) && npm run dev

dev-cp: ## Run the control-plane from source against the local stack's Postgres
	@bash $(DX)/common.sh require-local dev-cp
	@cd $(CP) && DATABASE_URL="postgres://quasar:quasar-local-dev@127.0.0.1:$$($(DX)/common.sh ports | sed 's/.*pg=\([0-9]*\).*/\1/')/quasar?sslmode=disable" go run ./cmd/quasar-control

## ── Dev-only agent auth (#399) ──────────────────────────────────────────────

agent-creds: ## Mint a throwaway dev-agent identity — ARGS='--role admin --ttl 1h'
	@bash $(DX)/agentcreds.sh

## ── Session display (adaptive external resolution) ─────────────────────────

session-display: ## PATCH a live session's display (stream/render/ui-scale) — SID=<id> ARGS='--stream 1280x720' HOST=gpu-test
	@bash $(DX)/session_display.sh

session-soak: ## 3-min external-resolution soak (bad-connection sim) against a live session — SID=<id>|latest ARGS='--duration 180' HOST=devbox
	@bash $(DX)/session_soak.sh

abr-ladder: ## D8 ABR-ladder acceptance: netem levels x observe soaks — SID=<id>|latest HOST=devbox ARGS='--dwell 240'
	@bash $(DX)/abr_ladder_netem.sh

## ── Codec validation (M2 harness, vulkanav1enc spec §7) ────────────────────

codec-validate: ## Per-codec live-decode + strict-decode gate — ARGS='--app Steam --codecs h264,av1' HOST=devbox
	@bash $(DX)/codec_validate.sh

nvenc-fallback-smoke: ## Vulkan->NVENC fallback smoke (effective encoder + live decode + clean teardown, #489-adjacent) — needs QUASAR_VULKAN_H264=0 already set on the agent (see script header) — ARGS='--app Steam' HOST=devbox
	@bash $(DX)/nvenc_fallback_smoke.sh

## ── Managed-home housekeeping ───────────────────────────────────────────────

homes-gc: ## Sweep throwaway (agent-*) managed homes on a host NOW — HOST=devbox ARGS='--dry-run'
	@bash $(DX)/homes_gc.sh

## ── Benchmarks (quasar-bench results service) ───────────────────────────────
# BENCH_URL + BENCH_KEY come from the environment — never commit the key.

report-publish: ## Publish a completion report to quasar-bench — REPORT=<file> TITLE=<text> [COMMIT=HEAD ISSUES="" RUNS="" TAGS="" PIN=1] HOST=devbox
	@bash $(DX)/report.sh publish

report-attach: ## Attach evidence (screenshot/video/log/bundle) to a report — COMMIT=<sha> FILE=<path> [ROLE= CAPTION=] HOST=devbox
	@bash $(DX)/report.sh attach

report-url: ## Print the stable report URL for a commit — COMMIT=<sha> HOST=devbox
	@bash $(DX)/report.sh url

bench-submit: ## Submit an existing soak/observe run dir — DIR=<dir> ARGS='--suite ... --scenario ...'
	@bash $(DX)/common.sh require-local bench-submit
	@bash $(DX)/pyrun.sh bench-submit bench_submit.py --dir DIR

bench-retro: ## Replay the archived pre-service run manifest into quasar-bench (idempotent)
	@bash $(DX)/bench_retro.sh

bench-run: ## ONE live benchmark iteration + submit — HOST=devbox ARGS="--profile 1080p60-h264 --secs 240"
	@bash $(DX)/bench_run.sh

bench-suite: ## Benchmark MATRIX (profiles x abr_mode x ladder x netem), resumable — HOST=devbox ARGS='--profiles ...'
	@bash $(DX)/bench_suite.sh

bench-budget: ## Print + gate the reconciled g2g budget table for a run — RUN=<id>|latest ARGS='--suite ... --baseline ...'
	@bash $(DX)/pyrun.sh bench-budget bench_budget.py --run RUN

bench-baseline: ## Pin a run as quasar-bench's named baseline — RUN=<id> NAME=<suite/scenario>
	@bash $(DX)/pyrun.sh bench-baseline bench_baseline.py --run RUN --name NAME

nightly-budget-install: ## Idempotently install the 03:30 nightly budget crontab line — HOST=devbox
	@bash $(DX)/nightly_budget_ctl.sh install

nightly-budget-run: ## Trigger one nightly-budget run now, foreground — HOST=devbox
	@bash $(DX)/nightly_budget_ctl.sh run

nightly-budget-status: ## Tail today's nightly-budget log + LAST_REGRESSION if any — HOST=devbox
	@bash $(DX)/nightly_budget_ctl.sh status

## ── Validation harness ──────────────────────────────────────────────────────

validate: ## End-to-end validation report — LEVEL=ui|api|session|all TARGET=local|<url> KEEP=1 ARGS='--reuse-web'
	@bash $(DX)/validate.sh

## ── Image QA ────────────────────────────────────────────────────────────────

qa: ## QA a candidate image on a live stack — IMAGE=<tag> PROFILE=<name> HOST=<role> [APP= RUNS= SKIP_INPUT= PEER= KEEP=1 ARGS='--no-repoint']
	@bash $(DX)/qa.sh

## ── Diagnostics ─────────────────────────────────────────────────────────────

diagnose: ## One page: git, instance, stack state, health, versions, error tails
	@bash $(DX)/diagnose.sh

diagnose-bundle: ## Archivable, redacted bundle under .diagnostics/
	@bash $(DX)/bundle.sh

admin-token: ## Print an admin bearer for a stack (THE one ladder) — HOST=<role|name> [ARGS='--fresh --ttl 2h']
	@bash $(DX)/admin_token.sh

session-list: ## Every session on a stack, running first — HOST=<role|name> [JSON=1]
	@bash $(DX)/session.sh list

session-verdict: ## Classifier verdict + evidence for a session — SID=<id>|latest HOST= [WINDOW=from,to] [JSON=1]
	@bash $(DX)/session.sh verdict

session-metrics: ## Recent telemetry samples as a table — SID= HOST= [SINCE=10m] [N=20] [JSON=1]
	@bash $(DX)/session.sh metrics

session-trace: ## Trace window + events for a session — SID= HOST= [WINDOW=from,to] [JSON=1]
	@bash $(DX)/session.sh trace

session-bundle: ## Raw diagnostic-bundle JSON to a file — SID= HOST= [OUT=<path>] [WINDOW=from,to]
	@bash $(DX)/session.sh bundle

session-capture: ## ONE bounded observation of a live session (graph / encoder props / burst stats) — SID=<id>|latest HOST= KIND=pipeline_dot|encoder_props|burst_stats|all [OUT=<dir>] [WINDOWS=20] [WINDOW_MS=250]
	@bash $(DX)/session.sh capture

session-logs: ## docker logs for a session's containers, over ssh — SID= HOST= [SINCE=10m] [GREP=pat]
	@bash $(DX)/session.sh logs

session-diagnose: ## Full smoothness analysis (network/encoder/client) — SID= HOST= [WINDOW=from,to] [JSON=1]
	@bash $(DX)/session.sh diagnose

## ── UI visual audit ─────────────────────────────────────────────────────────

ui-audit: ## Capture ALL routes + coverage report — URL=<base> KEY=<dev-agent-key>
	@bash $(DX)/common.sh require-local ui-audit
	@bash $(DX)/uiaudit.sh make-audit

ui-audit-routes: ## Capture specific routes + coverage report — URL=<base> ROUTES=id,id,...
	@$(MAKE) ui-audit

ui-audit-ab: ## A/B report from two evidence dirs — BEFORE=<dir> AFTER=<dir> [OUT=<report.html>]
	@bash $(DX)/common.sh require-local ui-audit-ab
	@bash $(DX)/uiaudit.sh make-ab

## ── Housekeeping ────────────────────────────────────────────────────────────

clean: ## Remove local build artifacts (web/dist, .build-tmp, build reports)
	@bash $(DX)/common.sh require-local clean
	@rm -rf $(WEB)/dist .build-tmp deploy/.build-report.json
	@printf 'RESULT status=ok target=clean\n'

reset: ## Tear down THIS worktree's stack — needs CONFIRM=reset (or reset-data)
	@bash $(DX)/reset.sh
