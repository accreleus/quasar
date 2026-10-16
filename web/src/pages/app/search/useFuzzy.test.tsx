import { describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { App } from "../../../api/types";
import { isGamePick, runGamePick } from "../../../components/shell/paletteGames";
import { requestOpen, useOpenRequests } from "./useFuzzy";

const apps = [{ id: "p", name: "Portal 2" }] as App[];

const item = { id: "app-p", label: "Portal 2", icon: "library" as const, to: "/app/library?q=Portal%202" };

describe("open requests", () => {
  it("a palette pick opens that game once it is in the list the box now asks for", () => {
    const reveal = vi.fn();
    const setQuery = vi.fn();
    const { rerender } = renderHook(({ list, search }) => useOpenRequests(list, reveal, search), {
      initialProps: { list: [] as App[], search: "" },
    });
    runGamePick(item, setQuery, vi.fn());
    expect(setQuery).toHaveBeenCalledWith("Portal 2");
    rerender({ list: apps, search: "" });
    expect(reveal).not.toHaveBeenCalled();
    rerender({ list: apps, search: "Portal 2" });
    expect(reveal).toHaveBeenCalledWith(apps[0], false);
  });

  it("a palette pick opens when the box already holds the name (the wake-up is the only signal)", async () => {
    const reveal = vi.fn();
    renderHook(() => useOpenRequests(apps, reveal, "Portal 2"));
    await act(async () => runGamePick(item, vi.fn(), vi.fn()));
    expect(reveal).toHaveBeenCalledWith(apps[0], false);
  });

  it("an admin's palette row is not a game pick", () => {
    expect(isGamePick({ ...item, to: "/admin/library/apps/p" }, true)).toBe(false);
    expect(isGamePick(item, false)).toBe(true);
    expect(isGamePick({ id: "host-h", label: "Host", icon: "library" }, false)).toBe(false);
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
});
