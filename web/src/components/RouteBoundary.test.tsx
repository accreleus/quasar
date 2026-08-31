import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { RouteErrorBoundary, RouteSkeleton } from "./RouteBoundary";

function Broken({ fail }: { fail: boolean }) {
  if (fail) throw new Error("chunk exploded");
  return <p>Recovered route</p>;
}

describe("RouteBoundary", () => {
  it("renders a stable accessible loading skeleton", () => {
    render(<RouteSkeleton label="Loading hosts" />);
    expect(screen.getByRole("status", { name: "Loading hosts" })).toBeInTheDocument();
  });

  it("shows actionable detail and can retry a render failure", () => {
    const error = vi.spyOn(console, "error").mockImplementation(() => undefined);
    let fail = true;
    const { rerender } = render(
      <RouteErrorBoundary><Broken fail={fail} /></RouteErrorBoundary>,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("This view could not be rendered");
    expect(screen.getByText("chunk exploded")).toBeInTheDocument();
    fail = false;
    rerender(<RouteErrorBoundary><Broken fail={fail} /></RouteErrorBoundary>);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(screen.getByText("Recovered route")).toBeInTheDocument();
    error.mockRestore();
  });
});
