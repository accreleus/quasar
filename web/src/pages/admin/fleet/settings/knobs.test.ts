import { describe, expect, it } from "vitest";
import type { ConfigKnob } from "../../../../api/types";
import { coerceEffective, groupFor, knobHelp, knobLabel, shallowEqualOverrides, valueLabel } from "./knobs";

function knob(overrides: Partial<ConfigKnob>): ConfigKnob {
  return {
    key: "some_key",
    type: "string",
    default: null,
    nullable: true,
    class: "live",
    env_var: "QUASAR_SOME_KEY",
    ...overrides,
  } as ConfigKnob;
}

describe("coerceEffective", () => {
  it("parses bool wire values", () => {
    const k = knob({ key: "abr_enabled", type: "bool" });
    expect(coerceEffective(k, "true")).toBe(true);
    expect(coerceEffective(k, "1")).toBe(true);
    expect(coerceEffective(k, "false")).toBe(false);
    expect(coerceEffective(k, "0")).toBe(false);
  });

  it("parses numeric wire values, falling back to the raw string when unparsable", () => {
    const intKnob = knob({ key: "gop", type: "int" });
    expect(coerceEffective(intKnob, "120")).toBe(120);
    expect(coerceEffective(intKnob, "not-a-number")).toBe("not-a-number");
    const floatKnob = knob({ key: "abr_floor_ratio", type: "float" });
    expect(coerceEffective(floatKnob, "0.35")).toBe(0.35);
  });

  it("passes enum/string values through unchanged", () => {
    expect(coerceEffective(knob({ type: "enum" }), "NVENC")).toBe("NVENC");
    expect(coerceEffective(knob({ type: "string" }), "/dev/dri/renderD128")).toBe("/dev/dri/renderD128");
  });
});

describe("valueLabel", () => {
  it("renders Unset for undefined", () => {
    expect(valueLabel(undefined)).toBe("Unset");
  });

  it("renders On/Off for booleans", () => {
    expect(valueLabel(true)).toBe("On");
    expect(valueLabel(false)).toBe("Off");
  });

  it("stringifies everything else", () => {
    expect(valueLabel(120)).toBe("120");
    expect(valueLabel("NVENC")).toBe("NVENC");
  });
});

describe("groupFor / knobLabel / knobHelp", () => {
  it("uses the catalog copy when the key is known", () => {
    const k = knob({ key: "idle_timeout_secs", type: "int" });
    expect(groupFor(k)).toBe("runtime");
    expect(knobLabel(k)).toBe("Idle timeout");
    expect(knobHelp(k)).toMatch(/Stops idle sessions/);
  });

  it("falls back to a humanized key and a generic help line for unknown knobs", () => {
    const k = knob({ key: "some_new_knob", class: "live" });
    expect(knobLabel(k)).toBe("some new knob");
    expect(knobHelp(k)).toBe("Advanced node-agent runtime setting.");
  });

  it("puts an unknown restart-class knob in Encoder and GPU, and an unknown live-class knob in Advanced", () => {
    expect(groupFor(knob({ key: "unknown_restart", class: "restart" }))).toBe("encoder");
    expect(groupFor(knob({ key: "unknown_live", class: "live" }))).toBe("advanced");
  });
});

describe("shallowEqualOverrides", () => {
  it("is true for two maps with the same keys and values", () => {
    expect(shallowEqualOverrides({ a: 1, b: "x" }, { a: 1, b: "x" })).toBe(true);
  });

  it("is false when a key's value differs, including null vs a value", () => {
    expect(shallowEqualOverrides({ a: 1 }, { a: 2 })).toBe(false);
    expect(shallowEqualOverrides({ a: null }, { a: 1 })).toBe(false);
  });

  it("is false when the key sets differ, even at equal length", () => {
    expect(shallowEqualOverrides({ a: 1 }, { b: 1 })).toBe(false);
  });

  it("does not depend on key order", () => {
    expect(shallowEqualOverrides({ a: 1, b: 2 }, { b: 2, a: 1 })).toBe(true);
  });

  it("is true for two empty maps", () => {
    expect(shallowEqualOverrides({}, {})).toBe(true);
  });
});
