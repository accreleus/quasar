/**
 * ImagesTab (handoff §A.13): a table whose row actions are `.gbar` icons.
 * What is asserted here is the page's own wiring (states, filters,
 * action dispatch, policy card, sync/uninstall confirm, toast copy); the
 * load/cancel/poll races behind lib/resource are covered once in
 * lib/resource/core.test.ts.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ImagesTab } from "./ImagesTab";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { LIBRARY_TABS } from "../../../components/shell/sectionTabs";
import { AuthContext } from "../../../auth/context";
import type { AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";
import { ApiError } from "../../../api/client";
import type {
  CatalogImage,
  ImageCatalogEnvelope,
  ManifestProvenance,
  SettingsResponse,
} from "../../../api/types";

// Hosts come from the fleet poll AdminLayout mounts above this page.
vi.mock("../../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => ({ hosts: [], sessions: [] }),
}));

vi.mock("../../../api/admin", () => ({
  listImages: vi.fn(),
  syncImages: vi.fn(),
  installImage: vi.fn(),
  uninstallImage: vi.fn(),
  pinImage: vi.fn(),
  unpinImage: vi.fn(),
  updateImage: vi.fn(),
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  listHosts: vi.fn(),
}));

import * as adminApi from "../../../api/admin";

function renderImagesTab() {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "test-token",
    isAdmin: true,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  // The page publishes its head to the section container, so the test mounts
  // the container it lives in — its "Sync catalog" action renders there.
  return render(
    <MemoryRouter initialEntries={["/admin/library/images"]}>
      <AuthContext.Provider value={authValue}>
        <ToastProvider>
          <SectionHeadProvider title="Library" tabs={LIBRARY_TABS}>
            <ImagesTab />
          </SectionHeadProvider>
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const settingsEnvelope: SettingsResponse = {
  settings: {
    registration_mode: "invite_only",
    storage_provider: "local",
    library_discovery_enabled: false,
    library_discovery_interval_minutes: 360,
    library_discovery_appdetails_enabled: false,
    mic_capture_enabled: false,
    updated_by: null,
    updated_at: "2026-08-01T00:00:00Z",
    image_update_policy: "manual",
  },
};

const installedImage: CatalogImage = {
  id: "steam",
  display_name: "Steam",
  description: "Valve's Steam client",
  kind: "prebuilt",
  version: "1.2.3",
  registry_ref: "ghcr.io/accreleus/quasar-steam:1.2.3",
  installed: true,
  installed_version: "1.2.3",
  pinned: false,
  update_available: false,
  hosts: [],
};

const notInstalledImage: CatalogImage = {
  id: "moonlit",
  display_name: "Moonlit",
  description: "A sample app",
  kind: "prebuilt",
  version: "2.0.0",
  registry_ref: "ghcr.io/accreleus/quasar-moonlit:2.0.0",
  installed: false,
  installed_version: null,
  pinned: false,
  update_available: false,
  hosts: [],
};

const templateImage: CatalogImage = {
  id: "custom-template",
  display_name: "Custom Template",
  description: "Builds locally in a later phase",
  kind: "template",
  version: "0.1.0",
  installed: false,
  installed_version: null,
  pinned: false,
  update_available: false,
  hosts: [],
};

const baseProvenance: ManifestProvenance = {
  sha256: "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111",
  previous_sha256: null,
  commit_sha: "cccccccccccc22222222222222222222222222222",
  ref: "stable",
  url: "https://raw.githubusercontent.com/accreleus/quasar-images/stable/quasar-manifest.json",
  changed: false,
  changed_at: "2026-08-01T00:00:00Z",
};

const baseCatalog: ImageCatalogEnvelope = {
  manifest_version: 1,
  catalog_ref: "main",
  fetched_at: "2026-08-01T00:00:00Z",
  sync_error: null,
  manifest_provenance: baseProvenance,
  images: [installedImage],
};

beforeEach(() => {
  vi.mocked(adminApi.getSettings).mockResolvedValue(settingsEnvelope);
});

// RTL does not auto-cleanup under this project's vitest setup, and a
// component left mounted between tests keeps its poll alive into the next
// test's mocks.
afterEach(() => {
  cleanup();
});

describe("ImagesTab — P1 read/sync", () => {
  it("lists catalog images with their version state", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    renderImagesTab();

    await waitFor(() => {
      expect(screen.getByText("Steam")).toBeInTheDocument();
    });
    expect(screen.getByText("up to date")).toBeInTheDocument();
  });

  it("renders sync_error prominently when the cached catalog's last sync failed", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      sync_error: "manifest fetch timed out",
    });
    renderImagesTab();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("manifest fetch timed out");
    });
    // The stale-but-cached catalog is still shown, not hidden behind the error.
    expect(screen.getByText("Steam")).toBeInTheDocument();
  });

  it("Sync catalog calls syncImages and applies the refreshed envelope", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    vi.mocked(adminApi.syncImages).mockResolvedValue({
      ...baseCatalog,
      sync_error: "registry unreachable",
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    fireEvent.click(screen.getByRole("button", { name: /sync catalog/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("registry unreachable");
    });
    expect(adminApi.syncImages).toHaveBeenCalledWith("test-token");
  });

  // #548 manifest provenance.

  it("shows the manifest digest and ref for the served catalog", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    // Short form on screen, full digest in title for copy-out.
    const digest = screen.getByTitle(baseProvenance.sha256);
    expect(digest).toHaveTextContent("aaaaaaaaaaaa");
    expect(screen.getByText(baseProvenance.url)).toBeInTheDocument();
    expect(screen.getByTitle(baseProvenance.commit_sha!)).toBeInTheDocument();
  });

  it("does not raise the changed alert when the digest held steady", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    // sync_error is null here too, so ANY alert on the page would be the
    // provenance banner crying wolf on an unchanged sync.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("flags a changed manifest digest loudly, with the digest it replaced", async () => {
    const previous = "bbbbbbbbbbbb3333333333333333333333333333333333333333333333333333";
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      manifest_provenance: { ...baseProvenance, changed: true, previous_sha256: previous },
    });
    renderImagesTab();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("The manifest changed at the last sync.");
    });
    const alert = screen.getByRole("alert");
    expect(within(alert).getByTitle(previous)).toHaveTextContent("bbbbbbbbbbbb");
    expect(within(alert).getByTitle(baseProvenance.sha256)).toHaveTextContent("aaaaaaaaaaaa");
    // The catalog stays visible: this is a warning to check, not a block.
    expect(screen.getByText("Steam")).toBeInTheDocument();
  });

  it("renders nothing provenance-shaped before the first successful sync", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      manifest_provenance: null,
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    expect(screen.queryByText(/^Manifest /)).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});

describe("ImagesTab — P3 install/uninstall/pin/update actions", () => {
  it("installs a not-installed image eagerly (the gbar has no lazy control)", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ ...baseCatalog, images: [notInstalledImage] });
    vi.mocked(adminApi.installImage).mockResolvedValue({ ...notInstalledImage, installed: true });
    renderImagesTab();

    await waitFor(() => screen.getByText("Moonlit"));
    fireEvent.click(screen.getByRole("button", { name: "Install on every host" }));

    await waitFor(() => {
      expect(adminApi.installImage).toHaveBeenCalledWith("test-token", "moonlit", { lazy: false });
    });
  });

  it("mentions library discovery in Settings when installing a provider-backed image", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [{ ...notInstalledImage, library_provider: "steam" }],
    });
    vi.mocked(adminApi.installImage).mockResolvedValue({
      ...notInstalledImage,
      installed: true,
      library_provider: "steam",
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Moonlit"));
    fireEvent.click(screen.getByRole("button", { name: "Install on every host" }));

    await waitFor(() => {
      expect(
        screen.getByText(/Enable steam library discovery in Settings for it to appear in the library\./),
      ).toBeInTheDocument();
    });
  });

  it("shows a re-sync hint on a 409 digest_unresolved install error, as a toast", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ ...baseCatalog, images: [notInstalledImage] });
    vi.mocked(adminApi.installImage).mockRejectedValue(
      new ApiError(409, "digest_unresolved", "digest unresolved for this image"),
    );
    renderImagesTab();

    await waitFor(() => screen.getByText("Moonlit"));
    fireEvent.click(screen.getByRole("button", { name: "Install on every host" }));

    await waitFor(() => {
      expect(screen.getByText(/digest unresolved for this image/)).toBeInTheDocument();
    });
    expect(screen.getByText(/try syncing the catalog again/i)).toBeInTheDocument();
  });

  it("a template-kind row's install/re-ensure buttons are dimmed and disabled, not wired to a guaranteed 409", async () => {
    vi.mocked(adminApi.installImage).mockClear();
    vi.mocked(adminApi.listImages).mockResolvedValue({ ...baseCatalog, images: [templateImage] });
    renderImagesTab();

    await waitFor(() => screen.getByText("Custom Template"));
    const install = screen.getByRole("button", { name: "Install on every host" });
    expect(install).toBeDisabled();
    expect(install).toHaveAttribute("title", expect.stringContaining("Not installable yet"));
    fireEvent.click(install);
    expect(adminApi.installImage).not.toHaveBeenCalled();
  });

  it("uninstall requires confirmation via the modal before calling the API", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    vi.mocked(adminApi.uninstallImage).mockResolvedValue(undefined);
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    fireEvent.click(screen.getByRole("button", { name: "Uninstall everywhere" }));

    const dialog = await screen.findByRole("dialog");
    expect(adminApi.uninstallImage).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: /^uninstall$/i }));

    await waitFor(() => {
      expect(adminApi.uninstallImage).toHaveBeenCalledWith("test-token", "steam");
    });
  });

  it("surfaces a 409 provider_enabled uninstall refusal as a toast (#471)", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [{ ...installedImage, library_provider: "steam" }],
    });
    vi.mocked(adminApi.uninstallImage).mockRejectedValue(
      new ApiError(
        409,
        "provider_enabled",
        "Steam library discovery is enabled; disable it in Settings first, or the image will be reinstalled automatically.",
      ),
    );
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    fireEvent.click(screen.getByRole("button", { name: "Uninstall everywhere" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: /^uninstall$/i }));

    await waitFor(() => {
      expect(
        screen.getByText(/Steam library discovery is enabled; disable it in Settings first/),
      ).toBeInTheDocument();
    });
  });

  it("mentions the pinned-digest reinstall behaviour in the uninstall confirm dialog", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    fireEvent.click(screen.getByRole("button", { name: "Uninstall everywhere" }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/re-fetches whatever digest the catalog currently has pinned/i)).toBeInTheDocument();
  });

  it("toggles pin on and off via the pin gbtn", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    vi.mocked(adminApi.pinImage).mockResolvedValue(undefined);
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    fireEvent.click(screen.getByRole("button", { name: /^Pin to/ }));

    await waitFor(() => {
      expect(adminApi.pinImage).toHaveBeenCalledWith("test-token", "steam");
    });
  });

  it("shows Update to {version} as .todo when update_available, and calls updateImage", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [{ ...installedImage, update_available: true }],
    });
    vi.mocked(adminApi.updateImage).mockResolvedValue({
      applied: true,
      image: { ...installedImage, update_available: false, installed_version: "1.3.0" },
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    const updateBtn = screen.getByRole("button", { name: /Update to 1.2.3/i });
    expect(updateBtn.className).toContain("todo");
    fireEvent.click(updateBtn);

    await waitFor(() => {
      expect(adminApi.updateImage).toHaveBeenCalledWith("test-token", "steam");
    });
  });

  it("a pinned image never offers Update to X — the refresh gbtn falls back to Re-ensure (409 image is pinned)", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [{ ...installedImage, update_available: true, pinned: true }],
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    expect(screen.queryByRole("button", { name: /Update to/ })).not.toBeInTheDocument();
    const reEnsure = screen.getByRole("button", { name: "Re-ensure on every host" });
    expect(reEnsure.className).not.toContain("todo");
  });

  it("a background pull in flight offers Uninstall everywhere, not Install (which would 409)", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [
        {
          ...notInstalledImage,
          hosts: [{ host_id: "h1", node_name: "quasar-node-1", state: "pulling", version: null, error: null }],
        },
      ],
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Moonlit"));
    expect(screen.getByRole("button", { name: "Uninstall everywhere" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Install on every host" })).not.toBeInTheDocument();
  });

  it("maps a 409 already_installed error to plain language and immediately refetches", async () => {
    // Asserts an exact call count, which only makes sense against this
    // test's own calls — reset what earlier tests in the file accumulated.
    vi.mocked(adminApi.listImages).mockReset();
    vi.mocked(adminApi.listImages)
      .mockResolvedValueOnce({ ...baseCatalog, images: [notInstalledImage] })
      .mockResolvedValueOnce({ ...baseCatalog, images: [installedImage] });
    vi.mocked(adminApi.installImage).mockRejectedValue(
      new ApiError(
        409,
        "already_installed",
        "image is already installed; use POST /v1/admin/images/{id}/update to move it to a newer version",
      ),
    );
    renderImagesTab();

    await waitFor(() => screen.getByText("Moonlit"));
    fireEvent.click(screen.getByRole("button", { name: "Install on every host" }));

    await waitFor(() => {
      expect(screen.getByText(/was just installed/i)).toBeInTheDocument();
    });
    // The raw endpoint-speak body must never reach the DOM verbatim.
    expect(screen.queryByText(/use POST \/v1\/admin\/images/)).not.toBeInTheDocument();

    await waitFor(() => {
      expect(adminApi.listImages).toHaveBeenCalledTimes(2);
    });
  });
});

describe("ImagesTab — filters", () => {
  it("filters by name/ref via search, and by the All/Installed/Updates segments", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [installedImage, notInstalledImage, { ...installedImage, id: "proton", display_name: "Proton", update_available: true }],
    });
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    expect(screen.getByText("Moonlit")).toBeInTheDocument();
    expect(screen.getByText("Proton")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: /^Updates/ }));
    expect(screen.queryByText("Steam")).toBeNull();
    expect(screen.getByText("Proton")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: /^All/ }));
    fireEvent.change(screen.getByPlaceholderText("Filter catalog"), { target: { value: "moonlit" } });
    expect(screen.getByText("Moonlit")).toBeInTheDocument();
    expect(screen.queryByText("Steam")).toBeNull();
  });
});

describe("ImagesTab — instance update-policy card", () => {
  it("surfaces a settings-load failure with a retry, instead of hiding the selector", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    vi.mocked(adminApi.getSettings).mockReset();
    vi.mocked(adminApi.getSettings)
      .mockRejectedValueOnce(new ApiError(500, "internal_error", "settings unavailable"))
      .mockResolvedValueOnce(settingsEnvelope);
    renderImagesTab();

    await waitFor(() => screen.getByText("Steam"));
    await waitFor(() => {
      expect(screen.getByText(/settings unavailable/i)).toBeInTheDocument();
    });
    expect(screen.queryByRole("tablist", { name: /image update policy/i })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /retry/i }));

    await waitFor(() => {
      expect(screen.getByRole("tablist", { name: /image update policy/i })).toBeInTheDocument();
    });
    expect(screen.queryByText(/settings unavailable/i)).not.toBeInTheDocument();
  });

  it("loads the current policy and PATCHes on change", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue(baseCatalog);
    vi.mocked(adminApi.updateSettings).mockResolvedValue({
      settings: { ...settingsEnvelope.settings, image_update_policy: "auto" },
    });
    renderImagesTab();

    await waitFor(() => {
      expect(screen.getByRole("tablist", { name: /image update policy/i })).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("tab", { name: "Auto" }));

    await waitFor(() => {
      expect(adminApi.updateSettings).toHaveBeenCalledWith("test-token", { image_update_policy: "auto" });
    });
  });

  it("shows the catalog stats with the update count in warning colour", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      ...baseCatalog,
      images: [installedImage, { ...installedImage, id: "p2", update_available: true }],
    });
    renderImagesTab();

    await waitFor(() => screen.getByText(/2 images · 2 installed/));
    expect(screen.getByText(/1 update available/)).toBeInTheDocument();
  });
});
