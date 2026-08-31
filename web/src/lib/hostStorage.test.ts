import { describe, expect, it } from "vitest";

import { resolveHomeDriver } from "./hostStorage";

// Mirrors control-plane internal/storage.Manager.resolveDriver exactly (wizard-v2
// §S4b/§S4c) — these cases are the client-side twin of the Go table this ticket
// also fixed in inertReason (control-plane #472).
describe("resolveHomeDriver", () => {
  // #473 hard removal (2026-08-25): the docker-volume driver is gone, so an
  // explicit "volume" provider value — which the settings layer now rejects
  // on write anyway — resolves exactly like every other value: off the root
  // alone. There is no longer a "volume" outcome to unit-test for.
  it("a legacy 'volume' provider value resolves off the root alone, never to 'volume'", () => {
    expect(resolveHomeDriver("volume", "/data/homes")).toEqual({ driver: "local", root: "/data/homes" });
    expect(resolveHomeDriver("volume", "")).toEqual({ driver: "misconfigured", root: "" });
  });

  it("explicit 'local' with no root is misconfigured", () => {
    expect(resolveHomeDriver("local", "")).toEqual({ driver: "misconfigured", root: "" });
    expect(resolveHomeDriver("local", null)).toEqual({ driver: "misconfigured", root: "" });
  });

  it("explicit 'local' with a root resolves local", () => {
    expect(resolveHomeDriver("local", "/data/homes")).toEqual({ driver: "local", root: "/data/homes" });
  });

  // 2026-08-10: these four assertions used to expect "volume". That silent
  // downgrade is the thing the operator decision removed, so the case it used to
  // describe now reads exactly like rootless 'local' above — which is the point:
  // 'auto' and 'local' are one behaviour.
  it("'auto' resolves local with a root and is MISCONFIGURED without one — never 'volume'", () => {
    expect(resolveHomeDriver("auto", "/data/homes")).toEqual({ driver: "local", root: "/data/homes" });
    expect(resolveHomeDriver("auto", "")).toEqual({ driver: "misconfigured", root: "" });
    expect(resolveHomeDriver("auto", undefined)).toEqual({ driver: "misconfigured", root: "" });
    expect(resolveHomeDriver("auto", null)).toEqual({ driver: "misconfigured", root: "" });
  });

  it("defaults to the 'auto' rule when provider is undefined", () => {
    expect(resolveHomeDriver(undefined, "/data/homes")).toEqual({ driver: "local", root: "/data/homes" });
    expect(resolveHomeDriver(undefined, "")).toEqual({ driver: "misconfigured", root: "" });
  });

  it("trims whitespace-only roots to empty (treated as unset)", () => {
    expect(resolveHomeDriver("auto", "   ")).toEqual({ driver: "misconfigured", root: "" });
  });

  it("only an explicit 'volume' setting can ever yield the volume driver", () => {
    for (const provider of ["auto", "local", undefined] as const) {
      for (const root of ["/data/homes", "", null, undefined]) {
        expect(resolveHomeDriver(provider, root).driver).not.toBe("volume");
      }
    }
  });
});
