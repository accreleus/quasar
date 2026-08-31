/**
 * LoginPage — the v3 pre-auth card (handoff-v3-spec.md §C).
 *
 * What is worth a test here is everything the mock made behavioural: submit-
 * time validation with its three messages, focus landing on the first invalid
 * field, errors clearing as the user types, the in-field Show/Hide, the
 * submitting state, the 401 message rendering under the password field, and
 * "Keep me signed in on this device" actually reaching login() — that last one
 * is the whole point of the checkbox, and it is invisible in a screenshot.
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { LoginPage } from "./LoginPage";
import { AuthContext, type AuthContextValue } from "../auth/context";
import { ThemeProvider } from "../settings/ThemeContext";
import { ApiError } from "../api/client";

function renderLogin(login: AuthContextValue["login"], from?: string) {
  const value: AuthContextValue = {
    status: "unauthenticated",
    user: null,
    token: null,
    isAdmin: false,
    login,
    claim: vi.fn(),
    logout: vi.fn(),
  };
  const initial = from
    ? [{ pathname: "/login", state: { from: { pathname: from } } }]
    : [{ pathname: "/login" }];

  render(
    <ThemeProvider>
      <AuthContext.Provider value={value}>
        <MemoryRouter initialEntries={initial}>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/app" element={<div>library</div>} />
            <Route path="/admin/hosts" element={<div>hosts</div>} />
          </Routes>
        </MemoryRouter>
      </AuthContext.Provider>
    </ThemeProvider>,
  );
}

const emailBox = () => screen.getByLabelText("Email") as HTMLInputElement;
const passwordBox = () => screen.getByLabelText("Password") as HTMLInputElement;
const submitButton = () => screen.getByRole("button", { name: /sign in/i });

function fill(email: string, password: string) {
  fireEvent.change(emailBox(), { target: { value: email } });
  fireEvent.change(passwordBox(), { target: { value: password } });
}

describe("LoginPage", () => {
  it("renders the lockup, both fields and the remember box checked by default", () => {
    renderLogin(vi.fn());

    expect(screen.getByRole("heading", { name: "Quasar" })).toBeInTheDocument();
    expect(emailBox()).toHaveAttribute("placeholder", "you@example.com");
    expect(emailBox()).toHaveAttribute("autocomplete", "username");
    expect(passwordBox()).toHaveAttribute("placeholder", "Your password");
    expect(screen.getByLabelText(/keep me signed in on this device/i)).toBeChecked();
  });

  // shell.css hides `.wordmark` below 760px and under a collapsed rail; the
  // card's heading is a different element wearing the same class, and it must
  // survive both. A CSS rule is what broke it, so a CSS rule is what is
  // asserted — jsdom applies no stylesheets, so the check reads login.css.
  it("keeps its heading visible where the topbar's wordmark is hidden", () => {
    const css = readFileSync(resolve(__dirname, "../styles/login.css"), "utf8");
    expect(css).toMatch(/\.auth-scene \.wordmark \{[^}]*display:\s*block/);
  });

  it("omits the forgot-password link — there is no reset flow", () => {
    renderLogin(vi.fn());
    expect(screen.queryByText(/forgot password/i)).not.toBeInTheDocument();
  });

  it("keeps the invite link to registration", () => {
    renderLogin(vi.fn());
    expect(screen.getByRole("link", { name: /create an account/i })).toHaveAttribute("href", "/register");
  });

  it("validates on submit, focuses the first invalid field, and does not call login", () => {
    const login = vi.fn();
    renderLogin(login);

    fireEvent.click(submitButton());

    expect(screen.getByText("Enter your email address.")).toBeInTheDocument();
    expect(screen.getByText("Enter your password.")).toBeInTheDocument();
    expect(emailBox()).toHaveFocus();
    expect(emailBox()).toHaveAttribute("aria-invalid", "true");
    expect(login).not.toHaveBeenCalled();
  });

  it("reports a malformed email and focuses password when only it is missing", () => {
    renderLogin(vi.fn());

    fill("nope", "");
    fireEvent.click(submitButton());
    expect(screen.getByText("That does not look like an email address.")).toBeInTheDocument();

    fill("a@b.co", "");
    fireEvent.click(submitButton());
    expect(screen.getByText("Enter your password.")).toBeInTheDocument();
    expect(passwordBox()).toHaveFocus();
  });

  it("clears a field's error as soon as it is typed in", () => {
    renderLogin(vi.fn());

    fireEvent.click(submitButton());
    expect(screen.getByText("Enter your email address.")).toBeInTheDocument();

    fireEvent.change(emailBox(), { target: { value: "a" } });
    expect(screen.queryByText("Enter your email address.")).not.toBeInTheDocument();
    expect(emailBox()).not.toHaveAttribute("aria-invalid", "true");
    // The password error is untouched — clearing is per field.
    expect(screen.getByText("Enter your password.")).toBeInTheDocument();
  });

  it("shows and hides the password from inside the field", () => {
    renderLogin(vi.fn());
    const reveal = screen.getByRole("button", { name: "Show" });

    expect(passwordBox()).toHaveAttribute("type", "password");
    expect(reveal).toHaveAttribute("aria-pressed", "false");

    fireEvent.click(reveal);
    expect(passwordBox()).toHaveAttribute("type", "text");
    expect(passwordBox()).toHaveFocus();
    expect(screen.getByRole("button", { name: "Hide" })).toHaveAttribute("aria-pressed", "true");

    fireEvent.click(screen.getByRole("button", { name: "Hide" }));
    expect(passwordBox()).toHaveAttribute("type", "password");
  });

  it("signs in with remember=true by default and redirects to the intended route", async () => {
    const login = vi.fn().mockResolvedValue(undefined);
    renderLogin(login, "/admin/hosts");

    fill("a@b.co", "hunter2");
    fireEvent.click(submitButton());

    await waitFor(() => expect(screen.getByText("hosts")).toBeInTheDocument());
    expect(login).toHaveBeenCalledWith("a@b.co", "hunter2", true);
  });

  it("passes remember=false when the box is unchecked", async () => {
    const login = vi.fn().mockResolvedValue(undefined);
    renderLogin(login);

    fill("a@b.co", "hunter2");
    fireEvent.click(screen.getByLabelText(/keep me signed in on this device/i));
    fireEvent.click(submitButton());

    await waitFor(() => expect(login).toHaveBeenCalledWith("a@b.co", "hunter2", false));
    await waitFor(() => expect(screen.getByText("library")).toBeInTheDocument());
  });

  it("shows the submitting state while the request is in flight", async () => {
    let release: () => void = () => {};
    const login = vi.fn().mockReturnValue(new Promise<void>((resolve) => (release = resolve)));
    renderLogin(login);

    fill("a@b.co", "hunter2");
    fireEvent.click(submitButton());

    await waitFor(() => expect(screen.getByRole("button", { name: /signing in/i })).toBeDisabled());

    await act(async () => {
      release();
    });
    expect(screen.getByText("library")).toBeInTheDocument();
  });

  it("renders a 401 under the password field in the mock's words", async () => {
    const login = vi.fn().mockRejectedValue(new ApiError(401, "invalid_credentials", "unauthorized"));
    renderLogin(login);

    fill("a@b.co", "wrong");
    fireEvent.click(submitButton());

    await waitFor(() => expect(screen.getByText("Email or password is incorrect.")).toBeInTheDocument());
    expect(passwordBox()).toHaveAttribute("aria-invalid", "true");
    expect(submitButton()).not.toBeDisabled();
  });

  it("renders any other server message verbatim", async () => {
    const login = vi.fn().mockRejectedValue(new ApiError(429, "rate_limited", "too many attempts, try later"));
    renderLogin(login);

    fill("a@b.co", "hunter2");
    fireEvent.click(submitButton());

    await waitFor(() => expect(screen.getByText("too many attempts, try later")).toBeInTheDocument());
  });

  it("names a transport failure as one", async () => {
    const login = vi.fn().mockRejectedValue(new TypeError("network down"));
    renderLogin(login);

    fill("a@b.co", "hunter2");
    fireEvent.click(submitButton());

    await waitFor(() =>
      expect(
        screen.getByText("Could not reach the server. Check your connection and try again."),
      ).toBeInTheDocument(),
    );
  });
});
