# Intel runtime-migration validation for external testers

Intel hardware is not available to the maintainers for this increment. AMD/NVIDIA acceptance does not certify Intel. This procedure prepares external testing without adding an Intel driver-volume provisioner or changing the host's kernel/firmware.

## Candidate and prerequisites

The maintainer supplies an exact runtime image tag/digest, image ID and source commit after local build and review. Do not substitute `latest` or compare different images between probes and the session. Record the existing deployment/image and preserve its data, agent identity and configuration before changing the agent. Apply the candidate only while the host has no active sessions.

Test only hardware physically available to you, with separate results for each available class:

- i3-12100 / UHD 730 (Alder Lake integrated graphics).
- Arc A-series / Alchemist, recording exact model and PCI ID.
- Arc B570/B580 / Battlemage, recording exact model and PCI ID.

The earlier i3-12100 success in [#126](https://github.com/accreleus/quasar/issues/126) does not certify every Intel generation or this new candidate. [#149](https://github.com/accreleus/quasar/issues/149) includes superseded packaging/generation claims; use its later correction and #126's closing confirmation when comparing results.

Host kernel driver and firmware must support the device. Newer userspace inside an image cannot repair missing host kernel support. Report a missing render node or firmware error; do not apply speculative `force_probe`, module options or host driver replacements as part of this test.

## Record the environment

Record OS/version, kernel (`uname -r`), GPU model/PCI ID, loaded kernel driver (`i915` or `xe`), selected `/dev/dri/renderD*`, and firmware status where exposed by the host. Avoid posting full system logs, machine identifiers, addresses or credentials.

Record runtime image ID/source label and the installed versions of `intel-media-driver`, `intel-gmmlib`, `libva`, and Mesa. The reviewed pre-migration image carries the full iHD driver; the absence of that package is a packaging regression, not a reason to mount arbitrary host library directories into the container. Record the application image separately: agent media drivers do not prove application graphics libraries work.

## Probe before launching

In the actual agent container, run the following with its selected render node. Replace `<agent>` and `<render-node>` with local values; those placeholders are not literal commands.

```sh
docker exec <agent> vainfo --display drm --device <render-node>
```

Record initialization success/failure and encode entrypoints, not just decode profiles. Then inspect the relevant GStreamer elements with a fresh registry so a GPU-less build cache cannot hide device-specific registration:

```sh
docker exec <agent> sh -c '
  registry=$(mktemp)
  rm -f "$registry"
  trap '\''rm -f "$registry"'\'' EXIT
  export GST_REGISTRY="$registry"
  gst-inspect-1.0 va
'
```

Successful `vainfo`, element registration or the prior #126 report does not certify this candidate. A successful bounded session establishes only the tested path on the exact device, kernel, driver and images recorded. Registration is only a preliminary check. Capture bounded actual encode and session results below before declaring a codec usable. Low-power and ordinary VA element names differ; record what the driver exposes rather than requiring one name across all Intel generations.

## Run the real session

1. Start with the existing Intel VA default. Save any explicit Vulkan encoder override and temporarily select VA for this baseline. Vulkan remains a supported opt-in; restore the original settings after testing. Use one normal H.264 720p60 application session with known visible content and sound.
2. Verify the host readiness and effective encoder. Record the actual selected GStreamer encoder from the session logs. A silent fallback must be reported as fallback, not success of the requested hardware path.
3. Confirm useful application rendering, keyboard/mouse response, audible application sound and continued video motion. A black picture with advancing frame counters is not rendering acceptance.
4. Where the maintainer's browser harness is available, run its `AUDIO_PROBE=1` in normal measurement mode and save the sanitized report. It requires advancing audio packets/decoded duration/energy and a live unmuted playing audio element. Do not post auth or signaling tokens.
5. Stop the session normally. Record elapsed time from acknowledged stop until the exact owned application/audio containers and session socket directories disappear; target is at most 35 seconds. Confirm persistent home/data and agent identity remain.
6. Repeat launch/stop once to catch stale sockets or name collisions. Leave no test sessions running.

Test HEVC/AV1 only when the hardware and selected driver expose a working encoder; unsupported codecs are an explicit result. Test Vulkan separately if investigating that path, recording the actual selected encoder and preserving `QUASAR_INTEL_VULKAN_VIDEO` behavior. A missing Vulkan encode queue is not evidence that VA cannot encode. Restore all temporary settings afterwards and wait for the agent to reconnect after settings that restart it.

## Maintainer-assisted helper checks

When supplied with the ticket's guarded helper-acceptance test, run it against uniquely owned disposable assets. It should record realized mount type/source/mode and device access inside the helper, reject a deliberately nonexistent fixture path/device, preserve foreign collisions, and reconcile interrupted cleanup without deleting backing data. Exercise interruption only through the supplied harness or maintainer-guided fixture; otherwise mark it unperformed. Do not simulate these failures by removing access from your real Docker daemon or replacing the installed GPU driver. If the harness is unavailable, mark these checks unperformed and return the normal-session evidence first.

A rollback to an older agent must follow confirmed cleanup of the candidate's API-owned helpers/audio. Older CLI cleanup code does not understand those journals. Preserve any pending cleanup state for diagnosis instead of starting an older sweep over it.

## Return a sanitized result

For each device/candidate return:

- Exact image/source and application image; OS/kernel; GPU/PCI ID; kernel driver; selected render node; firmware status (or "not exposed"); media/Mesa package versions.
- VA initialization and encoder entrypoints; actual chosen encoder and codec/profile.
- Application-image graphics and visible rendering, input and audible sound results; browser probe result if available.
- Launch/stop timings, exact cleanup outcome and any pending cleanup/readiness error.
- Whether identity/data were preserved and original settings restored.

Use the repository's `make diagnose-bundle` where available and review its redaction/manifest before sharing. Include only a short sanitized log window around failures. Omit `.env`, secrets, owner tokens, user data and full unredacted container inspections. Do not delete directories or use broad container/volume pruning to hide a failure; retain evidence and report it.
