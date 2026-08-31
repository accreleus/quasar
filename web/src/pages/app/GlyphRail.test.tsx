// The four-step glyph rail (handoff-v3 §D). Every state is painted from one
// step index, and each completed step keeps its own glyph — a row of identical
// ticks would erase which phase finished.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { GlyphRail, RAIL_STEPS } from "./GlyphRail";

const states = () =>
  screen.getAllByRole("listitem").map((li) => li.getAttribute("data-state"));

describe("GlyphRail", () => {
  it("labels itself and its four steps for assistive tech", () => {
    render(<GlyphRail step={0} />);
    expect(screen.getByRole("list", { name: "Connection progress" })).toBeInTheDocument();
    for (const label of ["Signalling", "Secure path", "Video channel", "Input capture"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(RAIL_STEPS).toHaveLength(4);
  });

  it("marks earlier steps done, the current one active and the rest idle", () => {
    const { rerender } = render(<GlyphRail step={0} />);
    expect(states()).toEqual(["active", "idle", "idle", "idle"]);

    rerender(<GlyphRail step={2} />);
    expect(states()).toEqual(["done", "done", "active", "idle"]);

    rerender(<GlyphRail step={3} />);
    expect(states()).toEqual(["done", "done", "done", "active"]);
  });

  it("shows every step done and none active at the handoff", () => {
    render(<GlyphRail step={4} />);
    expect(states()).toEqual(["done", "done", "done", "done"]);
  });

  it("keeps each step's own glyph once it is done", () => {
    const { container } = render(<GlyphRail step={4} />);
    // The padlock's shackle and the gamepad's keys are still in the DOM: the
    // done state is a ring and a colour, not a substitution.
    expect(container.querySelector(".shackle")).not.toBeNull();
    expect(container.querySelectorAll(".key")).toHaveLength(2);
  });

  it("draws three connectors, hidden from assistive tech", () => {
    const { container } = render(<GlyphRail step={1} />);
    const links = [...container.querySelectorAll(".sl-link")];
    expect(links).toHaveLength(3);
    expect(links.map((l) => l.getAttribute("data-state"))).toEqual(["done", "active", "idle"]);
    for (const link of links) expect(link).toHaveAttribute("aria-hidden", "true");
  });
});
