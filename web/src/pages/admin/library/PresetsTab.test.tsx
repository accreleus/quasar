// PresetsTab (v3 handoff §A.11/§A.12): a table plus a drawer.
// What is asserted here is the page's own wiring (states,
// filters, delete confirm, toast copy, cache write after a save); the
// load/cancel/poll races behind it are covered once in lib/resource/core.test.ts.

import { MemoryRouter, useLocation } from "react-router-dom";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { LIBRARY_TABS } from "../../../components/shell/sectionTabs";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act, within, fireEvent } from "@testing-library/react";
import { PresetsTab } from "./PresetsTab";
import { ToastProvider } from "../../../components/Toast";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { RuntimePreset } from "../../../api/types";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");
// The drawer has its own suite; here it only needs to report which preset it
// was opened on and emit onSaved/onClose.
vi.mock("./RuntimePresetDrawer", () => ({
  RuntimePresetDrawer: ({
    preset: open,
    onSaved,
    onClose,
  }: {
    preset: RuntimePreset | null;
    onSaved: (p: RuntimePreset) => void;
    onClose: () => void;
  }) => (
    <div data-testid="drawer">
      <span data-testid="drawer-on">{open ? open.name : "new"}</span>
      <button data-testid="drawer-save" onClick={() => onSaved(preset({ id: "p1", name: "Renamed" }))}>
        save
      </button>
      <button data-testid="drawer-close" onClick={onClose}>
        close
      </button>
    </div>
  ),
}));

const mocked = vi.mocked(adminApi);

function preset(over: Partial<RuntimePreset> = {}): RuntimePreset {
  return {
    id: "p1",
    name: "Steam runtime",
    image: "quasar-steam:latest",
    env: {},
    mounts: [],
    managed_home: false,
    home_container_path: "",
    used_by: [],
    ...over,
  } as RuntimePreset;
}

/** Prints the live query string, so the `?preset=` round-trip is asserted on
 *  the router rather than on a spy. */
function SearchProbe() {
  return <span data-testid="search">{useLocation().search}</span>;
}

