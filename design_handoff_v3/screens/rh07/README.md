# RH-07 surfaces — mockups (#394)

**Status: awaiting the owner's approval (#394).** No RH-07 UI ticket builds against these
until the owner approves them on #394 (D21).

- Mockup: [`../rh07-v3.html`](../rh07-v3.html). A standalone reference page built the same
  way as `fleet-rh06-v3.html`: each specimen is one surface in one state.
- Section renderer: [`../assets/pages-rh07.js`](../assets/pages-rh07.js). It reuses the
  approved RH-06 helpers from `pages-rh06.js` (`snippet`, `diag`, `stage`, `modal`,
  `railFact`, `rhDetailHead`), which the page loads first, and the `ui.js` helpers.
- Screenshots: this directory, 23 specimens plus three narrow captures, about 2.5 MB.
  Each PNG is one specimen cropped to its element, caption included. Desktop PNGs come from
  a 1440 px viewport (1080 px wide); `narrow-*` from a 900 px viewport (820 px wide).
  Rendered with headless Chromium (Playwright's `chromium_headless_shell-1243`) at device
  scale 1.
- Product authority: `docs/rh07/2026-09-28-decisions.md` (P1, D1–D23), specification #390,
  contract amendment 17 (`protocol/agent-api.md`: host facts `engine`, `engine_version`,
  `engine_mode`; readiness `skip` for an optional diagnostic the host did not grant; the
  "RH07 checks" ids), and `deploy/prepare-host.sh` for the host-preparation flags and printed
  lines.

Machine names, versions, digests and paths are placeholders.

## Two styles on one page

The quick start is not in the admin console. It is on the documentation site, which is
Starlight with its own theme (`site/src/styles/theme.css`, the `--q-*` tokens) and the
wizard's own `qs-*` classes (`site/src/components/QuickStart.astro`). **The quick-start
specimens (`qs-*`) are drawn in the site's style, not console-v3.** Their CSS in
`rh07-v3.html` is copied from those two files and scoped to a `.site` frame. The admin
console specimens (`console-*`, `rd-*`) use console-v3 like every other v3 mock.

## The sample machines

They continue the RH-06 fleet story.

| Machine | Story here |
|---|---|
| living-room-pc | Combined host, Fedora, Podman rootless, NVIDIA; a TV on HDMI for console mode; the quick-start example |
| gpu-host-4 | Docker rootful (readiness `rd-rootful`) |
| gpu-host-2 | Ubuntu 24.04, Docker rootless: an experimental profile (`rd-experimental`) |
| gpu-host-6 | Just added, Podman rootless, host preparation incomplete (`rd-fail`) |
| gpu-host-3 | Older node agent that reports no engine facts (`rd-unknown`) |

## Where each surface lives

| Surface | Placement |
|---|---|
| Engine choice and profile badges | The site quick start: a new step 2, **Engine**, after Host. The wizard grows from six steps to seven. |
| Host preparation | The quick start's Result step, as its step 1, above the install. Its three optional toggles are checkboxes there; "What it changes" is a disclosure holding the printed lines. |
| Quadlet unit / seed `docker run` | The Result step's step 2. Podman gets a Quadlet unit for the quasar account (D15), with the `podman run` seed behind a closed "only trying it out?" disclosure. Docker keeps today's script (without the host preparation it used to do) and today's Dockge or Arcane stack. |
| Console mode on an owned host | Fleet ▸ host ▸ **Local console**, the existing page from `admin-console-v3.html`. The panel head carries the state chip and the switch; turning it on or off asks first; progress and failure appear as a note above the panel. |
| Engine facts and "skipped, and why" | The host page's readiness card (the product's `ReadinessCard`, grid layout). Engine facts head the card; skipped checks sit in the card's existing disclosure, relabelled "N checks skipped, and why". |

## Surface → state → screenshot

