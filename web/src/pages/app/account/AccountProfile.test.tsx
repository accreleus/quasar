// The identity card's four facts, and that the password form lives on this
// page rather than a route of its own (spec §3.3).

import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccountProfile } from "./AccountProfile";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";

const listDevices = vi.hoisted(() => vi.fn());
vi.mock("../../../api/auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../api/auth")>();
  return { ...actual, listDevices };
});

const getMyStorage = vi.hoisted(() => vi.fn());
vi.mock("../../../api/storage", () => ({ getMyStorage }));

function authValue(role: "user" | "admin" = "admin"): AuthContextValue {
  return {
    status: "authenticated",
    user: {
      id: "u1",
      email: "admin@quasar.local",
      username: "salty2011",
      role,
      created_at: "2025-08-14T00:00:00Z",
    },
    token: "t0k3n",
    isAdmin: role === "admin",
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  } as unknown as AuthContextValue;
}

function renderProfile(role: "user" | "admin" = "admin") {
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={authValue(role)}>
        <ToastProvider>
          <AccountProfile />
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listDevices.mockResolvedValue({
    devices: [
      {
        id: "d1",
        device_key: "abc",
        name: "Studio desktop",
        trusted: true,
        first_seen_at: "2026-08-01T00:00:00Z",
        last_seen_at: new Date().toISOString(),
        current: true,
        active_session_id: null,
        capabilities: {},
      },
      {
        id: "d2",
        device_key: "def",
        name: "TV",
        trusted: false,
        first_seen_at: "2026-08-01T00:00:00Z",
        last_seen_at: new Date().toISOString(),
        current: false,
        active_session_id: null,
        capabilities: {},
      },
    ],
  });
  getMyStorage.mockResolvedValue({
    items: [
      { app_id: "a1", app_name: "Cyberpunk 2077", bytes_used: 6_200_000_000, last_used_at: null },
      { app_id: "a2", app_name: "Blender", bytes_used: 1_800_000_000, last_used_at: null },
    ],
  });
});

describe("AccountProfile", () => {
  it("shows the identity line with initials, role and joined date", async () => {
    const { container } = renderProfile();
    expect(container.querySelector(".ac-ava")?.textContent).toBe("SA");
    expect(screen.getByText("salty2011")).toBeTruthy();
    expect(screen.getByText(/Admin · active/)).toBeTruthy();
    expect(screen.getByText(/admin@quasar\.local · joined/)).toBeTruthy();
    // Settles both reads before unmount, so neither lands outside act().
    await screen.findByText("7.5 GB");
  });

  it("counts the devices and totals the managed home", async () => {
    renderProfile();
    // Devices fact: two records from the list.
    await waitFor(() => expect(screen.getByText("2")).toBeTruthy());
    // Managed home: the sum of both apps (GiB), not the largest.
    await waitFor(() => expect(screen.getByText("7.5 GB")).toBeTruthy());
  });

  it("names all four facts", async () => {
    renderProfile();
    for (const label of ["Role", "Last sign-in", "Devices", "Managed home"]) {
      expect(screen.getByText(label)).toBeTruthy();
    }
    await waitFor(() => expect(listDevices).toHaveBeenCalled());
  });

  it("does not call a plain user an admin", async () => {
    renderProfile("user");
    expect(screen.getByText(/User · active/)).toBeTruthy();
    await screen.findByText("7.5 GB");
  });

  // last_seen_at is refreshed by this page's own device fetch, so it always
  // reads "just now" and tells the user nothing.
  it("dates the last sign-in from the current device's first_seen_at", async () => {
    listDevices.mockResolvedValue({
      devices: [
        {
          id: "d1",
          device_key: "abc",
          name: "Studio desktop",
          trusted: true,
          first_seen_at: new Date(Date.now() - 3 * 60 * 60_000).toISOString(),
          last_seen_at: new Date().toISOString(),
          current: true,
          active_session_id: null,
          capabilities: {},
        },
      ],
    });
    renderProfile();
    expect(await screen.findByText("3h")).toBeTruthy();
  });

  it("shows an em dash, never a fabricated 'now', when no device is current", async () => {
    listDevices.mockResolvedValue({
      devices: [
        {
          id: "d2",
          device_key: "def",
          name: "TV",
          trusted: false,
          first_seen_at: "2026-08-01T00:00:00Z",
          last_seen_at: "2026-08-01T00:00:00Z",
          current: false,
          active_session_id: null,
          capabilities: {},
        },
      ],
    });
    const { container } = renderProfile();
    await screen.findByText("7.5 GB");
    const signIn = Array.from(container.querySelectorAll(".ae-fact")).find(
      (f) => f.firstElementChild?.textContent === "Last sign-in",
    );
    expect(signIn?.lastElementChild?.textContent).toBe("—");
    expect(screen.queryByText("now")).toBeNull();
  });

  it("carries the password form and its sign-out warning", async () => {
    renderProfile();
    expect(screen.getByLabelText("Current password")).toBeTruthy();
    expect(screen.getByLabelText("New password")).toBeTruthy();
    expect(screen.getByLabelText("Confirm new password")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Update password" })).toBeTruthy();
    expect(screen.getByText(/signs you out of/i).textContent).toMatch(
      /every device.*including this one/is,
    );
    await screen.findByText("7.5 GB");
  });
});
