// AppsTab — v3 Library > Apps tab (handoff §A.9). Covers what moved from the
// old AdminApps page (enable/disable, Ignore, Delete, catalogue-wide artwork
// re-fetch — see AdminApps/index.test.tsx in git history for their original
// coverage) plus what's new here: the toolbar's segment/search/source filter,
// the `?preset=` drill-in, and discovered-but-unpublished rows.

import { MemoryRouter, useLocation } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { AppsTab } from "./AppsTab";
import { ToastProvider } from "../../../components/Toast";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { LIBRARY_TABS } from "../../../components/shell/sectionTabs";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AdminApp, LibraryUnpublishedItem, RuntimePreset } from "../../../api/types";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "app-1",
    name: "Half-Life 2",
    description: "",
    cover_url: null,
    hero_url: null,
    kind: "game",
    parent_app_id: null,
    external_source: "",
    external_id: "",
    origin: "manual",
    library_provider: "",
    library_discovery_suspended: false,
    favourite: false,
    default_width: 1920,
    default_height: 1080,
    default_fps: 60,
    default_bitrate_kbps: 8000,
    default_profile_id: null,
    profile_policy: "inherit",
    enabled: true,
    default_vram_mb: 0,
    default_encode_slots: 1,
    runtime_spec: {},
    managed_home: false,
    home_container_path: "",
    runtime_preset_id: null,
    launchable_profile_ids: [],
    sessions_30d: 0,
    ...over,
  } as AdminApp;
}

function unpublished(over: Partial<LibraryUnpublishedItem> = {}): LibraryUnpublishedItem {
  return {
    external_source: "steam",
    external_id: "228980",
    name: "Deep Rock Galactic",
    suppressed_by: "other",
    users: 3,
    last_seen_at: "2026-08-01T00:00:00Z",
    has_tile: false,
    ...over,
  };
}

function preset(over: Partial<RuntimePreset> = {}): RuntimePreset {
  return {
    id: "preset-1",
    name: "Proton GPU",
    description: "",
    image: "",
    args: [],
    env: {},
    mounts: [],
    managed_home: false,
    home_container_path: "",
    network: "",
    used_by: [],
    created_at: "",
    updated_at: "",
    ...over,
  } as RuntimePreset;
}

// Exposes the router's current search string for tests asserting the toolbar
// writes back to the URL (MemoryRouter doesn't touch window.location).
function LocationSearchProbe() {
  const location = useLocation();
  return <div data-testid="search-probe">{location.search}</div>;
}

function renderPage(initialEntries: string[] = ["/admin/library/apps"]) {
  // The page publishes its head to the section container, so tests mount the
  // container it lives in — the toolbar's actions and the tab count render there.
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <ToastProvider>
        <SectionHeadProvider title="Library" tabs={LIBRARY_TABS}>
          <AppsTab />
        </SectionHeadProvider>
      </ToastProvider>
      <LocationSearchProbe />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.listAdminApps.mockResolvedValue({ items: [], next_cursor: null });
  mocked.listUnpublishedLibraryItems.mockResolvedValue({ items: [] });
  mocked.listRuntimePresets.mockResolvedValue({ items: [] });
  mocked.listLaunchProfiles.mockResolvedValue({ items: [] });
});

describe("head sub, actions and tab count", () => {
  it("publishes enabled/total, the pending count, and the Scan/Add actions", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "a1", enabled: true }), app({ id: "a2", enabled: false })],
      next_cursor: null,
    });
    renderPage();

    await screen.findByText(/1 of 2 apps enabled/);
    expect(screen.getByText(/0 discovered titles not yet imported/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Scan sources/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Add app/ })).toBeTruthy();

    const tab = screen.getByRole("tab", { name: /Apps/ });
    expect(tab.textContent).toContain("2");
  });
});

