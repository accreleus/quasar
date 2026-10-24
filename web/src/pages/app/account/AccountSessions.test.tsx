// Each row's Resume and End name their session's app, so a screen reader does
// not hear the same two buttons once per row (#706).

import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccountSessions } from "./AccountSessions";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";

const lib = vi.hoisted(() => ({ getMySessions: vi.fn(), listApps: vi.fn(), stopSession: vi.fn() }));
vi.mock("../../../api/library", () => lib);

const authValue = {
  status: "authenticated",
  user: { id: "u1", email: "me@example.com", username: "tester", role: "user" },
  token: "t0k3n",
  isAdmin: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
} as unknown as AuthContextValue;

const session = (id: string, app_id: string, state = "running") => ({
  id,
  app_id,
  host_id: "h1234567",
  state,
  started_at: "2026-10-01T00:00:00Z",
  ended_at: null,
});

beforeEach(() => {
  vi.clearAllMocks();
  lib.getMySessions.mockResolvedValue({
    items: [session("s1", "a1"), session("s2", "a2"), session("s3", "a3", "starting")],
  });
  lib.listApps.mockResolvedValue({
    items: [
      { id: "a1", name: "Portal 2" },
      { id: "a2", name: "Blender" },
    ],
  });
});

describe("AccountSessions", () => {
  it("gives each row's Resume and End a name that says which session it acts on", async () => {
    render(
      <MemoryRouter>
        <AuthContext.Provider value={authValue}>
          <ToastProvider>
            <AccountSessions />
          </ToastProvider>
        </AuthContext.Provider>
      </MemoryRouter>,
    );

    const resume = await screen.findByRole("button", { name: "Resume Portal 2" });
    expect(resume.textContent).toBe("Resume");
    expect(screen.getByRole("button", { name: "Resume Blender" })).toBeTruthy();
    // Only running sessions resume; the starting one (no catalog name) falls back to its id.
    expect(screen.queryByRole("button", { name: /^Resume a3/ })).toBeNull();

    const end = screen.getByRole("button", { name: "End Portal 2" });
    expect(end.textContent).toBe("End");
    expect(screen.getByRole("button", { name: "End Blender" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "End a3" })).toBeTruthy();
  });
});
