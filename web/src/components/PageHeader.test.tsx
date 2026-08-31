/**
 * PageHeader tests (UI component consolidation, Task 3).
 * Covers: title+sub+actions render into .page-head/.sub/.toolbar, and the
 * title-only case renders neither .sub nor .toolbar.
 */
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { PageHeader } from "./PageHeader";

describe("PageHeader", () => {
  it("renders title, sub and actions", () => {
    const { container } = render(
      <PageHeader
        title="Sessions"
        sub="Live and historical streaming sessions across the fleet."
        actions={<button>Refresh</button>}
      />,
    );

    const head = container.querySelector(".page-head");
    expect(head).toBeInTheDocument();

    const h1 = head?.querySelector("h1");
    expect(h1).toHaveTextContent("Sessions");

    const sub = head?.querySelector(".sub");
    expect(sub).toHaveTextContent("Live and historical streaming sessions across the fleet.");

    const toolbar = head?.querySelector(".toolbar");
    expect(toolbar).toBeInTheDocument();
    expect(toolbar).toContainElement(screen.getByRole("button", { name: "Refresh" }));
  });

  it("renders title-only with no sub and no toolbar", () => {
    const { container } = render(<PageHeader title="Invites" />);

    const head = container.querySelector(".page-head");
    expect(head).toBeInTheDocument();
    expect(head?.querySelector("h1")).toHaveTextContent("Invites");
    expect(head?.querySelector(".sub")).not.toBeInTheDocument();
    expect(head?.querySelector(".toolbar")).not.toBeInTheDocument();
  });
});
