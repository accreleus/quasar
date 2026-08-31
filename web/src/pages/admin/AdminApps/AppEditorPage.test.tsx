// The v3 app editor (handoff §A.10). The property under test is the one the
// tabs exist to hide: six panels, one draft, one save. An edit made on Identity
// and an edit made on Runtime must leave in a single PATCH of only the keys
// that moved, and Discard must put every tab back at once.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import type { AdminApp } from "../../../api/types";
import { ToastProvider } from "../../../components/Toast";
import { AppEditorPage } from "./AppEditorPage";

vi.mock("../../../api/admin");
vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));

const mocked = vi.mocked(adminApi);

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "app-1",
    name: "Cyberpunk 2077",
    description: "Open-world RPG",
    cover_url: null,
    hero_url: null,
    kind: "game",
    parent_app_id: null,
    external_source: "steam",
    external_id: "1091500",
    origin: "discovered",
    library_provider: "",
    library_discovery_suspended: false,
    favourite: false,
    sessions_30d: 214,
    default_width: 1920,
    default_height: 1080,
    default_fps: 60,
    default_bitrate_kbps: 8000,
    default_profile_id: "lp-1",
    profile_policy: "prefer",
    enabled: true,
    default_vram_mb: 0,
    default_encode_slots: 1,
    runtime_spec: { image: "ghcr.io/quasar/proton:9.0", args: [], env: {}, mounts: [], gpu: true },
    managed_home: true,
    home_container_path: "/home/quasar",
    runtime_preset_id: "preset-1",
    launchable_profile_ids: [],
    ...over,
  } as AdminApp;
}

function renderEditor(path = "/admin/library/apps/app-1") {
  return render(
    <ToastProvider>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/admin/library/apps/:id" element={<AppEditorPage />} />
          <Route path="/admin/library/apps/:id/:tab" element={<AppEditorPage />} />
          <Route path="/admin/library/apps" element={<p>Apps list</p>} />
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getApp.mockResolvedValue({ app: app() });
  mocked.listLaunchProfiles.mockResolvedValue({
    items: [
      {
        id: "lp-1",
        display_name: "Adaptive 1440p",
        description: "",
        visibility: "user",
        sort_order: 1,
        rungs: [
          {
            position: 1,
            stream_profile: {
              id: "sp-1",
              display_name: "HEVC 1440p60",
              codec: "hevc",
              width: 2560,
              height: 1440,
              fps: 60,
              nominal_bitrate_kbps: 24000,
            },
          },
        ],
      },
    ],
  } as never);
  mocked.listRuntimePresets.mockResolvedValue({
    items: [{ id: "preset-1", name: "Proton GPU", env: {} }],
  } as never);
  mocked.getSettings.mockResolvedValue({ settings: { storage_provider: "local" } } as never);
  mocked.listAppEntitlements.mockResolvedValue({ items: [] });
  mocked.listUnpublishedLibraryItems.mockResolvedValue({ items: [] });
  mocked.listUsers.mockResolvedValue({ items: [], next_cursor: null });
  mocked.getAppArtwork.mockResolvedValue({
    artwork: null,
    provider_configured: false,
    provider_name: "",
    provider_origin: "none",
  } as never);
  mocked.listImages.mockResolvedValue({
    images: [
      {
        id: "img-1",
        display_name: "Proton",
        kind: "prebuilt",
        version: "9.0",
        registry_ref: "ghcr.io/quasar/proton:9.0",
        installed: true,
        hosts: [
          { host_id: "h1", state: "ready" },
          { host_id: "h2", state: "ready" },
          { host_id: "h3", state: "absent" },
        ],
      },
    ],
  } as never);
});

describe("app editor — head and tabs", () => {
  it("heads the page with the app, its kind, source and 30-day sessions", async () => {
    renderEditor();
    await screen.findByRole("heading", { name: "Cyberpunk 2077" });
    expect(screen.getByText("Game · Steam · 214 sessions in the last 30 days")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Library" }).getAttribute("href")).toBe(
      "/admin/library/apps",
    );
  });

  it("retitles the page from the draft, before the rename is saved", async () => {
    renderEditor();
    fireEvent.change(await screen.findByLabelText("Display name"), {
      target: { value: "Cyberpunk" },
    });
    expect(screen.getByRole("heading", { name: "Cyberpunk" })).toBeTruthy();
  });

  it("selects the tab in the URL and renders only that panel", async () => {
    renderEditor("/admin/library/apps/app-1/runtime");
    const runtime = await screen.findByRole("tab", { name: "Runtime" });
    expect(runtime.getAttribute("aria-selected")).toBe("true");
    await screen.findByLabelText("Image");
    expect(screen.queryByLabelText("Display name")).toBeNull();
  });

  it("defaults to Identity, and offers Library only on a provider app", async () => {
    renderEditor();
    expect((await screen.findByRole("tab", { name: "Identity" })).getAttribute("aria-selected")).toBe(
      "true",
    );
    expect(screen.queryByRole("tab", { name: /Library/ })).toBeNull();
  });

  it("counts the suppressed appids on a provider app's Library tab", async () => {
    mocked.getApp.mockResolvedValue({ app: app({ library_provider: "steam", kind: "launcher" }) });
    mocked.listUnpublishedLibraryItems.mockResolvedValue({
      items: [
        {
          external_source: "steam",
          external_id: "228980",
          name: "Steamworks Common Redistributables",
          suppressed_by: "builtin_prefix",
          users: 3,
          last_seen_at: "2026-08-01T00:00:00Z",
          has_tile: false,
        },
      ],
    });
    renderEditor();
    const tab = await screen.findByRole("tab", { name: /Library/ });
    expect(tab.textContent).toContain("1");
  });
});

