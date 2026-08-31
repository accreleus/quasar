// #521: the provider stack in main.tsx (ThemeProvider -> AuthProvider ->
// SetupStatusProvider -> ToastProvider -> App) used to sit ABOVE the only
// error boundary, which lived inside App wrapping <Routes>. A throw during a
// provider's render/init (AuthProvider being the plausible one) was uncaught
// and produced a blank page. main.tsx now wraps the whole provider stack in
// RouteErrorBoundary. This test proves that composition shape directly,
// independent of main.tsx's own createRoot() side effect (which can't be
// exercised from a unit test without a real DOM root).
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { RouteErrorBoundary } from "./components/RouteBoundary";

function ThrowingProvider(): never {
  // Simulates a provider (e.g. AuthProvider) throwing during its own render,
  // as opposed to a throw from a routed page deeper in the tree.
  throw new Error("provider init failed");
}

describe("provider stack error boundary (#521)", () => {
  it("catches a throw from a provider above App and renders the boundary UI, not a blank page", () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => undefined);

    render(
      <RouteErrorBoundary>
        <ThrowingProvider />
      </RouteErrorBoundary>,
    );

    expect(screen.getByRole("alert")).toHaveTextContent("This view could not be rendered");
    expect(screen.getByRole("button", { name: "Reload application" })).toBeInTheDocument();

    consoleError.mockRestore();
  });
});
