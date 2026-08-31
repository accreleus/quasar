# Smithay upstream issue: no retroactive `wl_pointer.enter` on `GetPointer`

**Status: NOT YET FILED.** File against [Smithay/smithay](https://github.com/Smithay/smithay)
after (or alongside) games-on-whales/gst-wayland-display
[PR #44](https://github.com/games-on-whales/gst-wayland-display/pull/44), whose third
commit is the compositor-side mitigation. This doc is the complete filing material;
Michael signs off on the actual posting (standing upstream-comms rule).

## One-sentence summary

A `wl_pointer` created **after** its client's surface already holds smithay's pointer
focus never receives a `wl_pointer.enter` — and because focus is recorded compositor-side
regardless of delivery, every subsequent motion takes the same-target arm
(`wl_pointer.motion` only), so the client never learns it has pointer focus, permanently.

## Affected code (verified at the rev we build: `games-on-whales/smithay @ a166cf4c`, a fork of ~0.7; the same structure exists upstream)

`src/wayland/seat/mod.rs`, `GetPointer` dispatch — registration only, no catch-up event:

```rust
wl_seat::Request::GetPointer { id } => {
    ...
    if let Some(ref ptr_handle) = inner.pointer {
        ptr_handle.wl_pointer.new_pointer(pointer);   // just pushes into known_pointers
    }
}
```

`src/wayland/seat/pointer.rs` — `enter()` broadcasts only to resources existing at
emission time, and records state *before* the recipient loop (so it "succeeds" with zero
recipients):

```rust
fn enter<D: SeatHandler + 'static>(&self, surface: &WlSurface, event: &MotionEvent) {
    *self.last_enter.lock().unwrap() = Some(event.serial);      // recorded unconditionally
    self.for_each_focused_pointer(surface, |ptr| {              // zero iterations if the
        ...                                                     // client has no wl_pointer yet
        ptr.enter(event.serial.into(), surface, location.x, location.y);
    })
}
```

`src/input/pointer/mod.rs`, `PointerInnerHandle::motion` — once focus is recorded, the
same-target arm sends `motion` only; nothing ever re-emits the missed `enter`:

```rust
match (focus, old_focus) {
    (focus, Some((old, _))) if focus == old => focus.motion(...),   // motion only, forever
    (focus, Some((old, _)))                 => focus.replace(...),  // leave + enter
    (focus, None)                           => focus.enter(...),
}
```

## Why a compositor hits this

Any compositor that resolves pointer focus at surface **map time** (e.g. a synthetic
zero-delta motion after `map_element`, needed so a pointer constraint requested before
the surface became mappable can activate — nested gamescope `--force-grab-cursor`) races
the client's `wl_seat.get_pointer`. Dispatch order within one client connection decides
the winner, so:

- **Rootful Xwayland loses deterministically** — one surface for the whole X screen,
  mapped once, and Xwayland requests its pointer in its seat setup which can land after
  the mapping commit's dispatch. Result in the field: the compositor-drawn cursor moves,
  but Xwayland's X pointer never moves, so every X `ButtonPress` is delivered at one
  frozen stale coordinate — "mouse moves, nothing responds to clicks".
- **gamescope loses intermittently** and self-heals on its next surface re-map (the
  focus *change* takes the replace arm and emits a fresh enter) — presents as "input dead
  at launch, starts working a bit later".

## Field evidence (2026-08-04/05, production deployment)

- `WAYLAND_DEBUG=1` inside the affected client's container: `wl_pointer.motion` and
  `wl_pointer.button` delivered; **zero `wl_pointer.enter` in the entire trace** while
  `wl_keyboard.enter` arrives normally (keyboard focus is assigned directly, no race).
- `xinput test-xi2 --root` inside the rootful Xwayland: `Motion=0`, `RawMotion=0`,
  `ButtonPress` frozen at one root coordinate across the whole session.
- evdev capture upstream of the compositor proved motion+buttons arrived on its input
  devices — the loss is exactly at the missing `enter`.

## Suggested fix (smithay)

In the `GetPointer` dispatch, after `new_pointer(...)`: if the seat's current pointer
focus is a surface belonging to this client, send `enter` (and `frame` on version ≥ 5)
directly to the just-created resource, with the current location translated to
surface-local coordinates and a fresh serial (update `last_enter`). This is standard
catch-up behavior (keyboards get the analogous treatment via `enter` on focus set; other
compositors deliver an immediate enter to a pointer created under an already-focused
surface), and it removes the race at its source for every downstream compositor rather
than each one carrying a map-time-focus workaround.

Wire-protocol note from the mitigation work: any re-emitted/late `enter` must carry a
serial **newer** than any preceding `leave` — clients echo enter serials (`set_cursor`,
constraint activation) and assume recency.

## Relationship to our shipped mitigation (context for the issue, and for us)

gst-wayland-display PR #44 (amended 2026-08-05) carries a compositor-side guard: an
edge-triggered flag armed at map (after the synthetic motion) that, on the next real
motion — guarded on surface-present / pointer-not-grabbed / no-active-constraint —
forces focus to `None` so the following motion re-emits a genuine `enter`. Three fixture
tests. That guard is correct regardless of the smithay fix, but it is a mitigation:
the root cause is the missing catch-up in `GetPointer`. If smithay lands the retroactive
enter, the guard becomes redundant (harmless — the forced cycle simply never finds a
client that missed its enter) and can be dropped at a later smithay bump.

## Repro sketch for the issue (in-process, smithay-only)

1. Client connects, creates+maps a surface (commit with buffer).
2. Compositor resolves pointer focus onto it (any `pointer.motion(Some(surface), ..)`).
3. Client *then* calls `wl_seat.get_pointer`.
4. Compositor sends further `pointer.motion(Some(surface), ..)` and `pointer.button`.
5. Observe on the client: `button`/`motion` events arrive on the new `wl_pointer` with no
   preceding `enter` — a protocol-order violation from the client's perspective
   (`wl_pointer` events for a surface it was never told the pointer entered).

Note our downstream fixture could not lose the race naturally (it binds its pointer at
seat-bind), which is why our tests arm the mitigation flag directly — an upstream repro
should sequence `get_pointer` after the focus assignment explicitly, as above.

## Filing checklist

- [ ] Confirm the code shape at current smithay `master` (we verified `a166cf4c`
      of the games-on-whales fork; re-check `for_each_focused_pointer`, `GetPointer`,
      and the `PointerInnerHandle::motion` match arms upstream before quoting lines)
- [ ] Title suggestion: *"wl_pointer created after its surface gained pointer focus
      never receives wl_pointer.enter (focus recorded regardless of delivery)"*
- [ ] Link gst-wayland-display PR #44 as the in-the-wild case + mitigation
- [ ] Offer the `GetPointer` catch-up patch as a PR if maintainers agree with the
      direction (small change, one dispatch arm + a targeted send)
- [ ] Michael reviews the final issue text before posting