describe("app editor — one save across the tabs", () => {
  it("keeps Save and Discard disabled until something changes", async () => {
    renderEditor();
    const save = (await screen.findByRole("button", { name: "Save changes" })) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Cyberpunk" } });
    expect(save.disabled).toBe(false);
  });

  it("sends one PATCH carrying an Identity edit and a Runtime edit together", async () => {
    mocked.updateApp.mockResolvedValue({ app: app({ name: "Cyberpunk" }) });
    renderEditor();

    fireEvent.change(await screen.findByLabelText("Display name"), {
      target: { value: "Cyberpunk" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "Runtime" }));
    fireEvent.change(await screen.findByLabelText("Image"), {
      target: { value: "ghcr.io/quasar/proton:9.1" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mocked.updateApp).toHaveBeenCalledTimes(1));
    expect(mocked.updateApp).toHaveBeenCalledWith("tok", "app-1", {
      name: "Cyberpunk",
      runtime_spec: {
        image: "ghcr.io/quasar/proton:9.1",
        args: [],
        env: {},
        mounts: [],
        gpu: true,
      },
    });
  });

  it("puts every tab back on Discard, and re-disables the save", async () => {
    renderEditor();
    fireEvent.change(await screen.findByLabelText("Display name"), { target: { value: "Nope" } });
    fireEvent.click(screen.getByRole("tab", { name: "Runtime" }));
    fireEvent.change(await screen.findByLabelText("Image"), { target: { value: "wrong:1" } });

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));

    expect((screen.getByLabelText("Image") as HTMLInputElement).value).toBe(
      "ghcr.io/quasar/proton:9.0",
    );
    fireEvent.click(screen.getByRole("tab", { name: "Identity" }));
    expect((await screen.findByLabelText("Display name") as HTMLInputElement).value).toBe(
      "Cyberpunk 2077",
    );
    expect((screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(mocked.updateApp).not.toHaveBeenCalled();
  });

  // One save over six tabs: a rejected field can be on a tab that is not open,
  // so the page must say which one rather than failing silently.
  it("names the tab holding an invalid field instead of sending the PATCH", async () => {
    renderEditor();
    fireEvent.change(await screen.findByLabelText("Display name"), { target: { value: "  " } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await screen.findByText("Fix the highlighted fields on the Identity tab.");
    expect(mocked.updateApp).not.toHaveBeenCalled();
  });

  it("creates a new app from the same draft, then routes to its editor", async () => {
    mocked.createApp.mockResolvedValue({ app: app({ id: "app-2", name: "Blender" }) });
    renderEditor("/admin/library/apps/new");

    await screen.findByRole("heading", { name: "New app" });
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Blender" } });
    fireEvent.click(screen.getByRole("tab", { name: "Runtime" }));
    fireEvent.change(await screen.findByLabelText("Image"), {
      target: { value: "ghcr.io/quasar/blender:4.2" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mocked.createApp).toHaveBeenCalledTimes(1));
    expect(mocked.createApp.mock.calls[0][1]).toMatchObject({
      name: "Blender",
      runtime_spec: expect.objectContaining({ image: "ghcr.io/quasar/blender:4.2" }),
      launchable_profile_ids: [],
    });
    // The route moved to the app it just created, which is what loads it.
    await waitFor(() => expect(mocked.getApp).toHaveBeenCalledWith("tok", "app-2"));
  });
});

describe("app editor — the rail", () => {
  it("states the facts about this app, and links the hosts holding its image", async () => {
    renderEditor();
    await screen.findByText("Sessions · 30d");
    expect(screen.getByText("214")).toBeTruthy();
    expect(screen.getByRole("link", { name: "2 / 3 hosts" }).getAttribute("href")).toBe(
      "/admin/library/images",
    );
    expect(screen.getByRole("link", { name: "Proton GPU" }).getAttribute("href")).toBe(
      "/admin/library/presets?preset=preset-1",
    );
    expect(screen.getByText("Launch profile").nextElementSibling?.textContent).toBe("Adaptive 1440p");
    expect(screen.getByText("Source").nextElementSibling?.textContent).toBe("Steam");
  });

  it("omits the image fact when no catalog image is this app's", async () => {
    mocked.listImages.mockResolvedValue({ images: [] } as never);
    renderEditor();
    await screen.findByText("Sessions · 30d");
    expect(screen.queryByText("Image present on")).toBeNull();
  });

  // Enabled is not part of the draft: hiding a misbehaving app must not wait
  // on a save, and must not carry unrelated edits with it.
  it("writes Enabled through immediately, without the draft", async () => {
    mocked.updateApp.mockResolvedValue({ app: app({ enabled: false }) });
    renderEditor();
    fireEvent.change(await screen.findByLabelText("Display name"), { target: { value: "Edited" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "Enabled for users" }));

    await waitFor(() =>
      expect(mocked.updateApp).toHaveBeenCalledWith("tok", "app-1", { enabled: false }),
    );
  });

  it("confirms before deleting, then returns to the list", async () => {
    mocked.deleteApp.mockResolvedValue(undefined);
    renderEditor();
    fireEvent.click(await screen.findByRole("button", { name: "Delete app" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Delete app" })[1]);

    await waitFor(() => expect(mocked.deleteApp).toHaveBeenCalledWith("tok", "app-1"));
    await screen.findByText("Apps list");
  });
});
