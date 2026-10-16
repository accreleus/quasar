import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import type { App } from "../../../api/types";
import { beforePaletteRun } from "../../../components/shell/paletteGames";
import { requestOpen, useOpenRequests } from "./useFuzzy";

const apps = [{ id: "p", name: "Portal 2" }] as App[];

describe("open requests", () => {
  it("a palette game row opens that game once it is in the list on screen", () => {
    const reveal = vi.fn();
    beforePaletteRun({ id: "app-p", label: "Portal 2", icon: "library", to: "/app/library?q=Portal" }, false);
    const { rerender } = renderHook(({ list }) => useOpenRequests(list, reveal, ""), { initialProps: { list: [] as App[] } });
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
});
