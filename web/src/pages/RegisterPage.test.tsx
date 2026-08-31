/**
 * RegisterPage on the v3 pre-auth card. The card is shared with /login, so what
 * is tested here is what is this page's own: the invite code riding ?invite=
 * into the register call, the local password-match check, the server's error
 * codes becoming sentences, and signing straight in afterwards.
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RegisterPage } from "./RegisterPage";
import { AuthContext, type AuthContextValue } from "../auth/context";
import { ThemeProvider } from "../settings/ThemeContext";
import { ApiError } from "../api/client";

vi.mock("../api/auth", () => ({ register: vi.fn() }));
import * as authApi from "../api/auth";

function renderRegister(login: AuthContextValue["login"], path = "/register") {
  const value: AuthContextValue = {
    status: "unauthenticated",
    user: null,
    token: null,
    isAdmin: false,
    login,
    claim: vi.fn(),
    logout: vi.fn(),
  };
  render(
    <ThemeProvider>
      <AuthContext.Provider value={value}>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/register" element={<RegisterPage />} />
            <Route path="/app" element={<div>library</div>} />
          </Routes>
        </MemoryRouter>
      </AuthContext.Provider>
    </ThemeProvider>,
  );
}

function fill({ confirm = "a-very-long-password" } = {}) {
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "new@example.com" } });
  fireEvent.change(screen.getByLabelText("Username"), { target: { value: "newbie" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a-very-long-password" } });
  fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: confirm } });
  fireEvent.click(screen.getByRole("button", { name: /create account/i }));
}

beforeEach(() => {
  vi.mocked(authApi.register).mockReset();
});

describe("RegisterPage", () => {
  it("renders the shared lockup and its own fields", () => {
    renderRegister(vi.fn());

    expect(screen.getByRole("heading", { name: "Quasar" })).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toHaveAttribute("placeholder", "At least 12 characters");
    expect(screen.getByRole("link", { name: /sign in/i })).toHaveAttribute("href", "/login");
  });

  it("submits the invite code from the magic link and signs the new account in", async () => {
    vi.mocked(authApi.register).mockResolvedValue(undefined as never);
    const login = vi.fn().mockResolvedValue(undefined);
    renderRegister(login, "/register?invite=inv-123");

    expect(screen.getByRole("status")).toHaveTextContent("You’ve been invited to join Quasar.");
    fill();

    await waitFor(() => expect(screen.getByText("library")).toBeInTheDocument());
    expect(authApi.register).toHaveBeenCalledWith("new@example.com", "newbie", "a-very-long-password", "inv-123");
    expect(login).toHaveBeenCalledWith("new@example.com", "a-very-long-password");
  });

  it("refuses a mismatched confirmation without calling the server, under the field it is about", () => {
    renderRegister(vi.fn());

    fill({ confirm: "something-else" });

    const confirmBox = screen.getByLabelText("Confirm password");
    expect(screen.getByText("Passwords do not match.")).toHaveAttribute(
      "id",
      confirmBox.getAttribute("aria-describedby"),
    );
    expect(confirmBox).toHaveAttribute("aria-invalid", "true");
    expect(authApi.register).not.toHaveBeenCalled();
  });

  it("turns the server's error codes into sentences", async () => {
    vi.mocked(authApi.register).mockRejectedValue(new ApiError(403, "invalid_invite", "nope"));
    renderRegister(vi.fn());

    fill();

    // A server refusal is about the submission, not about the last field the
    // user touched — it must not be wired to Confirm password.
    await waitFor(() =>
      expect(screen.getByText("This invite link is invalid or has expired.")).toBeInTheDocument(),
    );
    expect(screen.getByText("This invite link is invalid or has expired.")).not.toHaveAttribute(
      "id",
      screen.getByLabelText("Confirm password").getAttribute("aria-describedby"),
    );
  });
});
