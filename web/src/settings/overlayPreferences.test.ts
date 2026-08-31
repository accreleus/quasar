import { describe, expect, it } from "vitest";
import type { SessionOverlayWire } from "../api/me";
import {
  DEFAULT_OVERLAY_PREFERENCES,
  applyPreset,
  fromWire,
  itemsForPreset,
  toWire,
  toggleItem,
} from "./overlayPreferences";

describe("presets", () => {
  it("full shows every item, including mic and fullscreen", () => {
    expect(itemsForPreset("full")).toEqual({
      signal: true, identity: true, codec: true, metrics: true, hint: true,
      capture: true, exit: true, mic: true, fullscreen: true,
    });
  });

  it("minimal is exactly signal, capture, mic, exit (operator request 2026-08-05)", () => {
    expect(itemsForPreset("minimal")).toEqual({
      signal: true, identity: false, codec: false, metrics: false, hint: false,
      capture: true, exit: true, mic: true, fullscreen: false,
    });
  });

  it("metrics keeps the numbers only, but keeps every action including mic/fullscreen", () => {
    expect(itemsForPreset("metrics")).toEqual({
      signal: false, identity: false, codec: false, metrics: true, hint: true,
      capture: true, exit: true, mic: true, fullscreen: true,
    });
  });

  it("every named preset keeps capture and exit on — they are actions, not status", () => {
    for (const name of ["full", "minimal", "metrics"] as const) {
      const items = itemsForPreset(name);
      expect(items.capture).toBe(true);
      expect(items.exit).toBe(true);
    }
  });

  it("mic is on in every named preset; fullscreen is on everywhere except minimal", () => {
    for (const name of ["full", "minimal", "metrics"] as const) {
      expect(itemsForPreset(name).mic).toBe(true);
    }
    expect(itemsForPreset("full").fullscreen).toBe(true);
    expect(itemsForPreset("metrics").fullscreen).toBe(true);
    expect(itemsForPreset("minimal").fullscreen).toBe(false);
  });
});

describe("toggleItem", () => {
  it("becomes custom when the set no longer matches the preset", () => {
    const next = toggleItem(applyPreset(DEFAULT_OVERLAY_PREFERENCES, "full"), "codec");
    expect(next.stripPreset).toBe("custom");
    expect(next.stripItems.codec).toBe(false);
  });

  it("snaps back to a named preset when the set matches one again", () => {
    const off = toggleItem(applyPreset(DEFAULT_OVERLAY_PREFERENCES, "full"), "codec");
    const back = toggleItem(off, "codec");
    expect(back.stripPreset).toBe("full");
  });

  it("recognises a hand-built set that happens to equal a preset", () => {
    // full → drop identity, codec, metrics, hint, fullscreen == the new
    // "minimal" set exactly (signal, capture, mic, exit only).
    let p = applyPreset(DEFAULT_OVERLAY_PREFERENCES, "full");
    p = toggleItem(p, "identity");
    p = toggleItem(p, "codec");
    p = toggleItem(p, "metrics");
    p = toggleItem(p, "hint");
    p = toggleItem(p, "fullscreen");
    expect(p.stripPreset).toBe("minimal");
  });
});

describe("wire conversion", () => {
  it("an absent wire object is all defaults", () => {
    expect(fromWire(undefined)).toEqual(DEFAULT_OVERLAY_PREFERENCES);
  });

  // "left" became a valid dock in the UI v3 amendment; "middle" is the value
  // that is still out of vocabulary.
  it("ignores out-of-vocabulary values rather than crashing the session", () => {
    const p = fromWire({
      strip_position: "middle" as never,
      strip_auto_hide: "never_visible",
    });
    expect(p.stripPosition).toBe(DEFAULT_OVERLAY_PREFERENCES.stripPosition);
    expect(p.stripAutoHide).toBe("never_visible");
  });

  it("round-trips", () => {
    const p = toggleItem(applyPreset(DEFAULT_OVERLAY_PREFERENCES, "metrics"), "signal");
    expect(fromWire(toWire(p))).toEqual(p);
  });
});

