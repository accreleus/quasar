import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { PasswordCard } from "./PasswordCard";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";
import { ApiError } from "../../../api/client";

vi.mock("../../../api/auth", () => ({ changePassword: vi.fn() }));
import * as authApi from "../../../api/auth";

describe("PasswordCard", () => {
  it("shows the plain-HTTP refusal as the server words it and stays signed in", async () => {
    const msg = "This server only accepts sign-in, registration and password changes over HTTPS.";
    vi.mocked(authApi.changePassword).mockRejectedValue(new ApiError(403, "forbidden", msg));
    const logout = vi.fn();
    const value: AuthContextValue = {
      status: "authenticated",
      user: null,
      token: "tok",
      isAdmin: false,
      sessionExpired: false,
      login: vi.fn(),
      claim: vi.fn(),
      logout,
    };
    render(
      <ToastProvider>
        <AuthContext.Provider value={value}>
          <MemoryRouter>
            <PasswordCard />
          </MemoryRouter>
        </AuthContext.Provider>
      </ToastProvider>,
    );

    fireEvent.change(screen.getByLabelText("Current password"), { target: { value: "old-password-123" } });
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "new-password-456" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "new-password-456" } });
    fireEvent.submit(screen.getByLabelText("Current password").closest("form")!);

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(msg));
    expect(logout).not.toHaveBeenCalled();
  });
});
