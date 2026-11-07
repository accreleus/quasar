/**
 * Library ▸ Compatibility (#1075, mock #1067 v2). What is asserted is the page's
 * own wiring: states, the grouped default select, Remove (refused, disabled),
 * and the Add tool picker. The load/poll races live in lib/resource/core.test.ts.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CompatibilityTab } from "./CompatibilityTab";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { LIBRARY_TABS } from "../../../components/shell/sectionTabs";
import { AuthContext } from "../../../auth/context";
import type { AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";
import { ApiError } from "../../../api/client";
import type { CompatTool, CompatToolAvailable, CompatToolAvailableList, CompatToolList } from "../../../api/admin";

vi.mock("../../../api/admin", () => ({
  listCompatTools: vi.fn(),
  listAvailableCompatTools: vi.fn(),
  addCompatTool: vi.fn(),
  setCompatToolEnabled: vi.fn(),
  setCompatToolDefault: vi.fn(),
}));

import * as adminApi from "../../../api/admin";

const GB = 1024 ** 3;

function counts(over: Partial<NonNullable<CompatTool["hosts"]>> = {}): NonNullable<CompatTool["hosts"]> {
  return { installed: 3, installing: 0, waiting: 0, failed: 0, unsupported: 0, ...over };
}

function tool(over: Partial<CompatTool> & { id: string; display_name: string }): CompatTool {
  const ge = over.family !== "valve";
  return {
    kind: "proton",
    family: "ge",
    source: "recommended",
    tag: ge ? over.display_name : null,
    steam_name: over.display_name,
    recommended: true,
    enabled: true,
    asset: ge
      ? { name: `${over.display_name}.tar.gz`, url: "https://example.invalid/x", size_bytes: 1.6 * GB, sha512: "ab" }
      : null,
    hosts: ge ? counts() : null,
    mappings: 0,
    is_default: false,
    name_conflict: false,
    pin_conflict: false,
    created_at: "2026-11-01T00:00:00Z",
    ...over,
  };
}

const valve = (id: string, name: string) =>
  tool({ id, display_name: name, family: "valve", source: "steam", recommended: false });

const IN_USE = tool({ id: "t-104", display_name: "GE-Proton10-4", mappings: 6 });
const INSTALLING = tool({ id: "t-927", display_name: "GE-Proton9-27", hosts: counts({ installed: 1, installing: 2 }) });
const DONE = tool({ id: "t-920", display_name: "GE-Proton9-20", mappings: 3 });

function catalogue(items: CompatTool[], defaultId: string | null = null): CompatToolList {
  return { items, default_tool_id: defaultId };
}

const VALVE = [valve("v-exp", "Proton Experimental"), valve("v-10", "Proton 10.0")];

function avail(over: Partial<CompatToolAvailable> & { tag: string }): CompatToolAvailable {
  return {
    source: "github_latest",
    display_name: over.tag,
    published_at: "2026-11-04T10:00:00Z",
    tool_id: null,
    enabled: false,
    pin_conflict: false,
    name_conflict: false,
    hosts: null,
    ...over,
  };
}

function availableList(items: CompatToolAvailable[], github = true): CompatToolAvailableList {
  return {
    items,
    github_latest: github
      ? { available: true, reason: null, fetched_at: "2026-11-07T00:00:00Z" }
      : { available: false, reason: "rate_limited", fetched_at: null },
  };
}

function renderTab() {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "test-token",
    isAdmin: true,
    sessionExpired: false,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  return render(
    <MemoryRouter initialEntries={["/admin/library/compatibility"]}>
      <AuthContext.Provider value={authValue}>
        <ToastProvider>
          <SectionHeadProvider title="Library" tabs={LIBRARY_TABS}>
            <CompatibilityTab />
          </SectionHeadProvider>
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const list = vi.mocked(adminApi.listCompatTools);
const available = vi.mocked(adminApi.listAvailableCompatTools);

beforeEach(() => {
  vi.clearAllMocks();
  list.mockResolvedValue(catalogue([...VALVE, IN_USE, INSTALLING, DONE]));
});
afterEach(cleanup);

describe("CompatibilityTab states", () => {
  it("shows a loading line before the first read", () => {
    list.mockReturnValue(new Promise(() => {}));
    renderTab();
    expect(screen.getByText(/loading/i)).toBeTruthy();
  });

  it("shows the load error", async () => {
    list.mockRejectedValue(new ApiError(500, "internal", "boom"));
    renderTab();
    expect((await screen.findByRole("alert")).textContent).toContain("boom");
  });

  it("says so when no tool is installed, and still offers Valve's builds", async () => {
    list.mockResolvedValue(catalogue(VALVE));
    renderTab();
    expect(await screen.findByText(/No tools installed/)).toBeTruthy();
    expect(screen.queryByText("Proton Experimental", { selector: "td *" })).toBeNull();
  });

  it("lists installed tools with host counts and games using each; Valve's builds are not rows", async () => {
    renderTab();
    const row = (await screen.findByText("GE-Proton10-4", { selector: ".primary" })).closest("tr")!;
    expect(within(row).getByText("3 / 3")).toBeTruthy();
    expect(within(row).getByText("1.6 GB per host")).toBeTruthy();
    expect(within(row).getByText("6")).toBeTruthy();
    expect(screen.queryByText("Proton 10.0", { selector: ".primary" })).toBeNull();
  });

  it("shows an Installing chip with progress and disables Remove while installing", async () => {
    renderTab();
    const row = (await screen.findByText("GE-Proton9-27", { selector: ".primary" })).closest("tr")!;
    expect(within(row).getByText("Installing")).toBeTruthy();
    expect(within(row).getByText("1 / 3")).toBeTruthy();
    expect((within(row).getByRole("button", { name: "Remove GE-Proton9-27" }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("CompatibilityTab hosts that are not installing", () => {
  const row = async (name: string) =>
    (await screen.findByText(name, { selector: ".primary" })).closest("tr") as HTMLElement;
  const removeBtn = (r: HTMLElement, name: string) =>
    within(r).getByRole("button", { name: `Remove ${name}` }) as HTMLButtonElement;

  it.each([
    ["an offline host", { installed: 2, waiting: 1 }, "2 / 3", "1 waiting"],
    ["an unsupported host", { installed: 2, unsupported: 1 }, "2 / 3", "1 unsupported"],
    ["a failed host", { installed: 2, failed: 1 }, "2 / 3", "1 failed"],
  ])("%s among installed ones reads installed, can be removed and can be the default", async (_n, c, count, note) => {
    const t = tool({ id: "t-x", display_name: "GE-Proton9-1", hosts: counts(c) });
    list.mockResolvedValue(catalogue([...VALVE, t]));
    renderTab();
    const r = await row("GE-Proton9-1");
    expect(within(r).getByText(count)).toBeTruthy();
    expect(within(r).getByText(new RegExp(note))).toBeTruthy();
    expect(within(r).queryByText("Installing")).toBeNull();
    expect(within(r).queryByText("Failed")).toBeNull();
    expect(removeBtn(r, "GE-Proton9-1").disabled).toBe(false);
    const select = (await screen.findByLabelText("Default tool")) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.text)).toContain("GE-Proton9-1");
  });

  it("a tool no host holds reads Failed or Waiting, still removable", async () => {
    list.mockResolvedValue(
      catalogue([
        ...VALVE,
        tool({ id: "t-f", display_name: "GE-Proton9-2", hosts: counts({ installed: 0, failed: 3 }) }),
        tool({ id: "t-w", display_name: "GE-Proton9-3", hosts: counts({ installed: 0, waiting: 3 }) }),
      ]),
    );
    renderTab();
    const f = await row("GE-Proton9-2");
    expect(within(f).getByText("Failed")).toBeTruthy();
    expect(removeBtn(f, "GE-Proton9-2").disabled).toBe(false);
    const w = await row("GE-Proton9-3");
    expect(within(w).getByText("Waiting")).toBeTruthy();
    expect(removeBtn(w, "GE-Proton9-3").disabled).toBe(false);
  });

  it("a tool in conflict is not offered as a default", async () => {
    list.mockResolvedValue(catalogue([...VALVE, tool({ id: "t-c", display_name: "GE-Proton9-4", pin_conflict: true })]));
    renderTab();
    const select = (await screen.findByLabelText("Default tool")) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.text)).not.toContain("GE-Proton9-4");
  });

  it("a disabled Remove points at the chip that says why", async () => {
    renderTab();
    const r = await row("GE-Proton9-27");
    const btn = removeBtn(r, "GE-Proton9-27");
    expect(btn.disabled).toBe(true);
    expect(document.getElementById(btn.getAttribute("aria-describedby")!)?.textContent).toContain("Installing");
  });
});

describe("CompatibilityTab default tool", () => {
  it("orders the select Steam's default, installed tools, Valve's builds; every enabled tool without a conflict is usable", async () => {
    renderTab();
    const select = (await screen.findByLabelText("Default tool")) as HTMLSelectElement;
    expect(select.value).toBe("");
    const groups = Array.from(select.children).map((c) =>
      c.tagName === "OPTION"
        ? (c as HTMLOptionElement).text
        : [(c as HTMLOptGroupElement).label, Array.from(c.children).map((o) => (o as HTMLOptionElement).text)],
    );
    expect(groups).toEqual([
      "Steam's default",
      ["Installed tools", ["GE-Proton10-4", "GE-Proton9-27", "GE-Proton9-20"]],
      ["Valve's builds", ["Proton Experimental", "Proton 10.0"]],
    ]);
  });

  it("shows the current default and sets a new one, and null for Steam's default", async () => {
    list.mockResolvedValue(catalogue([...VALVE, { ...DONE, is_default: true }], "t-920"));
    vi.mocked(adminApi.setCompatToolDefault).mockResolvedValue({ default_tool_id: "v-10" });
    renderTab();
    const select = (await screen.findByLabelText("Default tool")) as HTMLSelectElement;
    expect(select.value).toBe("t-920");
    fireEvent.change(select, { target: { value: "v-10" } });
    await waitFor(() => expect(adminApi.setCompatToolDefault).toHaveBeenCalledWith("test-token", "v-10"));
    fireEvent.change(select, { target: { value: "" } });
    await waitFor(() => expect(adminApi.setCompatToolDefault).toHaveBeenLastCalledWith("test-token", null));
  });
});

describe("CompatibilityTab Remove", () => {
  it("disables the tool on every host", async () => {
    vi.mocked(adminApi.setCompatToolEnabled).mockResolvedValue({ ...DONE, enabled: false });
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Remove GE-Proton9-20" }));
    await waitFor(() => expect(adminApi.setCompatToolEnabled).toHaveBeenCalledWith("test-token", "t-920", false));
  });

  // The real shape from the control plane: counts in the message, not game names.
  const inUse = () =>
    new ApiError(409, "compat_tool_in_use", "6 per-game mapping(s) name the tool; fleet default: false");

  it("shows a refusal as a form-error row under the table row, built from the row's own counts", async () => {
    vi.mocked(adminApi.setCompatToolEnabled).mockRejectedValue(inUse());
    renderTab();
    const row = (await screen.findByText("GE-Proton10-4", { selector: ".primary" })).closest("tr")!;
    fireEvent.click(within(row).getByRole("button", { name: "Remove GE-Proton10-4" }));
    const err = await screen.findByText("Not removed: 6 games use this tool. Change their tool first.");
    expect(err.className).toContain("form-error");
    expect(err.closest("tr")?.className).toBe("exp-row");
    expect(row.nextElementSibling).toBe(err.closest("tr"));
    expect(screen.queryByText(/per-game mapping/)).toBeNull();
    expect((within(row).getByRole("button", { name: "Remove GE-Proton10-4" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it.each([
    ["only the default holds it", { mappings: 0, is_default: true }, "Not removed: It's the default tool. Pick another default first."],
    ["both", { mappings: 1, is_default: true }, "Not removed: 1 game uses this tool. Change their tool first. It's the default tool. Pick another default first."],
  ])("refusal copy when %s", async (_n, over, text) => {
    list.mockResolvedValue(catalogue([...VALVE, { ...IN_USE, ...over }], over.is_default ? "t-104" : null));
    vi.mocked(adminApi.setCompatToolEnabled).mockRejectedValue(inUse());
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Remove GE-Proton10-4" }));
    expect(await screen.findByText(text)).toBeTruthy();
  });
});

describe("CompatibilityTab Add tool picker", () => {
  const REC_DONE = avail({ tag: "GE-Proton10-4", source: "recommended", tool_id: "t-104", enabled: true, hosts: counts() });
  const REC_INSTALLING = avail({
    tag: "GE-Proton9-27",
    source: "recommended",
    tool_id: "t-927",
    enabled: true,
    hosts: counts({ installed: 1, installing: 2 }),
  });
  const REC_OFF = avail({ tag: "GE-Proton9-30", source: "recommended", tool_id: "t-930", hosts: counts({ installed: 0 }) });
  const NEW = avail({ tag: "GE-Proton10-6" });

  async function openPicker(items: CompatToolAvailable[], github = true) {
    available.mockResolvedValue(availableList(items, github));
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: /Add tool/ }));
    return within(await screen.findByRole("dialog"));
  }

  it("does not read the available list until the picker opens", async () => {
    renderTab();
    await screen.findByText("GE-Proton10-4", { selector: ".primary" });
    expect(available).not.toHaveBeenCalled();
  });

  it("shows installed and installing chips instead of Install, and Install for the rest", async () => {
    const dialog = await openPicker([REC_DONE, REC_INSTALLING, REC_OFF, NEW]);
    const item = (name: string) => dialog.getByText(name, { selector: ".ae-item-t" }).closest(".ae-item") as HTMLElement;
    expect(within(item("GE-Proton10-4")).getByText("installed")).toBeTruthy();
    expect(within(item("GE-Proton9-27")).getByText("installing")).toBeTruthy();
    expect(within(item("GE-Proton9-30")).getByRole("button", { name: "Install" })).toBeTruthy();
    expect(within(item("GE-Proton10-6")).getByRole("button", { name: "Install" })).toBeTruthy();
    expect(within(item("GE-Proton10-4")).getByText(/1\.6 GB per host/)).toBeTruthy();
    expect(within(item("GE-Proton10-6")).getByText(/^Released /)).toBeTruthy();
  });

  it("gives a conflict as the reason and offers no Install", async () => {
    const dialog = await openPicker([
      avail({ tag: "GE-Proton9-1", source: "recommended", tool_id: "t-1", pin_conflict: true, hosts: counts({ installed: 0 }) }),
      avail({ tag: "GE-Proton9-2", tool_id: "t-2", enabled: true, name_conflict: true, hosts: counts() }),
    ]);
    const pin = dialog.getByText("GE-Proton9-1", { selector: ".ae-item-t" }).closest(".ae-item") as HTMLElement;
    expect(within(pin).getByText("pin conflict")).toBeTruthy();
    expect(within(pin).getByText(/checksum was pinned/)).toBeTruthy();
    const name = dialog.getByText("GE-Proton9-2", { selector: ".ae-item-t" }).closest(".ae-item") as HTMLElement;
    expect(within(name).getByText("name conflict")).toBeTruthy();
    expect(dialog.queryByRole("button", { name: "Install" })).toBeNull();
  });

  it("pins a release with no catalogue row, then enables it, closes the picker and refreshes the table", async () => {
    vi.mocked(adminApi.addCompatTool).mockResolvedValue(tool({ id: "t-106", display_name: "GE-Proton10-6", enabled: false }));
    vi.mocked(adminApi.setCompatToolEnabled).mockResolvedValue(tool({ id: "t-106", display_name: "GE-Proton10-6" }));
    const dialog = await openPicker([NEW]);
    list.mockClear();
    fireEvent.click(dialog.getByRole("button", { name: "Install" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(adminApi.addCompatTool).toHaveBeenCalledWith("test-token", "GE-Proton10-6");
    expect(adminApi.setCompatToolEnabled).toHaveBeenCalledWith("test-token", "t-106", true);
    await waitFor(() => expect(list).toHaveBeenCalled());
  });

  it("only enables a release that already has a row", async () => {
    vi.mocked(adminApi.setCompatToolEnabled).mockResolvedValue(tool({ id: "t-930", display_name: "GE-Proton9-30" }));
    const dialog = await openPicker([REC_OFF]);
    fireEvent.click(dialog.getByRole("button", { name: "Install" }));
    await waitFor(() => expect(adminApi.setCompatToolEnabled).toHaveBeenCalledWith("test-token", "t-930", true));
    expect(adminApi.addCompatTool).not.toHaveBeenCalled();
  });

  it("keeps the picker open and names the refusal when the release can't be verified", async () => {
    vi.mocked(adminApi.addCompatTool).mockRejectedValue(
      new ApiError(422, "compat_tool_unverifiable", "checksum_missing"),
    );
    const dialog = await openPicker([NEW]);
    fireEvent.click(dialog.getByRole("button", { name: "Install" }));
    expect(await screen.findByText("Could not install GE-Proton10-6")).toBeTruthy();
    expect(screen.getByText("checksum_missing")).toBeTruthy();
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("says when github.com could not be read and still lists the recommended tools", async () => {
    const dialog = await openPicker([REC_OFF], false);
    expect(dialog.getByText(/github\.com is limiting requests/)).toBeTruthy();
    expect(dialog.getByRole("button", { name: "Install" })).toBeTruthy();
  });
});