describe("capture/exit migration (five pre-existing readout keys only)", () => {
  // Pins the path a pre-capture/exit-rollout stored preference takes: a wire
  // object written before capture/exit existed carries only the five old
  // readout keys. Every absent item key defaults to true (fromWire).
  //
  // "full" and "metrics" still resolve to their own name: PRESET_ITEMS says
  // true for every action in both, matching the "absent defaults true" rule,
  // and neither preset's five-key readout shape has changed since this test
  // was first written.
  //
  // "minimal" is DIFFERENT from when this test was first written (see the
  // PRESET_ITEMS comment in overlayPreferences.ts): the mic/fullscreen change
  // also redefined "minimal" itself — codec and hint flip from on to off — so
  // a legacy minimal item set (codec: true, hint: true) no longer matches the
  // current "minimal" shape (codec: false, hint: false) regardless of the two
  // new keys. It correctly falls to "custom" now; that is not a regression of
  // this test's original guarantee, it is the preset's definition changing
  // out from under old data, and fromWire's item-set-is-authoritative rule
  // means the user's actual five readout values plus defaulted actions are
  // still exactly what gets rendered — only the label changes.
  const legacyItemsFor = (preset: NamedLegacyPreset) => ({
    full: { signal: true, identity: true, codec: true, metrics: true, hint: true },
    minimal: { signal: true, identity: false, codec: true, metrics: false, hint: true },
    metrics: { signal: false, identity: false, codec: false, metrics: true, hint: true },
  })[preset];

  type NamedLegacyPreset = "full" | "minimal" | "metrics";

  it('a legacy wire object (five keys only) for "full" resolves back to "full"', () => {
    const p = fromWire({ strip_preset: "full", strip_items: legacyItemsFor("full") } as never);
    expect(p.stripPreset).toBe("full");
    expect(p.stripItems.capture).toBe(true);
    expect(p.stripItems.exit).toBe(true);
    expect(p.stripItems.mic).toBe(true);
    expect(p.stripItems.fullscreen).toBe(true);
  });

  it('a legacy wire object (five keys only) for "metrics" resolves back to "metrics"', () => {
    const p = fromWire({ strip_preset: "metrics", strip_items: legacyItemsFor("metrics") } as never);
    expect(p.stripPreset).toBe("metrics");
    expect(p.stripItems.capture).toBe(true);
    expect(p.stripItems.exit).toBe(true);
    expect(p.stripItems.mic).toBe(true);
    expect(p.stripItems.fullscreen).toBe(true);
  });

  it('a legacy wire object (five keys only) for "minimal" now resolves to "custom" — the preset itself was redefined', () => {
    const p = fromWire({ strip_preset: "minimal", strip_items: legacyItemsFor("minimal") } as never);
    expect(p.stripPreset).toBe("custom");
    // The user's actual choices survive byte for byte — only the label changed.
    expect(p.stripItems).toEqual({
      signal: true, identity: false, codec: true, metrics: false, hint: true,
      capture: true, exit: true, mic: true, fullscreen: true,
    });
  });
});

describe("mic/fullscreen migration (seven pre-existing keys only)", () => {
  // The subtle case this ticket exists to pin: a wire object written AFTER
  // the capture/exit rollout but BEFORE mic/fullscreen existed carries the
  // seven keys below. fromWire defaults the two absent keys (mic, fullscreen)
  // to true.
  //
  // "full": every key including the two defaults is true → matches the new
  // "full" definition exactly → stays "full".
  //
  // "metrics": readouts unchanged, every action (including the two new,
  // defaulted-true keys) is true → matches the new "metrics" definition
  // (which mirrors "full" on actions) → stays "metrics".
  //
  // "minimal": defaulting the two new keys to true gives fullscreen: true,
  // but the new "minimal" wants fullscreen: false — so it does NOT match,
  // and (independently) it already didn't match on codec/hint per the
  // preset redefinition above. It resolves to "custom". This is the
  // intentional, documented consequence of "minimal" being incompatible with
  // a blanket "absent defaults to true" migration rule for a key it wants
  // off — see the PRESET_ITEMS comment. No data is lost: every stored value
  // is preserved, only the label falls back to "custom".
  const sevenKeyItemsFor = (preset: NamedSevenKeyPreset) => ({
    full: { signal: true, identity: true, codec: true, metrics: true, hint: true, capture: true, exit: true },
    minimal: { signal: true, identity: false, codec: false, metrics: false, hint: false, capture: true, exit: true },
    metrics: { signal: false, identity: false, codec: false, metrics: true, hint: true, capture: true, exit: true },
  })[preset];

  type NamedSevenKeyPreset = "full" | "minimal" | "metrics";

  it('a stored seven-key "full" set resolves to "full", with mic and fullscreen defaulted true', () => {
    const p = fromWire({ strip_preset: "full", strip_items: sevenKeyItemsFor("full") } as never);
    expect(p.stripPreset).toBe("full");
    expect(p.stripItems.mic).toBe(true);
    expect(p.stripItems.fullscreen).toBe(true);
  });

  it('a stored seven-key "metrics" set resolves to "metrics", with mic and fullscreen defaulted true', () => {
    const p = fromWire({ strip_preset: "metrics", strip_items: sevenKeyItemsFor("metrics") } as never);
    expect(p.stripPreset).toBe("metrics");
    expect(p.stripItems.mic).toBe(true);
    expect(p.stripItems.fullscreen).toBe(true);
  });

  it('a stored seven-key "minimal" set (already using the NEW minimal shape) resolves to "custom", because fullscreen defaults true but minimal wants it false', () => {
    const p = fromWire({ strip_preset: "minimal", strip_items: sevenKeyItemsFor("minimal") } as never);
    expect(p.stripPreset).toBe("custom");
    // Every stored value is preserved verbatim; only mic/fullscreen were
    // defaulted (both true, since they were absent from the stored object).
    expect(p.stripItems).toEqual({
      signal: true, identity: false, codec: false, metrics: false, hint: false,
      capture: true, exit: true, mic: true, fullscreen: true,
    });
  });
});

// UI v3 amendment: the overlay draws four docks; the vocabulary listed two.
describe("strip position docks", () => {
  it("accepts every dock from the wire", () => {
    for (const pos of ["top", "bottom", "left", "right"] as const) {
      expect(fromWire({ strip_position: pos }).stripPosition).toBe(pos);
    }
  });

  it("still falls back to bottom for a value it does not know", () => {
    for (const junk of ["middle", "", "TOP", "42"]) {
      expect(
        fromWire({ strip_position: junk } as unknown as SessionOverlayWire).stripPosition,
      ).toBe("bottom");
    }
  });

  it("round-trips a side dock through toWire", () => {
    const prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripPosition: "right" as const };
    expect(toWire(prefs).strip_position).toBe("right");
    expect(fromWire(toWire(prefs)).stripPosition).toBe("right");
  });
});