| # | Surface | State | Screenshot |
|---|---|---|---|
| a | Quick start: Host (context) | normal: amended platform list | [`qs-host.png`](qs-host.png) |
| a | Quick start: Engine | empty: nothing chosen, Next waits | [`qs-engine-none.png`](qs-engine-none.png) |
| a | Quick start: Engine | normal: supported (Fedora, Podman rootless) | [`qs-engine-supported.png`](qs-engine-supported.png), [`narrow-qs-engine-supported.png`](narrow-qs-engine-supported.png) |
| a | Quick start: Engine | normal: supported, rootful (says the socket is equivalent to root, D17) | [`qs-engine-rootful.png`](qs-engine-rootful.png) |
| a | Quick start: Engine | variant: experimental (Ubuntu 24.04, Docker rootless) | [`qs-engine-experimental.png`](qs-engine-experimental.png) |
| a | Quick start: Engine | error: unsupported (Unraid, Podman rootless), reason and alternative named, Next waits | [`qs-engine-unsupported.png`](qs-engine-unsupported.png) |
| b, c | Quick start: Result | normal: Podman rootless, host preparation + Quadlet unit | [`qs-result-podman.png`](qs-result-podman.png) |
| b | Quick start: Result | normal: "What it changes" open, first run | [`qs-prep-output.png`](qs-prep-output.png) |
| b | Quick start: Result | normal: host preparation run again, nothing to change | [`qs-prep-rerun.png`](qs-prep-rerun.png) |
| b, c | Quick start: Result | normal: Docker rootful, host preparation + seed by `docker run` | [`qs-result-docker.png`](qs-result-docker.png) |
| b, c | Quick start: Result | variant: Docker rootless, port 443 toggle on, seed as the quasar account | [`qs-result-docker-rootless.png`](qs-result-docker-rootless.png) |
| d | Console mode | normal: off | [`console-off.png`](console-off.png) |
| d | Console mode | normal: confirm turning on (ends the host's sessions) | [`console-confirm.png`](console-confirm.png) |
| d | Console mode | normal: applying (agent being replaced through the recovery actor; settings locked) | [`console-applying.png`](console-applying.png) |
| d | Console mode | normal: on, local audio to the host's PipeWire | [`console-on.png`](console-on.png), [`narrow-console-on.png`](narrow-console-on.png) |
| d | Console mode | variant: on, no PipeWire, local audio to ALSA | [`console-on-alsa.png`](console-on-alsa.png) |
| d | Console mode | error: failed and restored (display held by another process) | [`console-failed.png`](console-failed.png) |
| d | Console mode | error: failed and restored (host not prepared with `--console`) | [`console-unprepared.png`](console-unprepared.png) |
| e | Readiness card | normal: Podman rootless, optional diagnostics skipped | [`rd-rootless.png`](rd-rootless.png), [`narrow-rd-rootless.png`](narrow-rd-rootless.png) |
| e | Readiness card | normal: Docker rootful (checks that do not apply are skipped, with why) | [`rd-rootful.png`](rd-rootful.png) |
| e | Readiness card | variant: experimental engine profile (`runtime_engine` warns) | [`rd-experimental.png`](rd-experimental.png) |
| e | Readiness card | error: host preparation missing (a failure with a fix, beside skips) | [`rd-fail.png`](rd-fail.png) |
| e | Readiness card | unknown: engine not reported (older agent) | [`rd-unknown.png`](rd-unknown.png) |

## Surface → implementing ticket

| Surface | Ticket | Why |
|---|---|---|
| Console mode settings, states off / confirm / applying / on / failed and restored, on rootful owned installs | #395 RH07-03 | "An admin can enable and disable console mode per owned host … UI matches the approved RH07-02 mockup"; "the setting does not claim success until the new agent verifies". |
| Console mode on rootless engines; local audio to PipeWire or ALSA; the "not prepared" failure | #407 RH07-15 | "Local audio uses the host's PipeWire when present … ALSA otherwise"; "a display another process holds is a named failure". |
| Quick start: engine step, profile badges, host-preparation block, Quadlet unit, Docker script changes; the engine-profile docs page the badges link to | #406 RH07-14 | "The quick start detects or asks for the engine and mode and emits the host-preparation command plus a seed for Docker or a Quadlet unit for Podman". |
| Readiness card: engine facts, `runtime_endpoint` / `runtime_engine` wording | #396 RH07-04 | Reports engine, version and mode as host facts; "readiness remediation stops naming only Docker". |

Supporting, with no surface of their own: #393 (the contract these read), #400 (the
`prepare-host.sh` flags and printed lines the quick start shows), #402 (`xid_visibility`
and the other checks that become "skipped, and why"), #405 (`engine_restart_on_boot`,
`engine_healthchecks`, the Quadlet socket path under D22), #401 (`input_device_access`),
#399 (`runtime_cdi`).

