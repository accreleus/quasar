import { describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { App } from "../../../api/types";
import { afterPaletteRun, beforePaletteRun } from "../../../components/shell/paletteGames";
import { requestOpen, useOpenRequests } from "./useFuzzy";

const apps = [{ id: "p", name: "Portal 2" }] as App[];

describe("open requests", () => {
  it("a palette game row opens that game once it is in the list on screen", () => {
    const reveal = vi.fn();
    beforePaletteRun({ id: "app-p", label: "Portal 2", icon: "library", to: "/app/library?q=Portal" }, false);
    const { rerender } = renderHook(({ list }) => useOpenRequests(list, reveal, "Portal"), { initialProps: { list: [] as App[] } });
    expect(reveal).not.toHaveBeenCalled();
    rerender({ list: apps });
    expect(reveal).toHaveBeenCalledWith(apps[0], false);
  });

  it("an admin's palette row asks for nothing", () => {
    const reveal = vi.fn();
    beforePaletteRun({ id: "app-p", label: "Portal 2", icon: "library", to: "/admin/library/apps/p" }, true);
    renderHook(() => useOpenRequests(apps, reveal, ""));
    expect(reveal).not.toHaveBeenCalled();
  });

  it("the popover's request opens at once", () => {
    const reveal = vi.fn();
    const { rerender } = renderHook(() => useOpenRequests(apps, reveal, ""));
    requestOpen("p");
    rerender();
    expect(reveal).toHaveBeenCalledWith(apps[0], false);
  });

  it("a popover pick opens although the URL still carries an older ?q=", () => {
    // Arrived by a palette link, then retyped the box: nothing clears the URL.
    window.history.pushState({}, "", "/app/library?q=Portal");
    const reveal = vi.fn();
    const halo = [{ id: "h", name: "Halo" }] as App[];
    renderHook(() => useOpenRequests(halo, reveal, "halo"));
    act(() => requestOpen("h"));
    expect(reveal).toHaveBeenCalledWith(halo[0], false);
    window.history.pushState({}, "", "/");
  });

  it("a palette pick waits for the box to hold the search its link seeds, then opens", () => {
    const reveal = vi.fn();
    const item = { id: "app-p", label: "Portal 2", icon: "library" as const, to: "/app/library?q=Portal%202" };
    const { rerender } = renderHook(({ search }) => useOpenRequests(apps, reveal, search), { initialProps: { search: "" } });
    beforePaletteRun(item, false);
    act(() => afterPaletteRun(item, false));
    expect(reveal).not.toHaveBeenCalled();
    rerender({ search: "Portal 2" });
    expect(reveal).toHaveBeenCalledWith(apps[0], false);
  });

  it("a palette pick opens when the box already holds that search (nothing else re-renders)", () => {
    const reveal = vi.fn();
    const item = { id: "app-p", label: "Portal 2", icon: "library" as const, to: "/app/library?q=Portal%202" };
    renderHook(() => useOpenRequests(apps, reveal, "Portal 2"));
    beforePaletteRun(item, false);
    expect(reveal).not.toHaveBeenCalled();
    act(() => afterPaletteRun(item, false));
    expect(reveal).toHaveBeenCalledWith(apps[0], false);
  });
});
