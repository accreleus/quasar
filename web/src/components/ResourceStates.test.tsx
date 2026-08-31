// ResourceStates — the gating rules it exists to hold in one place.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ResourceStates } from "./ResourceStates";

describe("ResourceStates", () => {
  it("renders the loading line as a polite live region", () => {
    render(<ResourceStates loading />);
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("Loading…");
    expect(status).toHaveAttribute("aria-live", "polite");
  });

  it("takes a custom loading label", () => {
    render(<ResourceStates loading loadingLabel="Loading hosts…" />);
    expect(screen.getByRole("status")).toHaveTextContent("Loading hosts…");
  });

  it("marks the error line as an alert", () => {
    render(<ResourceStates loading={false} error="could not load hosts" />);
    expect(screen.getByRole("alert")).toHaveTextContent("could not load hosts");
  });

  it("shows the empty state only when settled, error-free and empty", () => {
    const { rerender } = render(<ResourceStates loading isEmpty empty="No presets yet" />);
    expect(screen.queryByText("No presets yet")).toBeNull(); // still loading

    rerender(<ResourceStates loading={false} error="boom" isEmpty empty="No presets yet" />);
    expect(screen.queryByText("No presets yet")).toBeNull(); // an error is not emptiness

    rerender(<ResourceStates loading={false} isEmpty empty="No presets yet" />);
    expect(screen.getByText("No presets yet")).toBeInTheDocument();

    rerender(<ResourceStates loading={false} isEmpty={false} empty="No presets yet" />);
    expect(screen.queryByText("No presets yet")).toBeNull();
  });

  it("renders no empty state when the copy is omitted (the table owns it)", () => {
    const { container } = render(<ResourceStates loading={false} isEmpty />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders an error alongside nothing else once data is present", () => {
    render(<ResourceStates loading={false} error="gateway" isEmpty={false} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.queryByRole("status")).toBeNull();
  });
});
