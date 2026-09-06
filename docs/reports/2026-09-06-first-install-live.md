# First-install live validation: Unraid NVIDIA

## Scope and result

The operator authorized a fresh installation on the production Unraid deployment
host. The generated copied-file installation was exercised with the PR's locally
built candidate control-plane and agent images, plus an updater built on the
host through `deploy/build-images.sh`. This does not validate fetching those
candidates from a published release. No main/develop merge was performed.
Unrelated services, Docker daemon configuration, and existing unrelated data were
left alone.

The stack started after the securityfs and updater container-identity fixes in
`7cc8753`. Native NVIDIA graphics libraries were already present; automatic
NVRTC installation and the subsequent agent restart succeeded. This does not
exercise missing-graphics-driver provisioning or killed-provisioner recovery.

Browser media transport worked over the direct LAN address. The operator's
custom-domain attempt failed before WebRTC negotiation. Full Steam video,
audible sound, and interactive input acceptance remains outstanding.

## Domain failure: a rejected signaling origin

The operator used Chrome through their existing Caddy reverse proxy. The page
and authenticated launch API worked, but the launch screen showed:

> The host is ready, but the video path is not

The same browser automation reproduced this through the domain: session creation
returned 201, the signaling URL correctly used the public domain, and its
WebSocket upgrade repeatedly returned 403. The browser decoded zero frames.
The agent emitted offers but received no answers. This is not evidence of blocked
media UDP or missing STUN/TURN.

`GET /v1/admin/access-check`, requested through the proxy with the browser's
Origin header, established the cause:

- The proxy rewrites Host to the private HTTPS listener.
- Browser Origin is the public HTTPS domain, so the same-origin exemption fails.
- The resolved allowed-origin list was empty and sourced from the environment.
- `request_origin_allowed` was false, and the endpoint advised adding the domain.

The deployment was corrected by explicitly adding the operator's domain and LAN
HTTPS origin to `QUASAR_ALLOWED_ORIGINS` in its private `.env`, preserving all
other settings and credentials, then recreating only the control plane. The
access check subsequently reported `request_origin_allowed: true`. Repeating the
unchanged domain browser test then established both peer connections and decoded
2,475 video frames at 2560×1440, approximately 120 fps. The test stopped its own
session (202 response). Steam initialization remained pending.

## Gaps this exposes

The generated Compose passes `QUASAR_ALLOWED_ORIGINS: ${QUASAR_ALLOWED_ORIGINS:-}`.
That sets the variable even when the operator supplied nothing. Its intentional
SET-versus-UNSET semantics mean the empty environment value overrides the
admin-editable database list. The API contract explicitly documents that rule;
changing the meaning of an explicitly empty value is not the appropriate fix.
The generator should preserve the distinction between an absent override and an
explicitly configured policy.

The existing access-check endpoint already detects this failure precisely. The
first-stream setup path should surface its result for the current browser address
before asking the administrator to launch a stream. An explicit administrator
choice can permit that exact origin; untrusted request headers must not silently
expand the allow-list. Environment-pinned policy must remain visible.

The launch error should distinguish signaling failure from media connectivity.
Here a 403 prevented negotiation altogether, while the displayed remediation
pointed at LAN/VPN, UDP, STUN and TURN. Reuse the existing access-check diagnosis
for administrators and report signaling failure accurately to other users.

The session diagnostic classifier also reported `nominal` for a stopped operator
attempt with zero client samples, effectively zero encoder FPS and an encoder
stall event. That verdict does not establish a successful stream and needs a
separate evidence-sufficiency correction.

## Evidence and limits

Private, sanitized diagnostics are retained under
`.diagnostics/first-install-live/` (gitignored); credential and browser-state files
must not be published. Direct-LAN runs decoded 2,462 and 6,442 frames respectively
at 2560×1440, approximately 120 fps. The longer run received audio RTP packets,
but packet receipt does not prove audible content. Steam was still initializing,
and the loading overlay remained visible: these are media-transport checks,
not completed app acceptance. Each automated test stopped its own session.
