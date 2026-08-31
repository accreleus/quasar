import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AccessTab } from "./AccessTab";
import { ToastProvider } from "../../../../components/Toast";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AdminUser, Entitlement } from "../../../../api/types";

vi.mock("../../../../api/admin");
vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));

const mocked = vi.mocked(adminApi);

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

// The grants are loaded by the editor page (the tab bar carries their count),
// so the tab takes them as data and asks for a reload after each write.
function renderPanel(items: Entitlement[] = []) {
  const reload = vi.fn();
  render(
    <ToastProvider>
      <MemoryRouter>
        <AccessTab
          appId="app-1"
          token="tok"
          items={items}
          loading={false}
          error={null}
          reload={reload}
          libraryLink="/admin/library/apps/app-steam/library"
        />
      </MemoryRouter>
    </ToastProvider>,
  );
  return { reload };
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listUsers.mockResolvedValue({ items: [], next_cursor: null });
});

describe("Access tab — Everyone vs specific users", () => {
  it("shows Everyone selected when an 'all' entitlement exists", async () => {
    renderPanel([entitlement({ id: "all-1", subject_type: "all", subject_id: null, subject_username: null, granted_by: "migration" })]);

    const everyoneTab = await screen.findByRole("tab", { name: "Everyone" });
    expect(everyoneTab.getAttribute("aria-selected")).toBe("true");
  });

  it("shows Specific users selected when there is no 'all' row", async () => {
    renderPanel([entitlement()]);

    const specificTab = await screen.findByRole("tab", { name: "Specific users" });
    expect(specificTab.getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText("alice")).toBeTruthy();
  });

  it("grants an 'all' entitlement when Everyone is selected", async () => {
    mocked.grantEntitlement.mockResolvedValue({
      entitlement: entitlement({ id: "all-1", subject_type: "all", subject_id: null }),
    });
    renderPanel([]);

    fireEvent.click(await screen.findByRole("tab", { name: "Everyone" }));

    await waitFor(() =>
      expect(mocked.grantEntitlement).toHaveBeenCalledWith("tok", "app-1", { subject_type: "all" }),
    );
  });

  it("revokes the 'all' entitlement when switching back to specific users", async () => {
    mocked.revokeEntitlement.mockResolvedValue(undefined);
    renderPanel([entitlement({ id: "all-1", subject_type: "all", subject_id: null, subject_username: null })]);

    fireEvent.click(await screen.findByRole("tab", { name: "Specific users" }));

    await waitFor(() =>
      expect(mocked.revokeEntitlement).toHaveBeenCalledWith("tok", "app-1", "all-1"),
    );
  });
});

describe("Access tab — granting a specific user", () => {
  it("only offers users who do not already hold a grant", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [user({ id: "user-1", username: "alice" }), user({ id: "user-2", username: "bob" })],
      next_cursor: null,
    });
    renderPanel([entitlement({ subject_id: "user-1", subject_username: "alice" })]);

    await screen.findByText("alice");
    const picker = (await screen.findByLabelText("Add a user")) as HTMLSelectElement;
    const optionLabels = Array.from(picker.options).map((o) => o.textContent);
    expect(optionLabels).toContain("bob");
    expect(optionLabels).not.toContain("alice");
  });

  it("asks the page to re-read the grants after a write, so the tab count moves too", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user({ id: "user-2", username: "bob" })], next_cursor: null });
    mocked.grantEntitlement.mockResolvedValue({
      entitlement: entitlement({ id: "ent-2", subject_id: "user-2", subject_username: "bob" }),
    });
    const { reload } = renderPanel([]);

    fireEvent.change((await screen.findByLabelText("Add a user")) as HTMLSelectElement, {
      target: { value: "user-2" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Grant access" }));

    await waitFor(() => expect(reload).toHaveBeenCalled());
  });

  it("grants the picked user and clears the picker", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [user({ id: "user-2", username: "bob" })],
      next_cursor: null,
    });
    mocked.grantEntitlement.mockResolvedValue({
      entitlement: entitlement({ id: "ent-2", subject_id: "user-2", subject_username: "bob" }),
    });
    renderPanel([]);

    const picker = (await screen.findByLabelText("Add a user")) as HTMLSelectElement;
    fireEvent.change(picker, { target: { value: "user-2" } });
    fireEvent.click(screen.getByRole("button", { name: "Grant access" }));

    await waitFor(() =>
      expect(mocked.grantEntitlement).toHaveBeenCalledWith("tok", "app-1", {
        subject_type: "user",
        subject_id: "user-2",
      }),
    );
  });

  it("surfaces a 409 as 'already has access' rather than a generic failure", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [user({ id: "user-2", username: "bob" })],
      next_cursor: null,
    });
    mocked.grantEntitlement.mockRejectedValue(
      new ApiError(409, "conflict", "that subject already holds an entitlement for this app"),
    );
    renderPanel([]);

    const picker = (await screen.findByLabelText("Add a user")) as HTMLSelectElement;
    fireEvent.change(picker, { target: { value: "user-2" } });
    fireEvent.click(screen.getByRole("button", { name: "Grant access" }));

    await screen.findByText("That user already has access to this app.");
  });
});

describe("Access tab — revoking, and the provider-grant warning", () => {
  it("revokes a plain admin grant with no special warning", async () => {
    mocked.revokeEntitlement.mockResolvedValue(undefined);
    renderPanel([entitlement({ granted_by: "admin" })]);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    expect(screen.queryByText(/library sync/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Revoke access" }));
    await waitFor(() =>
      expect(mocked.revokeEntitlement).toHaveBeenCalledWith("tok", "app-1", "ent-1"),
    );
  });

  // Load-bearing copy (spec §6.6): revoking a provider-written grant does not
  // stop the next sync from re-granting it while the game is still installed,
  // and the fleet-wide fix is Ignore, not a per-user revoke.
  it("warns that a provider grant may be re-granted by the next sync, and names the fix", async () => {
    renderPanel([entitlement({ granted_by: "provider" })]);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));

    await screen.findByText(/This grant came from a library sync/i);
    expect(
      screen.getByText(/revoking one user’s entitlement is not how you get rid of a junk tile/i),
    ).toBeTruthy();
    expect(screen.getByText(/use\s+Ignore on the discovered tile/i)).toBeTruthy();
  });
});

// Load-bearing copy (spec §6.6): a sync-written grant comes back on the next
// sync, and the fleet-wide fix lives on the provider app's Library tab.
describe("Access tab — the sync note", () => {
  it("points at the provider app's Library tab when a grant came from a sync", async () => {
    renderPanel([entitlement({ granted_by: "provider" })]);

    await screen.findByText(/came from a library sync/i);
    const link = screen.getByRole("link", { name: /Steam › Library/ });
    expect(link.getAttribute("href")).toBe("/admin/library/apps/app-steam/library");
  });

  it("says nothing about a sync when every grant is an admin's", async () => {
    renderPanel([entitlement({ granted_by: "admin" })]);

    await screen.findByText("alice");
    expect(screen.queryByText(/came from a library sync/i)).toBeNull();
  });
});
