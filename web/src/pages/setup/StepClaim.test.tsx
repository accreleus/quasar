/**
 * StepClaim — claim-error handling (Spec B W1 acceptance: "A wrong or
 * missing token returns 401 with no distinction between the two cases" /
 * "Claim is refused with 409 once an admin exists"). The component must
 * show the server's own message verbatim, and on 409 must offer a way to
 * /login instead of a dead-end retry.
 */

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { StepClaim } from "./StepClaim";
import { AuthContext } from "../../auth/context";
import type { AuthContextValue } from "../../auth/context";
import { ApiError } from "../../api/client";

function renderStepClaim(claim: AuthContextValue["claim"], onClaimed = vi.fn()) {
  const authValue: AuthContextValue = {
    status: "unauthenticated",
    user: null,
    token: null,
    isAdmin: false,
    login: vi.fn(),
    claim,
    logout: vi.fn(),
  };
  render(
    <MemoryRouter>
      <AuthContext.Provider value={authValue}>
        <StepClaim onClaimed={onClaimed} />
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

function fillAndSubmit() {
  fireEvent.change(screen.getByLabelText(/setup token/i), { target: { value: "the-token" } });
  fireEvent.change(screen.getByLabelText(/email/i), { target: { value: "admin@example.com" } });
  fireEvent.change(screen.getByLabelText(/username/i), { target: { value: "admin" } });
  fireEvent.change(screen.getByLabelText(/^password$/i), { target: { value: "a-strong-password-123" } });
  fireEvent.click(screen.getByRole("button", { name: /create admin account/i }));
}

describe("StepClaim error handling", () => {
  it("calls claim with the form fields and onClaimed on success", async () => {
    const claim = vi.fn().mockResolvedValue(undefined);
    const onClaimed = vi.fn();
    renderStepClaim(claim, onClaimed);

    fillAndSubmit();

    await waitFor(() => expect(onClaimed).toHaveBeenCalled());
    expect(claim).toHaveBeenCalledWith("the-token", "admin@example.com", "admin", "a-strong-password-123");
  });

  it("shows the server's exact message on 401 (bad/missing token), no retry link", async () => {
    const claim = vi.fn().mockRejectedValue(new ApiError(401, "unauthorized", "invalid setup token"));
    renderStepClaim(claim);

    fillAndSubmit();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("invalid setup token");
    });
    // Still on the claim form — a 401 is retryable, not a dead end.
    expect(screen.getByRole("button", { name: /create admin account/i })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /go to sign in/i })).not.toBeInTheDocument();
  });

  it("shows the server's exact message on 409 and offers a sign-in link instead of retry", async () => {
    const claim = vi
      .fn()
      .mockRejectedValue(new ApiError(409, "setup_already_complete", "this instance is already set up"));
    renderStepClaim(claim);

    fillAndSubmit();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("this instance is already set up");
    });
    expect(screen.getByRole("link", { name: /go to sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /create admin account/i })).not.toBeInTheDocument();
  });

  it("falls back to a network-error message for a non-ApiError failure", async () => {
    const claim = vi.fn().mockRejectedValue(new Error("fetch failed"));
    renderStepClaim(claim);

    fillAndSubmit();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent(/could not reach the server/i);
    });
  });
});
