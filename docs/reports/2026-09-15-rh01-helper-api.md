# Owned diagnostic lifecycle (#233)

Implementation on `feature/rh-01-runtime-api`, based on #232 (`c18b535e54361a4372d4cd2d4464cfd410185ff7`). Approved contracts: [#228](https://github.com/accreleus/quasar/issues/228), [#229](https://github.com/accreleus/quasar/issues/229), and [research map #224](https://github.com/accreleus/quasar/issues/224). Integration remains restricted to `initiative/resilient-host-architecture`.

## Delivered behavior

The NVIDIA explicit host-path diagnostic uses the Quasar runtime API for its complete lifecycle. It writes a fresh marker through the agent-visible driver mount, asks an owned helper to read it through a read-only daemon-host bind, and accepts only a known zero exit with matching content. Incorrect or absent host directories fail validation; Docker does not create them. Image selection and existing path-validation/cache behavior remain.

The private Bollard adapter realizes a fixed diagnostic profile: no network or devices, read-only root and bind, no added capabilities, no-new-privileges, and no automatic removal. Quasar-owned requests and opaque container handles keep SDK types private. Every adoption and mutation verifies the original owner, operation, requested configuration and immutable container identity.

A synced per-operation journal retains uncertain creates, starts, explicit stops and removals. Retry reconciles the original operation rather than creating another helper. Final stdout/stderr and exit evidence are retained before explicit removal; an unknown exit is not success. Completed records replay the original result. Observation cancellation and control-plane disconnection never imply termination. Explicit stop intent survives restart. Startup recovery and later host-path checks retry cleanup without deleting volumes or user data.

Each log stream retains up to 4 KiB of raw bytes while draining to EOF, preserving fragmented UTF-8 across frames. Requests reserve journal capacity for escaped final evidence before Docker mutation.

## Validation

| Check | Result |
|---|---|
| `make verify` | 428 passed, 0 warnings, 0 failures |
| `make test-rust` | Formatting and Clippy passed; 1,367 library tests passed, 4 opt-in tests ignored; remaining test targets and benchmark smoke tests passed |
| Runtime lifecycle HTTP fixture | 23 passed, including recorded response-loss, ownership, cancellation and cleanup regressions |
| Local Docker lifecycle acceptance | Passed: read-only bind, final logs/nonzero exit, cancellation, explicit stop, restart recovery and data preservation |
| Actual host-path nonce caller on local Docker | Passed: same-directory marker, wrong-directory rejection, absent bind rejection, marker cleanup |
| `scripts/dev/leak-scan.sh --staged` | Clean |

Docker acceptance used Docker 29.8.0 with negotiated API 1.53. The standard devtools suite skips its documented live GPU/media and interpipe-backed checks. No changed behavior depends on those components.

## Review and test process

Primary standards/correctness review and independent specification review exercised ownership, uncertain outcomes, cancellation and recovery. Recorded failing public-interface regressions drove corrections for lost-create adoption, detached completion replay, definite create rejection, stop/remove response loss, stale-observer journal races, journal capacity and durable explicit-stop recovery. Initial worker draft code preceded tests; this report does not claim that entire draft was test-first.

**Standards/correctness:** no outstanding actionable findings after the corrections.

**Specification:** independent review cleared the final snapshot. Review additionally caught lost reconciliation error detail and incomplete bind-option verification; failing regressions drove both corrections, including cleanup-specific observations and explicit false defaults.

## Reproducing local Docker acceptance

From the repository root, using the explicitly selected local Docker daemon and existing development image:

```sh
docker run --rm --network host \
  -v "$PWD":/workspace \
  -v /var/run/docker.sock:/tmp/quasar-engine.sock \
  -e QUASAR_TEST_RUNTIME_SOCKET=/tmp/quasar-engine.sock \
  -e QUASAR_TEST_RUNTIME_IMAGE=quasar-agent-dev:latest \
  -e QUASAR_TEST_RUNTIME_HOST_ROOT="$PWD" \
  -w /workspace/node-agent quasar-agent-dev:latest \
  cargo test --test runtime_helpers_docker -- --ignored --nocapture
```

With the same container options, run `cargo test --lib real_docker_host_path_nonce_validation -- --ignored --nocapture` for the migrated caller. Fixtures use unique disposable containers and scratch directories, preserve shared data, and retain recovery state if cleanup fails. No host-wide fault injection or pruning is needed.

## Limits

Validation uses the existing local agent development image and uniquely owned Docker fixtures. No platform Dockerfile or media pipeline changes, platform image contract, fleet deployment, GitHub Actions image build or GPU streaming acceptance are claimed. Application/audio lifecycle migration, updater redesign, Podman/rootless certification and running-session adoption remain outside this ticket. No promotion to develop or main.
