/**
 * The Bar primitive owns the no-value glyph. Callers used to spell it
 * themselves and disagreed ("n/a" on one card, "—" on the next), so what is
 * pinned here is that a bar with a null value renders the glyph and that an
 * unknown bar draws no fill — a 0%-filled bar reads as "confirmed empty".
 */

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Bar } from "./Bar";

describe("Bar", () => {
  it("renders the no-value glyph for a null value", () => {
    const { container } = render(<Bar label="USED" percent={0} value={null} unknown />);
    expect(container.querySelector(".val")).toHaveTextContent("—");
  });

  it("renders no value cell at all when the prop is omitted", () => {
    const { container } = render(<Bar label="USED" percent={40} />);
    expect(container.querySelector(".val")).toBeNull();
  });

  it("draws a fill only when the reading is known", () => {
    const { container: known } = render(<Bar percent={40} value="2/5" />);
    expect(known.querySelector(".bar span")).toHaveStyle({ width: "40%" });
    expect(screen.getByText("2/5")).toBeInTheDocument();

    const { container: unknown } = render(<Bar percent={40} value={null} unknown />);
    expect(unknown.querySelector(".bar span")).toBeNull();
  });
});
