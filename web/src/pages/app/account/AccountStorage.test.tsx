// The three stats and the table, and that a failed read stays an error rather
// than being reported as "you have no storage".

import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccountStorage } from "./AccountStorage";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ApiError } from "../../../api/client";

const getMyStorage = vi.hoisted(() => vi.fn());
vi.mock("../../../api/storage", () => ({ getMyStorage }));

const authValue = {
  status: "authenticated",
  user: { id: "u1", email: "me@example.com", username: "tester", role: "user" },
  token: "t0k3n",
  isAdmin: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
} as unknown as AuthContextValue;

function renderStorage() {
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={authValue}>
        <AccountStorage />
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const items = [
  {
    app_id: "a1",
    app_name: "Cyberpunk 2077",
    bytes_used: 6 * 1024 ** 3,
    last_used_at: "2026-08-28T10:00:00Z",
  },
  {
    app_id: "a2",
    app_name: "Blender",
    bytes_used: 2 * 1024 ** 3,
    last_used_at: "2026-08-24T10:00:00Z",
  },
];

beforeEach(() => {
  vi.clearAllMocks();
  getMyStorage.mockResolvedValue({ items });
});

describe("AccountStorage", () => {
  it("shows the three stats over the table", async () => {
    renderStorage();
    await screen.findByText("Cyberpunk 2077");

    expect(screen.getByText("Apps with storage")).toBeTruthy();
    expect(screen.getByText("2")).toBeTruthy();
    expect(screen.getByText("Total used")).toBeTruthy();
    expect(screen.getByText("8 GB")).toBeTruthy();
    expect(screen.getByText("Largest app")).toBeTruthy();
    // The largest stat and that app's own row cell both read 6.00 GB.
    expect(screen.getAllByText("6 GB").length).toBe(2);
  });

  it("links each app to the library, not the admin editor a user cannot open", async () => {
    renderStorage();
    const link = await screen.findByRole("link", { name: "Cyberpunk 2077" });
    expect(link.getAttribute("href")).toBe("/app/library?q=Cyberpunk%202077");
  });

  it("offers no Clear action — there is no self-serve tombstone endpoint", async () => {
    renderStorage();
    await screen.findByText("Cyberpunk 2077");
    expect(screen.queryByRole("button", { name: "Clear" })).toBeNull();
    expect(screen.queryByText(/cannot be undone/i)).toBeNull();
  });

  it("surfaces a 404 as an error, not as the empty state", async () => {
    getMyStorage.mockRejectedValue(new ApiError(404, "not_found", "not found"));
    renderStorage();
    await waitFor(() => expect(screen.getByText("not found")).toBeTruthy());
    expect(screen.queryByText(/No managed storage yet/i)).toBeNull();
  });

  it("falls back to a readable message on a network failure", async () => {
    getMyStorage.mockRejectedValue(new Error("Network failure"));
    renderStorage();
    await waitFor(() => expect(screen.getByText("could not load your storage")).toBeTruthy());
  });

  it("shows the empty state, not an error, when there is nothing yet", async () => {
    getMyStorage.mockResolvedValue({ items: [] });
    renderStorage();
    await waitFor(() => expect(screen.getByText(/No managed storage yet/i)).toBeTruthy());
    expect(screen.queryByText(/could not load/i)).toBeNull();
  });
});
