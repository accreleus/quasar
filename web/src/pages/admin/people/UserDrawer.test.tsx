// Covers the entitlements section added to UserDrawer for the
// steam-library-discovery spec §6.6 per-user entitlement view. The session
// history part is pre-existing presentation and is not re-tested here.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { UserDrawer } from "./UserDrawer";
import * as adminApi from "../../../api/admin";
import type { AdminUser, Entitlement } from "../../../api/types";

vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function user(over: Partial<AdminUser> = {}): AdminUser {
  return {
    id: "user-1",
    username: "alice",
    email: "alice@example.test",
    role: "user",
    disabled: false,
    max_concurrent_sessions: 2,
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  } as AdminUser;
}

function entitlement(over: Partial<Entitlement> = {}): Entitlement {
  return {
    id: "ent-1",
    subject_type: "user",
    subject_id: "user-1",
    subject_username: "alice",
    app_id: "app-1",
    app_name: "Portal 2",
    granted_by: "admin",
    granted_by_user: "admin-1",
    source_ref: "",
    created_at: "2026-07-29T00:00:00Z",
    ...over,
  };
}

function renderDrawer() {
  return render(
    <MemoryRouter>
      <UserDrawer
        user={user()}
        token="tok"
        onClose={() => {}}
        onRoleClick={() => {}}
        onDisableClick={() => {}}
      />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null });
});

describe("UserDrawer — personal library grants (spec §6.6)", () => {
  it("makes clear an empty list does not mean the user sees nothing", async () => {
    mocked.listUserEntitlements.mockResolvedValue({ items: [] });
    renderDrawer();

    await screen.findByText(/No personal grants/i);
    expect(screen.getByText(/still see apps marked/i)).toBeTruthy();
    // Standing caption makes the "personal only" scope explicit even before
    // the list resolves.
    expect(screen.getByText(/not apps this user can see because they are marked/i)).toBeTruthy();
  });

  it("lists each personal grant with its app name and how it was granted", async () => {
    mocked.listUserEntitlements.mockResolvedValue({
      items: [entitlement({ app_name: "Portal 2", granted_by: "admin" })],
    });
    renderDrawer();

    await screen.findByText("Portal 2");
    expect(screen.getByText("Granted by an admin")).toBeTruthy();
  });

  it("calls the personal-grants endpoint for this user, not the app-side one", async () => {
    mocked.listUserEntitlements.mockResolvedValue({ items: [] });
    renderDrawer();

    await waitFor(() =>
      expect(mocked.listUserEntitlements).toHaveBeenCalledWith("tok", "user-1"),
    );
  });
});
