// SourcesTab — re-homes AdminSteamLibrary's scan-control assertions
// (census counts, recent scans, provider-app link, force scan, inert reason)
// plus the new composition: derived meta counts, the Steam/Manual apps source
// rows, and the SteamGridDB artwork key moved in from Settings.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type {
  AdminApp,
  ForceScanResult,
  LibraryRecentScan,
  LibraryStatus,
  LibraryUnpublishedItem,
  SecretStatus,
  SecretsResponse,
  SettingsResponse,
} from "../../../api/types";
import { ToastProvider } from "../../../components/Toast";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { LIBRARY_TABS } from "../../../components/shell/sectionTabs";
import { SourcesTab } from "./SourcesTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function settingsResponse(over: Partial<SettingsResponse["settings"]> = {}): SettingsResponse {
  return {
    settings: {
      registration_mode: "closed",
      storage_provider: "local",
      mic_capture_enabled: false,
      library_discovery_enabled: true,
      library_discovery_interval_minutes: 360,
      library_discovery_appdetails_enabled: false,
      updated_by: null,
      updated_at: "2026-07-29T00:00:00Z",
      ...over,
    },
  } as SettingsResponse;
}

function libraryStatus(over: Partial<LibraryStatus> = {}): LibraryStatus {
  return {
    enabled: true,
    storage_provider: "local",
    scan_interval_secs: 21600,
    appdetails_lookup: false,
    inert_reason: "",
    scans: { pending: 0, claimed: 0, reported: 0, failed: 0 },
    interval_overridden_by_env: false,
    appdetails_overridden_by_env: false,
    last_scan_completed_at: null,
    recent_scans: [],
    ...over,
  } as LibraryStatus;
}

function recentScan(over: Partial<LibraryRecentScan> = {}): LibraryRecentScan {
  return {
    user: "alice",
    host: "hermes",
    state: "reported",
    completed_at: "2026-08-01T10:00:00Z",
    observed: 3,
    suppressed: 1,
    created: 2,
    disabled: 0,
    granted: 1,
    revoked: 0,
    rejected: 0,
    backfilled: 1,
    error: "",
    ...over,
  };
}

function forceScanResult(over: Partial<ForceScanResult> = {}): ForceScanResult {
  return { queued: 0, skipped: 0, eligible: 0, inert_reason: "", ...over };
}

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "a1",
    name: "App",
    description: "",
    kind: "game",
    enabled: true,
    library_provider: "",
    parent_app_id: null,
    external_id: "",
    external_source: "",
    origin: "manual",
    default_vram_mb: 0,
    default_encode_slots: 1,
    runtime_spec: {},
    managed_home: false,
    home_container_path: "",
    runtime_preset_id: null,
    launchable_profile_ids: [],
    ...over,
  } as unknown as AdminApp;
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

function artworkSecret(over: Partial<SecretStatus> = {}): SecretStatus {
  return {
    name: "artwork.steamgriddb.api_key",
    label: "SteamGridDB API key",
    description: "Looks up cover artwork for apps in your catalogue.",
    env_var: "QUASAR_STEAMGRIDDB_API_KEY",
    docs_url: "",
    configured: false,
    readable: false,
    hint: "",
    env_set: false,
    origin: "none",
    key_version: 0,
    updated_by: null,
    updated_at: null,
    ...over,
  } as SecretStatus;
}

function secretsResponse(secrets: SecretStatus[], over: Partial<SecretsResponse> = {}): SecretsResponse {
  return { secrets, master_key_configured: true, key_versions: [1], ...over };
}

let lastLocation = "";

function LocationProbe() {
  const loc = useLocation();
  lastLocation = loc.pathname + loc.search;
  return null;
}

function renderPage() {
  lastLocation = "";
  return render(
    <ToastProvider>
      <MemoryRouter>
        <SectionHeadProvider title="Library" tabs={LIBRARY_TABS}>
          <SourcesTab />
        </SectionHeadProvider>
        <LocationProbe />
      </MemoryRouter>
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getSettings.mockResolvedValue(settingsResponse());
  mocked.getLibraryStatus.mockResolvedValue(libraryStatus());
  mocked.listAdminApps.mockResolvedValue({ items: [], next_cursor: null });
  mocked.listUnpublishedLibraryItems.mockResolvedValue({ items: [] });
  mocked.listSecrets.mockResolvedValue(secretsResponse([artworkSecret()]));
});