describe("toolbar filtering", () => {
  it("filters by segment, then search, then source", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "g1", name: "Stardew Valley", kind: "game" }),
        app({ id: "d1", name: "Blender Studio", kind: "desktop" }),
        app({ id: "s1", name: "Portal 2", parent_app_id: "provider-1" }),
      ],
      next_cursor: null,
    });
    renderPage();

    await screen.findByText("Stardew Valley");
    expect(screen.getByText("Blender Studio")).toBeTruthy();
    expect(screen.getByText("Portal 2")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: /^Games/ }));
    await waitFor(() => expect(screen.queryByText("Blender Studio")).toBeNull());
    expect(screen.getByText("Stardew Valley")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: /^All/ }));
    await screen.findByText("Blender Studio");

    fireEvent.change(screen.getByPlaceholderText("Filter apps"), { target: { value: "portal" } });
    await waitFor(() => expect(screen.queryByText("Stardew Valley")).toBeNull());
    expect(screen.getByText("Portal 2")).toBeTruthy();

    fireEvent.change(screen.getByPlaceholderText("Filter apps"), { target: { value: "" } });
    fireEvent.change(screen.getByLabelText("Filter by source"), { target: { value: "steam" } });
    await waitFor(() => expect(screen.queryByText("Stardew Valley")).toBeNull());
    expect(screen.getByText("Portal 2")).toBeTruthy();
  });
});

describe("?segment= and ?source= from the URL", () => {
  it("honours a ?segment=pending link from SourcesTab", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "a1", name: "Stardew Valley" }),
        app({ id: "p1", name: "Steam", kind: "launcher", library_provider: "steam" }),
      ],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "228980", name: "Deep Rock Galactic" })],
    });
    renderPage(["/admin/library/apps?segment=pending"]);

    await screen.findByText("Deep Rock Galactic");
    expect(screen.queryByText("Stardew Valley")).toBeNull();
    expect(screen.getByRole("tab", { name: /^Pending import/, selected: true })).toBeTruthy();
  });

  it("honours a ?source=manual link from SourcesTab", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "m1", name: "Blender Studio" }),
        app({ id: "s1", name: "Portal 2", parent_app_id: "provider-1" }),
      ],
      next_cursor: null,
    });
    renderPage(["/admin/library/apps?source=manual"]);

    await screen.findByText("Blender Studio");
    expect(screen.queryByText("Portal 2")).toBeNull();
    expect((screen.getByLabelText("Filter by source") as HTMLSelectElement).value).toBe("manual");
  });

  it("falls back to all for an unknown segment or source value", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "m1", name: "Blender Studio" }),
        app({ id: "s1", name: "Portal 2", parent_app_id: "provider-1" }),
      ],
      next_cursor: null,
    });
    renderPage(["/admin/library/apps?segment=bogus&source=nonsense"]);

    await screen.findByText("Blender Studio");
    expect(screen.getByText("Portal 2")).toBeTruthy();
    expect(screen.getByRole("tab", { name: /^All/, selected: true })).toBeTruthy();
    expect((screen.getByLabelText("Filter by source") as HTMLSelectElement).value).toBe("all");
  });

  it("writes ?segment= and ?source= as the user changes the toolbar, and drops them back at all", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "g1", name: "Stardew Valley", kind: "game" }),
        app({ id: "d1", name: "Blender Studio", kind: "desktop" }),
      ],
      next_cursor: null,
    });
    renderPage();
    const router = () => new URLSearchParams(screen.getByTestId("search-probe").textContent ?? "");

    await screen.findByText("Stardew Valley");
    fireEvent.click(screen.getByRole("tab", { name: /^Games/ }));
    await waitFor(() => expect(screen.queryByText("Blender Studio")).toBeNull());
    expect(router().get("segment")).toBe("games");

    fireEvent.change(screen.getByLabelText("Filter by source"), { target: { value: "steam" } });
    await waitFor(() => expect(router().get("source")).toBe("steam"));

    fireEvent.click(screen.getByRole("tab", { name: /^All/ }));
    await waitFor(() => expect(router().get("segment")).toBeNull());
  });
});

