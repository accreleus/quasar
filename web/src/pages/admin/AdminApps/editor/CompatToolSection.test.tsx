// #1076: a Steam game's own compatibility tool on the derived tile's Runtime tab.
// It saves through the page's one Save, beside the app PATCH, to the appid-keyed
// mapping routes (amendment 56).

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AdminApp, CompatTool } from "../../../../api/types";
import { ToastProvider } from "../../../../components/Toast";
import { AppEditorPage } from "../AppEditorPage";

vi.mock("../../../../api/admin");
vi.mock("../../../../auth/context", () => ({
  useAuth: () => ({ token: "tok" }),
}));

const mocked = vi.mocked(adminApi);

const tool = (
  id: string,
  family: "ge" | "valve",
  name: string,
  over: Partial<CompatTool> = {},
) =>
  ({
    id,
    kind: "proton",
    family,
    source: family === "ge" ? "recommended" : "steam",
    display_name: name,
    enabled: true,
    name_conflict: false,
    pin_conflict: false,
    ...over,
  }) as CompatTool;

// The catalogue lists Valve first, then GE; the select must regroup them.
const CATALOGUE = [
  tool("v-exp", "valve", "Proton Experimental"),
  tool("v-10", "valve", "Proton 10.0"),
  tool("ge-10", "ge", "GE-Proton10-4"),
  tool("ge-9", "ge", "GE-Proton9-20"),
  tool("ge-off", "ge", "GE-Proton8-1", { enabled: false }),
  tool("ge-bad", "ge", "GE-Proton7-1", { pin_conflict: true }),
];

const parent = {
  id: "steam",
  name: "Steam",
  library_provider: "steam",
  runtime_spec: {},
} as unknown as AdminApp;
const game = {
  id: "game-1",
  name: "Elden Ring",
  kind: "game",
  parent_app_id: "steam",
  external_source: "steam",
  external_id: "1245620",
  origin: "discovered",
  library_provider: "",
  runtime_spec: {},
  launchable_profile_ids: [],
} as unknown as AdminApp;

function renderEditor(id = "game-1") {
  return render(
    <ToastProvider>
      <MemoryRouter initialEntries={[`/admin/library/apps/${id}/runtime`]}>
        <Routes>
          <Route
            path="/admin/library/apps/:id/:tab"
            element={<AppEditorPage />}
          />
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
  );
}

const mapping = (over = {}) => ({
  provider: "steam" as const,
  external_id: "1245620",
  tool_id: "ge-10",
  forced: false,
  app_ids: ["game-1"],
  updated_at: "2026-11-07T00:00:00Z",
  ...over,
});

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getApp.mockImplementation(async (_t, id) => ({
    app: id === "steam" ? parent : game,
  }));
  mocked.listLaunchProfiles.mockResolvedValue({ items: [] } as never);
  mocked.listRuntimePresets.mockResolvedValue({ items: [] } as never);
  mocked.getSettings.mockResolvedValue({
    settings: { storage_provider: "local" },
  } as never);
  mocked.listAppEntitlements.mockResolvedValue({ items: [] });
  mocked.listUnpublishedLibraryItems.mockResolvedValue({ items: [] });
  mocked.listImages.mockResolvedValue({ images: [] } as never);
  mocked.listCompatTools.mockResolvedValue({
    items: CATALOGUE,
    default_tool_id: null,
  });
  mocked.listCompatToolMappings.mockResolvedValue({ items: [] });
});

const select = async () =>
  (await screen.findByLabelText("Compatibility tool")) as HTMLSelectElement;
const force = () => screen.getByLabelText("Force") as HTMLInputElement;
const save = () => screen.getByRole("button", { name: "Save changes" });

