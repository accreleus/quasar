# PyroWave / Granite patches

Applied by `deploy/Dockerfile.vulkan` to the Granite checkout that PyroWave's
`checkout_granite.sh` makes, after the `GRANITE_REF` check and before the CMake build.
Every file here is a toolchain input (`deploy/toolchain-hash.sh`).

| Patch | Why |
|---|---|
| `granite-borrowed-device-wait-own-work.patch` | #1663: on a borrowed `VkDevice`, wait for Granite's own submissions instead of `vkDeviceWaitIdle`. The header in the file has the detail. |

Re-apply (re-diff) every patch here on each `PYROWAVE_REF`/`GRANITE_REF` bump.
`scripts/dev/pyrowave-borrowed-wait/check.sh` builds the pinned library with the patch and
checks it on lavapipe; `--unpatched` shows the failure it fixes.