describe("SourcesTab — head", () => {
  it("publishes the sub-line and a sources count of 2 to the section head", async () => {
    renderPage();

    await screen.findByText("Steam");
    expect(
      screen.getByText("Where catalog content and cover art come from. Everything a source discovers lands in Apps."),
    ).toBeInTheDocument();
    const sourcesTab = screen.getByRole("tab", { name: /Sources/ });
    expect(sourcesTab).toHaveTextContent("2");
  });
});

describe("SourcesTab — Steam row", () => {
  it("derives discovered/imported/last-scan meta from apps + unpublished + status", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "steam", name: "Steam", library_provider: "steam" }),
        app({ id: "t1", name: "Game 1", parent_app_id: "steam" }),
        app({ id: "t2", name: "Game 2", parent_app_id: "steam" }),
      ],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "1" }), unpublished({ external_id: "2" })],
    });
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({ last_scan_completed_at: new Date(Date.now() - 21 * 60_000).toISOString() }),
    );
    renderPage();

    await waitFor(() => expect(mocked.listUnpublishedLibraryItems).toHaveBeenCalledWith("tok", "steam"));
    await screen.findByText(/4 titles discovered/);
    expect(screen.getByText("2 imported")).toBeInTheDocument();
    expect(screen.getByText(/last scan 21 minutes ago/)).toBeInTheDocument();
  });

  it("PATCHes library_discovery_enabled when the Steam switch flips", async () => {
    mocked.updateSettings.mockResolvedValue(settingsResponse({ library_discovery_enabled: false }));
    renderPage();

    const toggle = await screen.findByRole("checkbox", { name: "Steam discovery" });
    fireEvent.click(toggle);

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { library_discovery_enabled: false }),
    );
  });

  it("still renders the row when library_discovery_enabled is false", async () => {
    mocked.getSettings.mockResolvedValue(settingsResponse({ library_discovery_enabled: false }));
    renderPage();

    const toggle = await screen.findByRole("checkbox", { name: "Steam discovery" });
    expect(toggle).not.toBeChecked();
    expect(screen.getByText("Steam")).toBeInTheDocument();
  });

  it("shows the instance-level inert_reason as a warning note", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({ inert_reason: "library discovery is switched off" }),
    );
    renderPage();

    await screen.findByText("Library discovery is switched off");
  });

  it("sentence-cases an unrecognised inert_reason rather than pasting it raw", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({ inert_reason: "no registered host has a managed-home storage root" }),
    );
    renderPage();

    await screen.findByText("No registered host has a managed-home storage root");
  });

  it("calls Scan now and toasts the queued count with the follow-through sentence", async () => {
    mocked.forceLibraryScan.mockResolvedValue(forceScanResult({ queued: 3, eligible: 3 }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Scan now/ }));

    await waitFor(() => expect(mocked.forceLibraryScan).toHaveBeenCalledWith("tok"));
    await screen.findByText(
      "Queued 3 scans. The agent picks these up within about a minute; tiles appear once each scan reports back.",
    );
  });

  it("toasts 'no new work to do' when nothing was queued and the instance is not inert", async () => {
    mocked.forceLibraryScan.mockResolvedValue(forceScanResult({ queued: 0, eligible: 3 }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Scan now/ }));

    await screen.findByText(/No new work to do\./);
  });

  it("toasts a failure when Scan now itself fails", async () => {
    mocked.forceLibraryScan.mockRejectedValue(new ApiError(500, "internal", "could not reach the database"));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Scan now/ }));

    await screen.findByText("Could not start a scan");
  });

  it("refetches status a short while after Scan now so newly-reported rows can appear", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mocked.forceLibraryScan.mockResolvedValue(forceScanResult({ queued: 1, eligible: 1 }));
    renderPage();

    const scanBtn = await screen.findByRole("button", { name: /Scan now/ });
    fireEvent.click(scanBtn);

    await waitFor(() => expect(mocked.forceLibraryScan).toHaveBeenCalled());
    const callsAfterScan = mocked.getLibraryStatus.mock.calls.length;
    expect(callsAfterScan).toBeGreaterThanOrEqual(2);

    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ recent_scans: [recentScan({ user: "dave" })] }));

    await vi.advanceTimersByTimeAsync(6000);

    await waitFor(() => expect(mocked.getLibraryStatus.mock.calls.length).toBeGreaterThan(callsAfterScan));

    vi.useRealTimers();
  });

  it("shows the Review pending action only when there are unpublished items", async () => {
    renderPage();
    await screen.findByText("Steam");
    expect(screen.queryByText(/Review \d+ pending/)).toBeNull();
  });

  it("Review N pending navigates to the pending segment of Apps", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "steam", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [unpublished({ external_id: "1" }), unpublished({ external_id: "2" }), unpublished({ external_id: "3" })],
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Review 3 pending" }));

    await waitFor(() => expect(lastLocation).toBe("/admin/library/apps?segment=pending"));
  });

  it("shows an error line on the row, never a wrong count, when the unpublished read fails", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "steam", library_provider: "steam" })],
      next_cursor: null,
    });
    mocked.listUnpublishedLibraryItems.mockRejectedValue(new ApiError(500, "internal", "could not reach the database"));
    renderPage();

    await screen.findByText(/Could not read pending discovery items/);
    expect(screen.queryByText(/titles discovered/)).toBeNull();
    expect(screen.queryByText(/Review \d+ pending/)).toBeNull();
    // The rest of the card is unaffected by a Steam-row-scoped failure.
    expect(screen.getByText(/apps defined by hand/)).toBeInTheDocument();
  });

  it("shows a loading state on the row while the unpublished read is in flight", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "steam", library_provider: "steam" })],
      next_cursor: null,
    });
    let resolveUnpublished: (v: { items: LibraryUnpublishedItem[] }) => void = () => {};
    mocked.listUnpublishedLibraryItems.mockImplementation(
      () => new Promise((resolve) => { resolveUnpublished = resolve; }),
    );
    renderPage();

    await screen.findByText("Loading counts…");
    resolveUnpublished({ items: [] });
    await screen.findByText(/titles discovered/);
  });
});