describe("?preset= drill-in", () => {
  it("filters to one runtime preset and shows the removable chip", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "a1", name: "Stardew Valley", runtime_preset_id: "preset-1" }),
        app({ id: "a2", name: "Portal 2", runtime_preset_id: "preset-2" }),
      ],
      next_cursor: null,
    });
    mocked.listRuntimePresets.mockResolvedValue({ items: [preset({ id: "preset-1", name: "Proton GPU" })] });
    renderPage(["/admin/library/apps?preset=preset-1"]);

    await screen.findByText("Stardew Valley");
    expect(screen.queryByText("Portal 2")).toBeNull();
    expect(screen.getByText(/Runtime preset:/)).toBeTruthy();
    expect(screen.getByText("Proton GPU")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Clear runtime preset filter" }));
    await screen.findByText("Portal 2");
  });
});

describe("pending-import rows", () => {
  it("shows a discovered title and imports it with rule=allow", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "p1", name: "Steam", kind: "launcher", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "228980", name: "Deep Rock Galactic" })],
    });
    mocked.setLibraryRule.mockResolvedValue({
      rule: {
        external_source: "steam",
        external_id: "228980",
        rule: "allow",
        note: "",
        created_by: null,
        created_at: "",
      },
      disabled: false,
      revoked: 0,
    });
    renderPage();

    await screen.findByText("Deep Rock Galactic");
    expect(mocked.listUnpublishedLibraryItems).toHaveBeenCalledWith("tok", "p1");

    fireEvent.click(screen.getByRole("button", { name: "Import" }));

    await waitFor(() =>
      expect(mocked.setLibraryRule).toHaveBeenCalledWith("tok", "p1", "228980", {
        rule: "allow",
        external_source: "steam",
      }),
    );
    await screen.findByText(/will publish on the next scan/);
  });

  it("stays visible under the Games segment", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "p1", name: "Steam", kind: "launcher", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "228980", name: "Deep Rock Galactic" })],
    });
    renderPage();

    await screen.findByText("Deep Rock Galactic");
    fireEvent.click(screen.getByRole("tab", { name: /^Games/ }));
    expect(screen.getByText("Deep Rock Galactic")).toBeTruthy();
  });

  it("ignores a pending title with rule=ignore", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "p1", name: "Steam", kind: "launcher", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "228980", name: "Deep Rock Galactic" })],
    });
    mocked.setLibraryRule.mockResolvedValue({
      rule: {
        external_source: "steam",
        external_id: "228980",
        rule: "ignore",
        note: "",
        created_by: null,
        created_at: "",
      },
      disabled: false,
      revoked: 0,
    });
    renderPage();

    await screen.findByText("Deep Rock Galactic");
    fireEvent.click(screen.getByRole("button", { name: "Actions for Deep Rock Galactic" }));
    fireEvent.click(screen.getByText("Ignore"));

    await waitFor(() =>
      expect(mocked.setLibraryRule).toHaveBeenCalledWith("tok", "p1", "228980", {
        rule: "ignore",
        external_source: "steam",
      }),
    );
  });

  it("offers Un-ignore instead of Ignore once suppressed_by is a prior ignore rule", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "p1", name: "Steam", kind: "launcher", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "228980", name: "Deep Rock Galactic", suppressed_by: "rule_ignore" })],
    });
    renderPage();

    await screen.findByText("Deep Rock Galactic");
    expect(screen.getByText(/^Ignored/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Actions for Deep Rock Galactic" }));
    expect(screen.getByText("Un-ignore")).toBeTruthy();
    expect(screen.queryByText("Ignore")).toBeNull();
  });
});

