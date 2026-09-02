---
name: quasar-image
description: Use to audit, validate, build, re-pin, or deploy Quasar container images on any fleet host — "what's running on the fleet", "is the image still valid", "rebuild the agent image", "bump the gwd/gst pin and see what changed", "why is the image so big", "did the contract pass", "deploy the new image". Wraps deploy/build-images.sh + validate-image.sh + image-contract.json. Pairs with quasar-host (operating a deployment and shell access on any host).
---

# Quasar image fleet (qimg)

    .claude/skills/quasar-image/scripts/qimg <verb> [args]

| verb | does |
|---|---|
| `status [--host H\|--all] [--json] [--fast]` | read-only fleet audit + HTML report. Validates each role's contract by default; `--fast` skips contract validation and the live `running` checks (disk/image facts still collected). |
| `validate --host H --role R [--gpu\|--no-gpu]` | run `deploy/validate-image.sh` on that host. GPU-gated assertions auto-enable from `hosts.<H>.gpu` unless you force them. |
| `build --host H [roles...] [pin flags...]` | build via `deploy/build-images.sh` on the host. Pin/build-arg flags (`--gwd-ref`, `--gst-version`, `--build-arg K=V`, ...) pass straight through; an undeclared one is rejected by the script's own ARG guard. Contract failure blocks `:latest` promotion — that's `build-images.sh`'s own behaviour, not overridden here. |
| `repin --host H [--role R] [--deep] [pin flags...]` | snapshot `:latest` → build a candidate to a **dated tag only** (`--no-latest --tag-suffix`) → snapshot the candidate → diff (size delta, contract assertions newly failing/passing, element-presence deltas, pin-label deltas). **Never promotes `:latest`, in any case, including a clean pass** — this is load-bearing (see Gotchas). Going live is a separate, deliberate `qimg deploy`. |
| `deploy --host H` | run the host's configured deploy command from `config.json`. A host with no entry refuses rather than guessing. |
| `report [--host H\|--all] [-o FILE] [--fast]` | render the fleet report as a single self-contained HTML file (default `deploy/results/qimg-report-<ts>.html`, gitignored). |

Exit: `0` clean · `1` contract violation or drift · `2` usage/infra error.

**Hosts come from `_shared/hosts.json`.** `--host` takes a **role** (`gpu-test`,
`aux-infra`, `deploy-only`) or a literal host name; `--all` sweeps every configured
host. Adding a box is one JSON entry — never hardcode a host in a script.

**This skill never re-implements build or assertion logic.** `deploy/build-images.sh`
and `deploy/validate-image.sh` are the single source of truth and run ON the host.
`deploy/image-contract.json` is the executable form of every image defect that has
reached production — **never relax a contract assertion to make a build green; fix
the image.**

## Gotchas
- A host's `runtime_image` (from `hosts.json`) is a **runtime** image with **no Rust
  toolchain** — use the dev image (`quasar-agent-dev:latest`) for `cargo`.
- The agent binary is **baked**; an in-container `cargo build` or a compose `command:`
  override is a regression.
- GPU-gated assertions need the right host — `validate` auto-enables them from the
  host's `gpu` field (`hosts.json`), not from what you happen to be running on.
- `repin` leaves a **contract failure's dated tag** in place for inspection and
  refuses to promote `:latest` — that is the point, don't work around it. Silently
  auto-promoting on a "the diff looked fine" judgment call is exactly the failure
  mode this verb was built to catch (Task 6 review: an earlier draft let
  `build-images.sh`'s auto-promotion fire mid-verb, moving the production pointer
  before the operator ever saw the diff).
- **Never relax a contract assertion to make a build green; fix the image.** The
  contract exists because a real defect (a 7.33 GB agent image, missing its
  PulseAudio daemon) shipped silently once already.

Self-test: `scripts/validate [--live HOST]`
Design: `docs/superpowers/specs/2026-07-27-quasar-image-skill-design.md`
Spec (underlying build/contract tooling): `docs/design/plans/2026-07-26-image-lineage-consolidation-spec.md`