describe("SourcesTab — scan health", () => {
  it("is collapsed by default and reveals census/recent-scans on toggle", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({
        scans: { pending: 2, claimed: 1, reported: 5, failed: 1 },
        recent_scans: [recentScan()],
      }),
    );
    renderPage();

    await screen.findByText("Steam");
    expect(screen.queryByText("Recent scans")).toBeNull();

    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("Recent scans");
    expect(screen.getByText("pending")).toBeInTheDocument();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("alice");
    expect(rows[0]).toHaveTextContent("1"); // Backfilled column

    fireEvent.click(screen.getByRole("button", { name: "Hide scan health" }));
    expect(screen.queryByText("Recent scans")).toBeNull();
  });

  it("shows recent scans newest-first, exactly as given by the server", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({
        recent_scans: [
          recentScan({ user: "alice", host: "hermes", completed_at: "2026-08-01T12:00:00Z" }),
          recentScan({ user: "bob", host: "tower", completed_at: "2026-08-01T09:00:00Z" }),
        ],
      }),
    );
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("Recent scans");
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("alice");
    expect(rows[1]).toHaveTextContent("bob");
  });

  it("shows the empty state when no scans have run yet", async () => {
    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ recent_scans: [] }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("No scans have run yet.");
  });

  it("shows a failed row with a danger chip and the error in a tooltip", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({ recent_scans: [recentScan({ state: "failed", error: "steam API timed out", user: "carol" })] }),
    );
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("Recent scans");
    const chip = screen.getAllByText("Failed").map((el) => el.closest(".chip")).find(Boolean);
    expect(chip).toHaveClass("chip-danger");
    expect(chip).toHaveAttribute("title", expect.stringContaining("steam API timed out"));
  });

  it("shows a reported row as a success chip, with no error text", async () => {
    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ recent_scans: [recentScan({ state: "reported" })] }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("Recent scans");
    const chip = screen.getAllByText("Reported").map((el) => el.closest(".chip")).find(Boolean);
    expect(chip).toHaveClass("chip-success");
  });

  it("shows the pre-migration footnote only for an all-zero REPORTED row", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({
        recent_scans: [
          recentScan({
            state: "reported",
            observed: 0, suppressed: 0, created: 0, disabled: 0,
            granted: 0, revoked: 0, rejected: 0, backfilled: 0,
          }),
        ],
      }),
    );
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText(/Scans from before this upgrade show no recorded counts\./);
  });

  it("does not show the footnote when no row is all-zero", async () => {
    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ recent_scans: [recentScan()] }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("Recent scans");
    expect(screen.queryByText(/Scans from before this upgrade/)).toBeNull();
  });

  it("does not show the footnote for an all-zero FAILED row (zero counts are normal for a failure)", async () => {
    mocked.getLibraryStatus.mockResolvedValue(
      libraryStatus({
        recent_scans: [
          recentScan({
            state: "failed", error: "boom",
            observed: 0, suppressed: 0, created: 0, disabled: 0,
            granted: 0, revoked: 0, rejected: 0, backfilled: 0,
          }),
        ],
      }),
    );
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText("Recent scans");
    expect(screen.queryByText(/Scans from before this upgrade/)).toBeNull();
  });

  it("shows the absolute last-scan timestamp beside the relative one", async () => {
    const iso = new Date(Date.now() - 21 * 60_000).toISOString();
    mocked.getLibraryStatus.mockResolvedValue(libraryStatus({ last_scan_completed_at: iso }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText(`21 minutes ago (${new Date(iso).toLocaleString()})`);
  });

  it("says no provider app is marked, when none is", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText(/No app is marked as the Steam library provider/i);
  });

  it("links to the runtime preset by name when the provider app has one", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "steam", library_provider: "steam", runtime_preset_id: "preset-1" })],
      next_cursor: null,
    });
    mocked.getRuntimePreset.mockResolvedValue({
      runtime_preset: {
        id: "preset-1",
        name: "Steam preset",
        description: "",
        image: "quasar-steam:latest",
        args: [],
        env: {},
        mounts: [],
        managed_home: true,
        home_container_path: "/home/quasar",
        used_by: [],
      },
    } as never);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    const link = await screen.findByRole("link", { name: "Steam preset" });
    expect(link).toHaveAttribute("href", "/admin/library/presets");
  });

  it("offers to extract an inline runtime spec to a preset, linking to the app editor", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [app({ id: "steam", library_provider: "steam", runtime_preset_id: null })],
      next_cursor: null,
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    await screen.findByText(/Uses an inline runtime spec, not a shared preset\./);
    expect(screen.getByRole("link", { name: "Open the app editor" })).toHaveAttribute(
      "href",
      "/admin/library/apps/steam",
    );
  });

  it("links to Settings › Libraries for scan cadence and app-details lookup", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Show scan health" }));

    const link = await screen.findByRole("link", { name: "Settings › Libraries" });
    expect(link).toHaveAttribute("href", "/admin/settings");
  });
});

