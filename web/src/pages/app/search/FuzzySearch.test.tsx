import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import type { App } from "../../../api/types";
import type { ExtrasMap, LibraryExtras } from "../library/libraryExtras";
import { FuzzySearch, metaFor } from "./FuzzySearch";

const extras = vi.hoisted(() => ({ map: null as ExtrasMap | null }));
vi.mock("../library/libraryExtras", async (orig) => ({
  ...(await orig<typeof import("../library/libraryExtras")>()),
  useLibraryExtras: () => extras.map,
}));

const app = (id: string, name: string, over: Partial<App> = {}) => ({ id, name, kind: "game", cover_url: null, favourite: false, ...over }) as unknown as App;
const apps = [app("c", "Cities: Skylines"), app("p", "Portal 2"), app("f", "Fable Anniversary", { favourite: true })];

function Box({ onChoose }: { onChoose: (a: App) => void }) {
  const [q, setQ] = useState("");
  return <FuzzySearch query={q} setQuery={setQ} apps={apps} onChoose={onChoose} />;
}

describe("FuzzySearch", () => {
  it("offers favourites on an empty focused box, then ranked hits as you type", () => {
    render(<Box onChoose={vi.fn()} />);
    const box = screen.getByRole("combobox", { name: "Search library" });
    fireEvent.focus(box);
    expect(screen.getByRole("option", { name: "Fable Anniversary" })).toBeInTheDocument();
    fireEvent.change(box, { target: { value: "ciies sky" } });
    expect(screen.getAllByRole("option").map((o) => o.getAttribute("aria-label"))).toEqual(["Cities: Skylines"]);
    // No play data to go on: the kind alone, never "not played yet".
    expect(screen.getByRole("option").querySelector(".fz-meta")).toHaveTextContent(/^Game$/);
  });

  it("Enter chooses the top hit; a click chooses the clicked one", () => {
    const onChoose = vi.fn();
    render(<Box onChoose={onChoose} />);
    const box = screen.getByRole("combobox");
    expect(box).not.toHaveAttribute("aria-controls");
    fireEvent.change(box, { target: { value: "portl" } });
    expect(box).toHaveAttribute("aria-controls", screen.getByRole("listbox").id);
    fireEvent.keyDown(box, { key: "Enter" });
    expect(onChoose).toHaveBeenLastCalledWith(apps[1]);
    fireEvent.change(box, { target: { value: "fabl" } });
    fireEvent.click(screen.getByRole("option", { name: "Fable Anniversary" }));
    expect(onChoose).toHaveBeenLastCalledWith(apps[2]);
  });

  it("says so when nothing matches, and Escape closes without leaving the field", () => {
    render(<Box onChoose={vi.fn()} />);
    const box = screen.getByRole("combobox");
    fireEvent.change(box, { target: { value: "zzzzzz" } });
    expect(screen.getByText("No games match “zzzzzz”")).toBeInTheDocument();
    // The empty line is not an option container, and nothing points at a listbox that is absent.
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(box).not.toHaveAttribute("aria-controls");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
  });
});

describe("FuzzySearch icon", () => {
  it("shows the title's icon at the 96 px size, before the tile art", () => {
    const icon = `/v1/artwork/${"a".repeat(64)}.png`;
    extras.map = { p: { genres: [], added_at: null, play: null, icon_url: icon } };
    try {
      render(<Box onChoose={vi.fn()} />);
      fireEvent.change(screen.getByRole("combobox"), { target: { value: "portl" } });
      const img = screen.getByRole("option", { name: "Portal 2" }).querySelector("img");
      expect(img).toHaveAttribute("src", `${icon}?w=96`);
      expect(img).toHaveAttribute("sizes", "36px");
      expect(img).toHaveAttribute("srcset", expect.stringContaining(`${icon}?w=320 320w`));
    } finally {
      extras.map = null;
    }
  });
});

describe("metaFor", () => {
  it("is the kind alone without play data, and says not played only when the data says so", () => {
    expect(metaFor(apps[1], null)).toBe("Game");
    expect(metaFor(apps[1], {})).toBe("Game");
    const e = { genres: ["Puzzle"], added_at: "2026-01-01T00:00:00Z", play: null } as LibraryExtras;
    expect(metaFor(apps[1], { p: e })).toBe("Puzzle · not played yet");
  });
});