function renderPage(entry = "/admin/library/presets") {
  // The page publishes its head to the section container, so the test mounts
  // the container it lives in — its "New preset" action renders there.
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <ToastProvider>
        <SectionHeadProvider title="Library" tabs={LIBRARY_TABS}>
          <PresetsTab />
          <SearchProbe />
        </SectionHeadProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function openRowMenu(name: string) {
  act(() => {
    screen.getByRole("button", { name: `Actions for ${name}` }).click();
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.listRuntimePresets.mockResolvedValue({ items: [preset()] });
});

describe("PresetsTab", () => {
  it("announces loading, then renders the presets", async () => {
    renderPage();
    expect(screen.getByRole("status")).toHaveTextContent("Loading…");

    await act(async () => {});
    expect(screen.getByText("Steam runtime")).toBeInTheDocument();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("shows the empty state only once the load settles", async () => {
    mocked.listRuntimePresets.mockResolvedValue({ items: [] });
    renderPage();
    expect(screen.queryByText(/No runtime presets yet/)).toBeNull(); // still loading

    await act(async () => {});
    expect(screen.getByText(/No runtime presets yet/)).toBeInTheDocument();
  });

  it("reports a load failure as an alert and not as an empty state", async () => {
    mocked.listRuntimePresets.mockRejectedValue(new ApiError(502, "internal", "gateway"));
    renderPage();
    await act(async () => {});

    expect(screen.getByRole("alert")).toHaveTextContent("gateway");
    expect(screen.queryByText(/No runtime presets yet/)).toBeNull();
  });

  it("renders environment key count, mount count and the used-by link", async () => {
    mocked.listRuntimePresets.mockResolvedValue({
      items: [preset({ env: { A: "1", B: "2" }, mounts: ["/a:/b"], used_by: [{ id: "a1", name: "Cyberpunk" }] })],
    });
    renderPage();
    await act(async () => {});

    expect(screen.getByText("2 keys")).toBeInTheDocument();
    expect(screen.getByText("1", { selector: "td" })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "1 app" });
    expect(link).toHaveAttribute("href", "/admin/library/apps?preset=p1");
  });

  it("filters by the In use / Unused segments", async () => {
    mocked.listRuntimePresets.mockResolvedValue({
      items: [
        preset({ id: "p1", name: "Used preset", used_by: [{ id: "a1", name: "App" }] }),
        preset({ id: "p2", name: "Unused preset", used_by: [] }),
      ],
    });
    renderPage();
    await act(async () => {});

    expect(screen.getByText("Used preset")).toBeInTheDocument();
    expect(screen.getByText("Unused preset")).toBeInTheDocument();

    act(() => {
      screen.getByRole("tab", { name: /Unused/ }).click();
    });
    expect(screen.queryByText("Used preset")).toBeNull();
    expect(screen.getByText("Unused preset")).toBeInTheDocument();

    act(() => {
      screen.getByRole("tab", { name: /^In use/ }).click();
    });
    expect(screen.getByText("Used preset")).toBeInTheDocument();
    expect(screen.queryByText("Unused preset")).toBeNull();
  });

  it("filters by name via the search box", async () => {
    mocked.listRuntimePresets.mockResolvedValue({
      items: [preset({ id: "p1", name: "Proton GPU" }), preset({ id: "p2", name: "Native Linux" })],
    });
    renderPage();
    await act(async () => {});

    fireEvent.change(screen.getByPlaceholderText("Filter presets"), { target: { value: "proton" } });
    expect(screen.getByText("Proton GPU")).toBeInTheDocument();
    expect(screen.queryByText("Native Linux")).toBeNull();
  });

  it("deletes through the row menu's confirm modal, drops the row and toasts", async () => {
    mocked.deleteRuntimePreset.mockResolvedValue(undefined);
    renderPage();
    await act(async () => {});

    openRowMenu("Steam runtime");
    act(() => {
      screen.getByRole("menuitem", { name: "Delete preset" }).click();
    });
    const modal = screen.getByRole("dialog");
    await act(async () => {
      within(modal).getByRole("button", { name: "Delete preset" }).click();
    });

    expect(mocked.deleteRuntimePreset).toHaveBeenCalledWith("tok", "p1");
    expect(screen.queryByText("Steam runtime")).toBeNull();
    expect(screen.getByText('"Steam runtime" deleted')).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("disables the row menu's Delete preset item while the preset is in use", async () => {
    mocked.listRuntimePresets.mockResolvedValue({
      items: [preset({ used_by: [{ id: "a1", name: "App" }] })],
    });
    renderPage();
    await act(async () => {});

    openRowMenu("Steam runtime");
    const item = screen.getByRole("menuitem", { name: "Delete preset" });
    expect(item).toBeDisabled();
    expect(item).toHaveAttribute("title", "In use. Remove it from every app first.");
  });

  it("keeps the row and shows the server's message when delete is refused", async () => {
    mocked.deleteRuntimePreset.mockRejectedValue(
      new ApiError(409, "preset_in_use", "preset is still used by 2 apps"),
    );
    renderPage();
    await act(async () => {});

    openRowMenu("Steam runtime");
    act(() => {
      screen.getByRole("menuitem", { name: "Delete preset" }).click();
    });
    await act(async () => {
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete preset" }).click();
    });

    expect(screen.getByText("preset is still used by 2 apps")).toBeInTheDocument();
    // The row survives (the modal repeats the name too — match the table cell).
    expect(screen.getByText("Steam runtime", { selector: "td span" })).toBeInTheDocument();
  });

  it("opens the drawer on row click and writes a saved preset into the list without refetching", async () => {
    renderPage();
    await act(async () => {});

    act(() => {
      screen.getByText("Steam runtime").click();
    });
    await act(async () => {
      screen.getByTestId("drawer-save").click();
    });

    expect(screen.getByText("Renamed")).toBeInTheDocument();
    expect(mocked.listRuntimePresets).toHaveBeenCalledTimes(1); // no reload
  });

  it("opens an empty drawer from the New preset action", async () => {
    renderPage();
    await act(async () => {});
    await act(async () => {
      screen.getByRole("button", { name: /New preset/ }).click();
    });

    await act(async () => {
      screen.getByTestId("drawer-save").click();
    });

    expect(screen.getByText("Renamed")).toBeInTheDocument();
  });

  // The app editor's rail and its Runtime tab both link to
  // /admin/library/presets?preset=<id>.
  describe("?preset= deep link", () => {
    it("opens that preset's drawer, and closing clears the param", async () => {
      mocked.listRuntimePresets.mockResolvedValue({
        items: [preset(), preset({ id: "p2", name: "Proton runtime" })],
      } as never);
      renderPage("/admin/library/presets?preset=p2");
      await act(async () => {});

      expect(screen.getByTestId("drawer-on")).toHaveTextContent("Proton runtime");

      await act(async () => {
        screen.getByTestId("drawer-close").click();
      });
      expect(screen.queryByTestId("drawer")).toBeNull();
      expect(screen.getByTestId("search")).toHaveTextContent("");
    });

    it("opens nothing for an id that is not in the list", async () => {
      renderPage("/admin/library/presets?preset=gone");
      await act(async () => {});
      expect(screen.queryByTestId("drawer")).toBeNull();
    });
  });
});
