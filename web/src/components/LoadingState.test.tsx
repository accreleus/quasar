/**
 * LoadingState / EmptyState tests (UI component consolidation, Task 4).
 * LoadingState is a live-region ("status"/aria-live="polite") — the a11y
 * defect this task deliberately fixes across every migrated site.
 * EmptyState is plain muted text, no live-region semantics.
 */
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { LoadingState, EmptyState } from "./LoadingState";

describe("LoadingState", () => {
  it("renders default text as a polite live region with the muted class", () => {
    render(<LoadingState />);

    const el = screen.getByRole("status");
    expect(el).toHaveTextContent("Loading…");
    expect(el).toHaveAttribute("aria-live", "polite");
    expect(el).toHaveClass("muted");
  });

  it("renders custom children in place of the default text", () => {
    render(<LoadingState>Loading sessions…</LoadingState>);

    const el = screen.getByRole("status");
    expect(el).toHaveTextContent("Loading sessions…");
  });
});

describe("EmptyState", () => {
  it("renders muted text with no status role", () => {
    render(<EmptyState>No sessions yet.</EmptyState>);

    const el = screen.getByText("No sessions yet.");
    expect(el).toHaveClass("muted");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