## Copy

- Terms follow `CONTEXT.md` "Engines and privilege": container engine, engine mode
  (rootful, rootless), engine profile, Quasar user (shown to operators as "the quasar
  account"), host preparation, least privilege.
- A rootful engine says plainly that its socket is equivalent to root (D17), in the
  quick start and in `runtime_engine`'s summary.
- A skip is never drawn as a fault: dashed glyph, no chip, no remediation, no
  needs-attention count (amendment 17). Its summary names the host setting that would grant
  it. A failure has a copyable fix and, where it rests on evidence, a "Blocks launches" chip.
- Console mode never reads as on until the new node agent is healthy, and says that a
  replacement ends the host's live sessions.
- As in RH-06, wire identifiers (attempt ids, readiness check ids in a failure) appear only
  under a closed **Details** disclosure. Readiness rows keep the product's current titles,
  which are the check ids with spaces (see "Observations").

## Where DESIGN.md or the v3 patterns did not cover something

Said explicitly rather than invented:

1. **The site has no badge.** The quick start has never shown one. The mock uses
   Starlight's own `<Badge>` component (the site framework's built-in, small size),
   unmodified. Its colours are Starlight's default green, orange and red hues:
   `theme.css` re-points Starlight's accent and greys to Quasar's tokens but not its state
   hues. **Decide** whether `theme.css` should map them to the product's `--success`,
   `--warning`, `--danger` families. That is a one-place change in `theme.css`.
2. **A badge beside a choice.** `.qs-choices` rows had no trailing element. The badge sits at
   the row's end with `margin-left:auto`; nothing else about the row changes.
3. **The site has no danger note.** `.qs-note.qs-warn` (the orange edge) is used for both the
   experimental and the unsupported notes. The badge tells them apart.
4. **Long code on the site scrolls.** `.qs pre` caps at 26rem and scrolls sideways. The
   printed output, the Quadlet unit and the Docker scripts drop the cap here
   (`max-height:none`, as the site's own `.qs-tree pre` does), and the printed output wraps,
   so the screenshots show all of it. Whether the real page wraps or scrolls is the
   implementer's call within the site's existing rules.
5. **There is no readiness mock in the handoff.** The card is the product's `ReadinessCard`
   (grid layout), its CSS copied unchanged from `web/src/styles`. Engine facts inside it have
   no precedent; they use RH-06's `.rel-fact` rows under an eyebrow.
6. **A setting that is still being applied has no v3 precedent.** "Applying" follows RH-06's
   remove-in-progress: a `.note`, an info chip, and the controls disabled.

## Data the mock assumes — needs a source in the implementing ticket

| Value shown | Where | Owner |
|---|---|---|
| Console mode's per-host state (off, applying, on, failed and restored) and the failed attempt's reason | console specimens | #395 (console-config / attempt state; amendment 17 fixes only the readiness ids) |
| Whether the host runs PipeWire, and its outputs; ALSA outputs otherwise | console "Local audio output", Reported capabilities | #407 (console capabilities) |
| Monitor control availability ("DDC on i2c-4") | Reported capabilities | #395 / #407 |
| The engine profile's name and status in `runtime_engine`'s summary (the agent must know the OS) | readiness `rd-*` | #396 (agent-owned summary text) |
| The engine-profile data the quick start's badges read | quick start | #406 (the docs page is the source; the badge table should be generated from it, not hand-kept in two places) |

## Open questions for the owner

1. **Does "unsupported" block the quick start?** The mock blocks: Next waits and nothing is
   generated, which matches the enrollment command refusing an unsupported profile by name.
   Under D5 that also means **Debian, Arch and "another systemd Linux" become unsupported**,
   although today's quick start offers them. Confirm, or make unsupported a warning that
   still generates.
2. **The platform list.** The mock splits "Debian or Ubuntu" into "Ubuntu 24.04" and "Debian",
   because only Ubuntu 24.04 has a profile, and names Fedora's image-based editions (Silverblue,
   Bazzite, uCore) under Fedora. Confirm.
