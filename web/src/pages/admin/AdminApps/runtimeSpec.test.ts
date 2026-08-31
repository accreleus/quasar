import { describe, expect, it } from "vitest";
import { parseSpec, specToRecord } from "./runtimeSpec";

describe("parseSpec", () => {
  it("parses a complete, well-formed spec", () => {
    const raw = {
      image: "quasar-agent-dev:latest",
      args: ["--foo", "--bar=baz"],
      env: { DISPLAY: ":0", HOME: "/root" },
      mounts: ["/host:/container"],
      gpu: true,
    };
    expect(parseSpec(raw)).toEqual({
      image: "quasar-agent-dev:latest",
      args: ["--foo", "--bar=baz"],
      env: { DISPLAY: ":0", HOME: "/root" },
      mounts: ["/host:/container"],
      gpu: true,
      extras: {},
    });
  });

  it("falls back to safe defaults for missing fields", () => {
    expect(parseSpec({})).toEqual({
      image: "",
      args: [],
      env: {},
      mounts: [],
      gpu: false,
      extras: {},
    });
  });

  it("collects unknown keys into extras", () => {
    expect(
      parseSpec({ image: "x", no_new_privileges: false, future_key: [1] }),
    ).toMatchObject({
      image: "x",
      extras: { no_new_privileges: false, future_key: [1] },
    });
  });

  it("rejects a non-string image", () => {
    expect(parseSpec({ image: 42 })).toMatchObject({ image: "" });
  });

  it("rejects non-array args and mounts", () => {
    expect(parseSpec({ args: "foo", mounts: "bar" })).toMatchObject({
      args: [],
      mounts: [],
    });
  });

  it("rejects an array-shaped env (uses empty object)", () => {
    expect(parseSpec({ env: [["KEY", "val"]] })).toMatchObject({ env: {} });
  });

  it("rejects non-boolean gpu", () => {
    expect(parseSpec({ gpu: 1 })).toMatchObject({ gpu: false });
    expect(parseSpec({ gpu: "true" })).toMatchObject({ gpu: false });
  });

  it("accepts gpu=false", () => {
    expect(parseSpec({ gpu: false })).toMatchObject({ gpu: false });
  });
});

describe("specToRecord", () => {
  it("round-trips a spec through specToRecord → parseSpec", () => {
    const spec = {
      image: "my-image:1.0",
      args: ["--headless"],
      env: { FOO: "bar" },
      mounts: ["/data:/data:ro"],
      gpu: true,
      extras: { no_new_privileges: false },
    };
    expect(parseSpec(specToRecord(spec) as Record<string, unknown>)).toEqual(spec);
  });

  it("preserves an empty spec", () => {
    const spec = { image: "", args: [], env: {}, mounts: [], gpu: false, extras: {} };
    expect(specToRecord(spec)).toEqual({
      image: "",
      args: [],
      env: {},
      mounts: [],
      gpu: false,
    });
  });

  it("spreads extras without letting them shadow edited fields", () => {
    const rec = specToRecord({
      image: "edited:1",
      args: [],
      env: {},
      mounts: [],
      gpu: true,
      // Hostile/stale extras must not override the form's edited values.
      extras: { no_new_privileges: false, image: "stale:0", gpu: false },
    });
    expect(rec).toEqual({
      image: "edited:1",
      args: [],
      env: {},
      mounts: [],
      gpu: true,
      no_new_privileges: false,
    });
  });

  it("full raw → parse → serialize round-trip preserves unknown keys (regression: no_new_privileges)", () => {
    const raw = {
      image: "ghcr.io/games-on-whales/xfce:edge",
      args: [],
      env: {},
      mounts: [],
      gpu: true,
      no_new_privileges: false,
    };
    expect(specToRecord(parseSpec(raw))).toEqual(raw);
  });
});
