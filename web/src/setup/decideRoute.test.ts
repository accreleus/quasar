import { describe, expect, it } from "vitest";
import { shouldRouteToSetup } from "./decideRoute";

describe("shouldRouteToSetup", () => {
  it("does not redirect while status is unknown (loading or fetch failed)", () => {
    expect(shouldRouteToSetup(null, "/login")).toBe(false);
    expect(shouldRouteToSetup(null, "/app")).toBe(false);
  });

  it("sends a virgin instance (no admin yet) to /setup from any other path", () => {
    const status = { admin_exists: false, setup_completed: false };
    expect(shouldRouteToSetup(status, "/login")).toBe(true);
    expect(shouldRouteToSetup(status, "/app")).toBe(true);
    expect(shouldRouteToSetup(status, "/")).toBe(true);
  });

  it("does not redirect a virgin instance away from /setup itself", () => {
    const status = { admin_exists: false, setup_completed: false };
    expect(shouldRouteToSetup(status, "/setup")).toBe(false);
  });

  it("never redirects once an admin exists, regardless of setup_completed", () => {
    expect(shouldRouteToSetup({ admin_exists: true, setup_completed: false }, "/login")).toBe(false);
    expect(shouldRouteToSetup({ admin_exists: true, setup_completed: true }, "/app")).toBe(false);
  });
});
