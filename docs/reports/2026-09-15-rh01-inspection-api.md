# Installation and storage inspection through the runtime API (#238)

Implementation on `feature/rh-01-runtime-api`, based on completed #233 (`a3938e2`). Approved contracts: [#228](https://github.com/accreleus/quasar/issues/228), [#229](https://github.com/accreleus/quasar/issues/229), and [research map #224](https://github.com/accreleus/quasar/issues/224). Integration remains restricted to `initiative/resilient-host-architecture`.

## Delivered behavior

Installation discovery, image environment, agent mount and network inspection, engine storage information and home-cleanup liveness use the shared Quasar runtime API. Bollard models remain inside the private adapter. Configured image references stay separate from immutable image IDs; missing Compose labels remain unknown. Missing objects and failed inspection remain distinguishable. Unmigrated application and audio lifecycle callers retain their CLI paths; there is no automatic CLI fallback.

Daemon-host paths have a distinct Quasar type. A containerized agent translates filesystem paths using its inspected bind mounts, accounting for nested mounts and ambiguity. Disk checks require a valid reverse mapping before checking free space; unknown space retains the existing logged fail-open policy. Native agents retain the explicit same-host namespace assumption.

Both tracked-home GC and the throwaway-home sweep use a shared, fail-closed liveness snapshot. Foreign containers protect overlapping parent, child and exact host paths. The agent's own mounts provide namespace mapping without permanently protecting every managed home. Unknown runtime state, incomplete facts, unknown mappings and poisoned in-process live-reference state prevent deletion. Interrupted trash remains protected and dry-run preserves it. Deferred tracked homes are not confirmed to the control plane.

## Validation

| Check | Result |
|---|---|
| `make verify` | 428 passed, 0 warnings, 0 failures |
| `make test-rust` | Formatting and Clippy passed; 1,385 library tests passed, 4 opt-in tests ignored; remaining targets and benchmark smoke tests passed |
| Runtime inspection and namespace-mapping regressions | 15 passed |
| Shared storage-liveness / throwaway GC / tracked GC focused checks | 4 / 14 / 11 passed |
| Local Docker acceptance | Passed on Docker 29.8.0, negotiated API 1.53 |
| `scripts/dev/leak-scan.sh --staged` | Clean |

The first full Rust run caught duplicate logging tokens. Distinct failure tokens and centralized root-refusal logging corrected them; the complete rerun passed. The standard devtools suite skips documented GPU/media and interpipe-backed checks; no changed behavior depends on those components.

The local Docker acceptance exercises the actual installation/image callers and both reapers. It creates a uniquely labelled foreign container with no Compose labels, proves that its live parent bind protects two disposable homes, stops it, and removes only a fixture-owned socket alias to simulate engine unavailability. Both reapers preserve data while inspection is unavailable. A poisoned local-reference lock also prevents deletion. After restoring known liveness, both reapers delete only their disposable candidates; the tracked reaper confirms only the removed home. Cleanup verifies the fixture label and immutable container identity.

## Review and test process

Primary standards/correctness review and independent GPT-5.6 Sol specification review cleared the final implementation, cleanup-test changes and logging correction. Corrections addressed foreign volume sources, actual local-reference comparison, unknown mapping reporting, dry-run trash protection and unknown NVIDIA mount kinds.

Recorded behavioral RED→GREEN checks cover the tracked reaper deleting a foreign-mounted home and the NVIDIA locator accepting unknown mount semantics. Initial API and mapping work included implementation before some regression tests; this report does not claim that the entire implementation was test-first. The first full Docker run reached all policy assertions but failed its final cleanup assertion because teardown had removed its socket alias; the corrected assertion uses the stable fixture endpoint and passes.

## Reproducing local Docker acceptance

From the repository root with the existing development image and explicitly selected local Docker endpoint:

```sh
docker run --rm --network host \
  -v "$PWD":/workspace \
  -v /var/run/docker.sock:/tmp/quasar-engine.sock \
  -e QUASAR_TEST_RUNTIME_SOCKET=/tmp/quasar-engine.sock \
  -e QUASAR_TEST_RUNTIME_IMAGE=quasar-agent-dev:latest \
  -e QUASAR_TEST_RUNTIME_HOST_ROOT="$PWD" \
  -w /workspace/node-agent quasar-agent-dev:latest \
  cargo test --test runtime_inspection_docker -- --ignored --nocapture
```

## Limits

Liveness is a point-in-time observation, not a lock against another manager starting a container concurrently. Unmapped container filesystems defer cleanup. Docker is the validated target; full Podman/rootless certification remains separate.

No platform Dockerfile or media pipeline changed. No platform image contract, GPU streaming test, new image build, fleet deployment or GitHub Actions image build is claimed. Frozen wire contracts and the updater are unchanged. No promotion to develop or main.