3. **Badges before evidence.** The mock draws the target state: Fedora's three required
   profiles read Supported. Until #409's acceptance map exists they are unproven. Should the site
   show them as Experimental until then, driven by the engine-profile page?
4. **Where `prepare-host.sh` is fetched from.** The mock uses the documentation site
   (`curl -fsSLO https://accreleus.github.io/quasar/prepare-host.sh`). Not decided anywhere:
   the alternatives are a release asset or a pinned raw file, and whether the quick start
   prints a checksum to verify it.
5. **The Owner step on a rootless engine.** Home files belong to the quasar account (D14), so
   the mock's summary reads "Owned by: the quasar account" and the uid/gid choice would say
   "Nothing to choose" there. Confirm.
6. **The Quadlet unit's details.** The socket mount (`%t/podman/podman.sock` to
   `/run/podman/podman.sock`) is illustrative until the ADR 0007 amendment (D22, #405) fixes the
   engine socket path. A rootful Podman unit would live in `/etc/containers/systemd/` instead;
   not drawn.
7. **The skipped disclosure.** Amendment 17 widens `skip` to "not granted", so the mock relabels
   the card's "N checks not applicable to this host" to "N checks skipped, and why". The
   specimens show it open for review. **Decide** whether it stays closed by default, as today.
8. **Whose PipeWire.** On a rootless install, the desktop's PipeWire belongs to the person logged
   in, not to the quasar account, and host preparation grants no access to it. The UI only says
   "host PipeWire"; #407 has to settle how the agent reaches it.
9. **Turning console mode off** ends the host's sessions the same way. The mock draws only the
   "turn on" confirmation; "turn off" would reuse it with its own title and button. Confirm.
10. **Engine facts elsewhere.** The mock shows engine, version and mode on the readiness card and
    the Local console rail only. Should Fleet ▸ Hosts' expanded row show them too?
11. **Add host on a Podman machine.** RH-06's Add host dialog has a "Dockge or Arcane" tab, which
    is Docker only. Should a Podman GPU host get a Quadlet tab there (#406)? Not drawn.
12. **The kernel-log toggle's warning.** `kernel.dmesg_restrict=0` is machine-wide, so the copy
    says every account can then read the kernel log. Confirm the wording.

## Visual verification

Rendered in 1440 px and 900 px viewports and checked by eye against the sources:

- Quick-start specimens against `QuickStart.astro`'s markup and styles and `theme.css`'s dark
  tokens (the progress bar, choices, notes, summary, code blocks and nav are the same classes).
- Console mode against `admin-console-v3.html#/fleet/hosts/<id>/console` (`pageHostConsole`):
  `.cset` rows, eyebrow groups, the switch, selects, the rail cards.
- The readiness card against `web/src/components/ReadinessCard.tsx` and its CSS.
- Dialogs, notes, snippets and Details against `fleet-rh06-v3.html`.

Observations:
- **State chips render neutral**, as in every v3 mock (RH-06's README explains why:
  `console-v3.css`'s later `.chip` rule wins). The `chip-success` / `chip-danger` classes are
  kept and render grey here.
- **Readiness titles are check ids with spaces** ("xid visibility"), because that is what the
  product's card does today. Human titles would read better, but changing them is outside this
  ticket.
- At 900 px the console page keeps its two columns and the `.cset` rows stack their control
  under the text, as the existing mock does.
