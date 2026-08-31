import { describe, expect, it } from "vitest";
import type { AdminApp, ForceScanResult, LibraryUnpublishedItem } from "../../../api/types";
import { inertReasonCopy, lastScanText, manualAppCount, scanResultToast, steamCounts } from "./sourcesDerived";

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "a1",
    name: "App",
    parent_app_id: null,
    library_provider: "",
    ...over,
  } as AdminApp;
}

function unpublished(over: Partial<LibraryUnpublishedItem> = {}): LibraryUnpublishedItem {
  return {
    external_source: "steam",
    external_id: "123",
    name: "A game",
    suppressed_by: "none",
    users: 1,
    last_seen_at: "2026-08-01T00:00:00Z",
    has_tile: false,
    ...over,
  } as LibraryUnpublishedItem;
}

describe("steamCounts", () => {
  it("is all-zero when no app is marked as the Steam provider", () => {
    expect(steamCounts([app()], [unpublished()], null)).toEqual({ imported: 0, discovered: 0 });
  });

  it("counts derived tiles under the provider as imported, and adds unpublished to discovered", () => {
    const apps = [
      app({ id: "steam", library_provider: "steam" }),
      app({ id: "t1", parent_app_id: "steam" }),
      app({ id: "t2", parent_app_id: "steam" }),
      app({ id: "other", parent_app_id: "some-other-provider" }),
    ];
    const un = [unpublished({ external_id: "1" }), unpublished({ external_id: "2" }), unpublished({ external_id: "3" })];
    expect(steamCounts(apps, un, "steam")).toEqual({ imported: 2, discovered: 5 });
  });

  it("ignores apps that are not derived from the given provider", () => {
    const apps = [app({ id: "t1", parent_app_id: "some-other-provider" })];
    expect(steamCounts(apps, [], "steam")).toEqual({ imported: 0, discovered: 0 });
  });
});

describe("manualAppCount", () => {
  it("counts apps with no provider and no parent", () => {
    const apps = [
      app({ id: "m1" }),
      app({ id: "m2" }),
      app({ id: "steam", library_provider: "steam" }),
      app({ id: "t1", parent_app_id: "steam" }),
    ];
    expect(manualAppCount(apps)).toBe(2);
  });

  it("is zero when every app is either a provider or derived", () => {
    const apps = [
      app({ id: "steam", library_provider: "steam" }),
      app({ id: "t1", parent_app_id: "steam" }),
    ];
    expect(manualAppCount(apps)).toBe(0);
  });
});

describe("lastScanText", () => {
  it("renders 'never' when no scan has completed", () => {
    expect(lastScanText(null)).toBe("never");
  });

  it("renders the relative time otherwise", () => {
    const iso = new Date(Date.now() - 5 * 60_000).toISOString();
    expect(lastScanText(iso)).toBe("5 minutes ago");
  });
});

describe("inertReasonCopy", () => {
  it("renders the server's prose verbatim apart from a capital first letter", () => {
    expect(inertReasonCopy("library discovery is switched off")).toBe("Library discovery is switched off");
    expect(
      inertReasonCopy("QUASAR_LIBRARY_SCAN_INTERVAL is 0, which disables discovery regardless of the instance setting"),
    ).toBe("QUASAR_LIBRARY_SCAN_INTERVAL is 0, which disables discovery regardless of the instance setting");
  });
  it("sentence-cases an unrecognised reason rather than pasting it raw or going blank", () => {
    expect(inertReasonCopy("some future reason the client has never seen")).toBe(
      "Some future reason the client has never seen",
    );
  });

  it("passes an empty reason through unchanged", () => {
    expect(inertReasonCopy("")).toBe("");
  });
});

function forceScanResult(over: Partial<ForceScanResult> = {}): ForceScanResult {
  return { queued: 0, skipped: 0, eligible: 0, inert_reason: "", ...over };
}

describe("scanResultToast", () => {
  it("is info with the reason when the instance is inert", () => {
    expect(scanResultToast(forceScanResult({ inert_reason: "no host has a managed-home storage root" }))).toEqual({
      variant: "info",
      title: "no host has a managed-home storage root",
    });
  });

  it("is success with the queued count and the follow-through sentence when scans were queued", () => {
    const { variant, title } = scanResultToast(forceScanResult({ queued: 3, eligible: 3 }));
    expect(variant).toBe("success");
    expect(title).toBe(
      "Queued 3 scans. The agent picks these up within about a minute; tiles appear once each scan reports back.",
    );
  });

  it("singularizes 'scan' for a queued count of one", () => {
    const { title } = scanResultToast(forceScanResult({ queued: 1, eligible: 1 }));
    expect(title).toMatch(/^Queued 1 scan\./);
  });

  it("notes skipped scans already in progress alongside a queued count", () => {
    const { title } = scanResultToast(forceScanResult({ queued: 2, skipped: 1, eligible: 3 }));
    expect(title).toBe(
      "Queued 2 scans (1 already in progress, left alone). The agent picks these up within about a minute; tiles appear once each scan reports back.",
    );
  });

  it("is info saying nothing new to do when nothing was queued and the instance is not inert", () => {
    expect(scanResultToast(forceScanResult({ queued: 0, eligible: 3 }))).toEqual({
      variant: "info",
      title: "Already in progress. Every eligible scan is already queued or being walked. No new work to do.",
    });
  });
});
