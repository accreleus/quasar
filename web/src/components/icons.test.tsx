/**
 * Shared icon module — path/attribute contract tests.
 * Verifies the uniform IconProps signature (aria-hidden vs role=img/aria-label,
 * default "ic" class) and spot-checks canonical path data for a couple of the
 * highest-duplication glyphs so a future edit can't silently drift them.
 */
import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";

import { IconClose, IconSearch } from "./icons";

describe("icons", () => {
  it("IconClose renders aria-hidden with the default 'ic' class", () => {
    const { container } = render(<IconClose />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(svg).toHaveClass("ic");
    expect(svg).not.toHaveAttribute("role");
  });

  it("IconClose exposes role=img + aria-label when given a label", () => {
    const { container } = render(<IconClose label="Close" />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("role", "img");
    expect(svg).toHaveAttribute("aria-label", "Close");
    expect(svg).not.toHaveAttribute("aria-hidden");
  });

  it("IconClose path data matches the canonical close-X glyph", () => {
    const { container } = render(<IconClose />);
    const path = container.querySelector("path");
    expect(path).toHaveAttribute("d", "M4 4l8 8M12 4l-8 8");
  });

  it("IconSearch path/circle data matches the canonical search glyph", () => {
    const { container } = render(<IconSearch />);
    const circle = container.querySelector("circle");
    const path = container.querySelector("path");
    expect(circle).toHaveAttribute("cx", "7");
    expect(circle).toHaveAttribute("cy", "7");
    expect(circle).toHaveAttribute("r", "5");
    expect(path).toHaveAttribute("d", "M11 11l3 3");
  });
});