describe("game compatibility tool", () => {
  it("groups Steam's default, the installed tools, then Valve's builds", async () => {
    renderEditor();
    const sel = await select();
    await waitFor(() => expect(sel.disabled).toBe(false));
    const labels = Array.from(sel.querySelectorAll("option, optgroup")).map(
      (el) =>
        el.tagName === "OPTGROUP"
          ? `[${el.getAttribute("label")}]`
          : el.textContent,
    );
    // A disabled tool and a pin_conflict one are not choices: the server would refuse them.
    expect(labels).toEqual([
      "Steam’s default",
      "[Installed tools]",
      "GE-Proton10-4",
      "GE-Proton9-20",
      "[Valve's builds]",
      "Proton Experimental",
      "Proton 10.0",
    ]);
  });

  it("shows the saved mapping, and leaves Save off until something moves", async () => {
    mocked.listCompatToolMappings.mockResolvedValue({
      items: [
        mapping({ forced: true }),
        mapping({ external_id: "9", tool_id: "v-10" }),
      ],
    });
    renderEditor();
    const sel = await select();
    await waitFor(() => expect(sel.value).toBe("ge-10"));
    expect(force().checked).toBe(true);
    expect((save() as HTMLButtonElement).disabled).toBe(true);
  });

  it("PUTs the tool with Force through the one Save, without a PATCH of the app", async () => {
    mocked.setCompatToolMapping.mockResolvedValue(
      mapping({ tool_id: "v-10", forced: true }),
    );
    renderEditor();
    const sel = await select();
    await waitFor(() => expect(sel.disabled).toBe(false));
    expect(force().disabled).toBe(true); // nothing to force on Steam's default
    fireEvent.change(sel, { target: { value: "v-10" } });
    fireEvent.click(force());
    fireEvent.click(save());
    await waitFor(() =>
      expect(mocked.setCompatToolMapping).toHaveBeenCalledWith(
        "tok",
        "1245620",
        {
          tool_id: "v-10",
          forced: true,
        },
      ),
    );
    expect(mocked.updateApp).not.toHaveBeenCalled();
    await waitFor(() =>
      expect((save() as HTMLButtonElement).disabled).toBe(true),
    );
  });

  it("clears the mapping, and Force with it, when Steam's default is chosen", async () => {
    mocked.listCompatToolMappings.mockResolvedValue({
      items: [mapping({ forced: true })],
    });
    mocked.clearCompatToolMapping.mockResolvedValue(undefined);
    renderEditor();
    const sel = await select();
    await waitFor(() => expect(sel.value).toBe("ge-10"));
    fireEvent.change(sel, { target: { value: "" } });
    expect(force().checked).toBe(false);
    expect(force().disabled).toBe(true);
    fireEvent.click(save());
    await waitFor(() =>
      expect(mocked.clearCompatToolMapping).toHaveBeenCalledWith(
        "tok",
        "1245620",
      ),
    );
    expect(mocked.setCompatToolMapping).not.toHaveBeenCalled();
  });

  it("Discard puts the tool back", async () => {
    mocked.listCompatToolMappings.mockResolvedValue({ items: [mapping()] });
    renderEditor();
    const sel = await select();
    await waitFor(() => expect(sel.value).toBe("ge-10"));
    fireEvent.change(sel, { target: { value: "ge-9" } });
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(sel.value).toBe("ge-10");
  });

  it("keeps the choice and reports the server's refusal when the tool was disabled meanwhile", async () => {
    mocked.setCompatToolMapping.mockRejectedValue(
      new ApiError(409, "compat_tool_disabled", "That tool is not usable."),
    );
    renderEditor();
    const sel = await select();
    await waitFor(() => expect(sel.disabled).toBe(false));
    fireEvent.change(sel, { target: { value: "ge-9" } });
    fireEvent.click(save());
    await screen.findByText("That tool is not usable.");
    expect(sel.value).toBe("ge-9");
    expect((save() as HTMLButtonElement).disabled).toBe(false);
  });

  it("says so, and offers no choice, when the catalogue cannot be read", async () => {
    mocked.listCompatTools.mockRejectedValue(
      new ApiError(500, "internal", "boom"),
    );
    renderEditor();
    const sel = await select();
    await waitFor(() =>
      expect(screen.getAllByRole("alert").length).toBeGreaterThan(0),
    );
    expect(sel.disabled).toBe(true);
  });

  it("is only on a derived Steam game's Runtime tab", async () => {
    mocked.getApp.mockResolvedValue({ app: { ...game, parent_app_id: null } });
    renderEditor();
    await screen.findByLabelText("Image");
    expect(screen.queryByLabelText("Compatibility tool")).toBeNull();
    expect(mocked.listCompatTools).not.toHaveBeenCalled();
  });
});