describe("SourcesTab — Manual apps row", () => {
  it("counts apps with no provider and no parent, and is always on and disabled", async () => {
    mocked.listAdminApps.mockResolvedValue({
      items: [
        app({ id: "m1" }),
        app({ id: "m2" }),
        app({ id: "steam", library_provider: "steam" }),
        app({ id: "t1", parent_app_id: "steam" }),
      ],
      next_cursor: null,
    });
    renderPage();

    await screen.findByText("2 apps defined by hand");
    const toggle = screen.getByRole("checkbox", { name: "Manual apps" });
    expect(toggle).toBeChecked();
    expect(toggle).toBeDisabled();
  });

  it("Open Apps navigates to the manual source filter", async () => {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Open Apps" }));

    await waitFor(() => expect(lastLocation).toBe("/admin/library/apps?source=manual"));
  });
});

describe("SourcesTab — Artwork providers", () => {
  it("shows one not-configured chip and the plain 'API key' field for SteamGridDB", async () => {
    renderPage();

    await screen.findByText("SteamGridDB");
    expect(screen.getAllByText("not configured")).toHaveLength(1);
    expect(screen.getByLabelText("API key")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "SteamGridDB" })).not.toBeChecked();
  });

  it("shows the configured chip when the key is stored", async () => {
    mocked.listSecrets.mockResolvedValue(secretsResponse([artworkSecret({ configured: true, readable: true })]));
    renderPage();

    await screen.findByText("configured");
    expect(screen.getByRole("checkbox", { name: "SteamGridDB" })).toBeChecked();
  });

  it("saving the key calls setSecret and refreshes", async () => {
    mocked.setSecret.mockResolvedValue({ secret: artworkSecret({ configured: true }) } as never);
    renderPage();

    const input = await screen.findByLabelText("API key");
    fireEvent.change(input, { target: { value: "abc123" } });
    fireEvent.click(screen.getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(mocked.setSecret).toHaveBeenCalledWith("tok", "artwork.steamgriddb.api_key", "abc123"));
  });

  it("says no artwork provider credential is declared when the descriptor is absent", async () => {
    mocked.listSecrets.mockResolvedValue(secretsResponse([]));
    renderPage();

    await screen.findByText("This deployment declares no artwork provider credential yet.");
    expect(screen.queryByText("SteamGridDB")).toBeNull();
  });
});