describe("Scan sources", () => {
  it("reports how many scans were queued", async () => {
    mocked.forceLibraryScan.mockResolvedValue({ queued: 2, skipped: 0, eligible: 2, inert_reason: "" });
    renderPage();

    await screen.findByText("No apps yet");
    fireEvent.click(screen.getByRole("button", { name: /Scan sources/ }));

    await waitFor(() => expect(mocked.forceLibraryScan).toHaveBeenCalledWith("tok"));
    await screen.findByText("2 scans queued");
  });

  it("reports nothing new to queue when queued is 0 with no inert_reason", async () => {
    mocked.forceLibraryScan.mockResolvedValue({ queued: 0, skipped: 3, eligible: 3, inert_reason: "" });
    renderPage();

    await screen.findByText("No apps yet");
    fireEvent.click(screen.getByRole("button", { name: /Scan sources/ }));

    await waitFor(() => expect(mocked.forceLibraryScan).toHaveBeenCalled());
    await screen.findByText("Nothing new to queue; scans are already waiting on the agent.");
  });

  it("surfaces inert_reason as a warning, not a fabricated error", async () => {
    mocked.forceLibraryScan.mockResolvedValue({
      queued: 0,
      skipped: 0,
      eligible: 0,
      inert_reason: "no registered host has a managed-home storage root",
    });
    renderPage();

    await screen.findByText("No apps yet");
    fireEvent.click(screen.getByRole("button", { name: /Scan sources/ }));

    await waitFor(() => expect(mocked.forceLibraryScan).toHaveBeenCalled());
    await screen.findByText("no registered host has a managed-home storage root");
  });
});

describe("enabled switch", () => {
  it("PATCHes { enabled: false } through updateApp", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "a1", name: "Stardew Valley", enabled: true })],
      next_cursor: null,
    });
    mocked.updateApp.mockResolvedValue({ app: app({ id: "a1", name: "Stardew Valley", enabled: false }) });
    renderPage();

    await screen.findByText("Stardew Valley");
    fireEvent.click(screen.getByRole("switch", { name: "Disable Stardew Valley" }));

    await waitFor(() => expect(mocked.updateApp).toHaveBeenCalledWith("tok", "a1", { enabled: false }));
  });
});

describe("row menu", () => {
  it("offers Ignore only for a discovered tile", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "a1", name: "Manual App" }),
        app({ id: "a2", name: "Steam Tile", parent_app_id: "provider-1", external_id: "1" }),
      ],
      next_cursor: null,
    });
    renderPage();

    await screen.findByText("Manual App");
    fireEvent.click(screen.getByRole("button", { name: "Actions for Manual App" }));
    expect(
      within(screen.getByRole("menu", { name: "Actions for Manual App" })).queryByText("Ignore"),
    ).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Actions for Steam Tile" }));
    expect(
      within(screen.getByRole("menu", { name: "Actions for Steam Tile" })).getByText("Ignore"),
    ).toBeTruthy();
  });

  it("Delete opens the confirm modal and deleteApp runs on confirm", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "a1", name: "Manual App" })],
      next_cursor: null,
    });
    mocked.deleteApp.mockResolvedValue(undefined);
    renderPage();

    await screen.findByText("Manual App");
    fireEvent.click(screen.getByRole("button", { name: "Actions for Manual App" }));
    fireEvent.click(screen.getByText("Delete"));
    fireEvent.click(await screen.findByRole("button", { name: "Delete app" }));

    await waitFor(() => expect(mocked.deleteApp).toHaveBeenCalledWith("tok", "a1"));
  });
});

describe("a failed pending fan-out", () => {
  it("surfaces as a page error instead of reading as zero discovered titles", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "p1", name: "Steam", kind: "launcher", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockRejectedValue(
      new ApiError(500, "internal", "could not reach the database"),
    );
    renderPage();

    await screen.findByRole("alert");
    expect(screen.getByText("could not reach the database")).toBeTruthy();
    expect(screen.queryByText(/discovered title/)).toBeNull();
  });
});

describe("catalogue-wide artwork re-fetch", () => {
  it("calls reresolveAllArtwork with the force checkbox's value", async () => {
    mocked.reresolveAllArtwork.mockResolvedValue({ resolved: 3, total: 5, skipped_locked: 2, failed: 0 });
    renderPage();

    await screen.findByText("No apps yet");
    fireEvent.click(screen.getByRole("button", { name: "More app actions" }));
    fireEvent.click(screen.getByText("Re-fetch artwork for every app"));

    fireEvent.click(screen.getByLabelText("Overwrite locked artwork too"));
    fireEvent.click(screen.getByRole("button", { name: "Re-fetch" }));

    await waitFor(() => expect(mocked.reresolveAllArtwork).toHaveBeenCalledWith("tok", true));
    await screen.findByText(/Re-fetched artwork for 3 of 5 apps/);
  });
});
