// #515 — a failed listUsers used to render behind a transient toast only; once
// the toast expired the page read "No users found." — indistinguishable from a
// genuinely empty instance. Migrated onto lib/resource/useResource +
// ResourceStates; these tests pin the three load-path outcomes plus the
// mutate-rejects-to-caller law for a row-level write.
//
// v3 (handoff §A.18): row actions moved from inline icon buttons into a
// per-row kebab menu ("Actions for {username}"); the promote/disable/delete
// tests below open that menu first. New tests cover the segmented
// All/Admins/Disabled toolbar and the bulk bar's three actions.

import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AdminUser } from "../../../api/types";
import { ToastProvider } from "../../../components/Toast";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { PEOPLE_TABS } from "../../../components/shell/sectionTabs";
import { UsersTab } from "./UsersTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function user(over: Partial<AdminUser> = {}): AdminUser {
  return {
    id: "u-1",
    email: "grace@example.com",
    username: "grace",
    role: "user",
    disabled: false,
    max_concurrent_sessions: 1,
    created_at: "2026-08-01T00:00:00Z",
    last_seen_at: null,
    active_session_count: 0,
    ...over,
  } as AdminUser;
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <UsersTab />
      </ToastProvider>
    </MemoryRouter>,
  );
}

async function openRowMenu(username: string) {
  fireEvent.click(await screen.findByRole("button", { name: `Actions for ${username}` }));
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listAdminHomes.mockResolvedValue({ items: [], next_cursor: null } as never);
});

describe("UsersTab — load failure renders the error state, not an empty collection (#515)", () => {
  it("renders an error banner when listUsers rejects", async () => {
    mocked.listUsers.mockRejectedValue(new ApiError(500, "internal", "database is on fire"));
    renderPage();

    await screen.findByText("database is on fire");
    expect(screen.getByRole("alert")).toHaveTextContent("database is on fire");
  });

  it("renders the empty-collection copy (no error) for a genuinely empty, successful load", async () => {
    mocked.listUsers.mockResolvedValue({ items: [], next_cursor: null });
    renderPage();

    await screen.findByText("No users found.");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("renders the users table for a successful load with data", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    renderPage();

    await screen.findByText("grace");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a failed best-effort storage read (listAdminHomes) does not fail the whole load — the user row still renders with no error banner", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    mocked.listAdminHomes.mockRejectedValue(new ApiError(404, "not_found", "endpoint not live yet"));
    renderPage();

    await screen.findByText("grace");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a role-change failure rejects to the caller (mutate law) and leaves the row's role unchanged", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    mocked.updateUser.mockRejectedValue(new ApiError(500, "internal", "server exploded"));
    const { container } = renderPage();

    await screen.findByText("grace");
    await openRowMenu("grace");
    fireEvent.click(screen.getByRole("menuitem", { name: "Promote to admin" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    // The failure surfaces as a toast (the caller's job) …
    await screen.findByText("server exploded");
    // … not as the ResourceStates load-error banner, and the row is unchanged.
    expect(container.querySelector("p.form-error")).toBeNull();
    expect(screen.getByText("grace")).toBeTruthy();
    expect(screen.queryByText("Admin")).toBeNull();
  });

  it("a successful role change updates the row locally via resource.mutate", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    mocked.updateUser.mockResolvedValue({ user: user({ role: "admin" }) });
    renderPage();

    await screen.findByText("grace");
    await openRowMenu("grace");
    fireEvent.click(screen.getByRole("menuitem", { name: "Promote to admin" }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(screen.getByText("Admin")).toBeTruthy());
  });
});

describe("UsersTab — toolbar (handoff §A.18)", () => {
  it("filters to Admins / Disabled via the segmented control", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [
        user({ id: "u-1", username: "grace", role: "user" }),
        user({ id: "u-2", username: "priya", role: "admin" }),
        user({ id: "u-3", username: "kenji", disabled: true }),
      ],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText("grace");

    fireEvent.click(screen.getByRole("tab", { name: "Admins" }));
    expect(screen.queryByText("grace")).toBeNull();
    expect(screen.getByText("priya")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: /Disabled/ }));
    expect(screen.queryByText("priya")).toBeNull();
    expect(screen.getByText("kenji")).toBeTruthy();
  });

  it("filters by name or email with the search input", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [user({ id: "u-1", username: "grace" }), user({ id: "u-2", username: "priya", email: "priya@example.com" })],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText("grace");

    fireEvent.change(screen.getByPlaceholderText("Filter by name or email"), {
      target: { value: "priya" },
    });
    expect(screen.queryByText("grace")).toBeNull();
    expect(screen.getByText("priya")).toBeTruthy();
  });

  it("shows the streaming chip when a user has an active session, else Active/Disabled", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [
        user({ id: "u-1", username: "grace", active_session_count: 2 }),
        user({ id: "u-2", username: "kenji", disabled: true }),
      ],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText("grace");

    expect(screen.getByText("Streaming")).toBeTruthy();
    // "Disabled" also names the toolbar segment (Disabled {n}) — scope to the
    // state chip specifically.
    expect(screen.getByRole("cell", { name: "Disabled" })).toBeTruthy();
  });

  it("renders a relative last-seen time, or Never when the user has none", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [
        user({ id: "u-1", username: "grace", last_seen_at: null }),
        user({ id: "u-2", username: "priya", last_seen_at: new Date().toISOString() }),
      ],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText("grace");

    expect(screen.getByText("Never")).toBeTruthy();
    expect(screen.getByText("just now")).toBeTruthy();
  });
});

describe("UsersTab — bulk bar (handoff §A.18)", () => {
  it("shows a bulk bar with Change role / Disable / Delete once a row is selected", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    renderPage();
    await screen.findByText("grace");

    fireEvent.click(screen.getByRole("checkbox", { name: "Select grace" }));

    // BulkBar splits the count into its own <span>, so the label is not one
    // text node — match on the toolbar's accessible name instead.
    expect(screen.getByRole("toolbar", { name: "Bulk actions" })).toHaveTextContent("1 user selected");
    expect(screen.getByRole("button", { name: "Change role" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Disable" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
  });

  it("bulk-sets the role for every selected user", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    mocked.updateUser.mockResolvedValue({ user: user({ role: "admin" }) });
    renderPage();
    await screen.findByText("grace");

    fireEvent.click(screen.getByRole("checkbox", { name: "Select grace" }));
    fireEvent.click(screen.getByRole("button", { name: "Change role" }));
    fireEvent.click(await screen.findByRole("button", { name: "Set to admin" }));

    await waitFor(() =>
      expect(mocked.updateUser).toHaveBeenCalledWith("tok", "u-1", { role: "admin" }),
    );
  });

  it("acts only on the selection ∩ the currently filtered rows: select three under All, switch to Admins, Delete sends exactly one request", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [
        user({ id: "u-1", username: "grace", role: "user" }),
        user({ id: "u-2", username: "priya", role: "admin" }),
        user({ id: "u-3", username: "devon", role: "user" }),
      ],
      next_cursor: null,
    });
    mocked.deleteUser.mockResolvedValue(undefined);
    renderPage();
    await screen.findByText("grace");

    fireEvent.click(screen.getByRole("checkbox", { name: "Select grace" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select priya" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select devon" }));
    expect(screen.getByRole("toolbar", { name: "Bulk actions" })).toHaveTextContent("3 users selected");

    // Switching segments trims the selection down to what's still visible —
    // only priya (the one admin among the three) survives.
    fireEvent.click(screen.getByRole("tab", { name: "Admins" }));
    expect(screen.getByRole("toolbar", { name: "Bulk actions" })).toHaveTextContent("1 user selected");

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete 1" }));

    await waitFor(() => expect(mocked.deleteUser).toHaveBeenCalledTimes(1));
    expect(mocked.deleteUser).toHaveBeenCalledWith("tok", "u-2");
  });
});

describe("UsersTab — Export (spec §9: client-side over the loaded rows)", () => {
  function stubDownload() {
    // jsdom's Blob doesn't implement .text() — stub Blob itself to capture
    // the CSV text `downloadCsv` handed it, rather than reading it back.
    let capturedContent = "";
    class FakeBlob {
      constructor(parts: BlobPart[]) {
        capturedContent = parts.join("");
      }
    }
    vi.stubGlobal("Blob", FakeBlob as unknown as typeof Blob);
    const created = vi.fn().mockReturnValue("blob:mock");
    const revoked = vi.fn();
    vi.stubGlobal("URL", { ...URL, createObjectURL: created, revokeObjectURL: revoked });
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    return {
      content: () => capturedContent,
      created,
      clickSpy,
      restore: () => {
        clickSpy.mockRestore();
        vi.unstubAllGlobals();
      },
    };
  }

  it("builds a CSV download from every loaded user when no filter is active", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [user({ id: "u-1", username: "grace" }), user({ id: "u-2", username: "priya" })],
      next_cursor: null,
    });
    // Export is a head action (useSectionHead) — it only renders under a
    // SectionHeadProvider, unlike the toolbar/table which are page body.
    render(
      <MemoryRouter initialEntries={["/admin/people/users"]}>
        <ToastProvider>
          <SectionHeadProvider title="People" tabs={PEOPLE_TABS}>
            <UsersTab />
          </SectionHeadProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    await screen.findByText("grace");

    const dl = stubDownload();
    fireEvent.click(screen.getByRole("button", { name: "Export" }));

    expect(dl.created).toHaveBeenCalled();
    expect(dl.content()).toContain("grace");
    expect(dl.content()).toContain("priya");
    expect(dl.clickSpy).toHaveBeenCalled();
    dl.restore();
  });

  it("exports only the segment/search-filtered rows, not the full loaded set", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [
        user({ id: "u-1", username: "grace", email: "grace@example.com" }),
        user({ id: "u-2", username: "priya", email: "priya@example.com" }),
      ],
      next_cursor: null,
    });
    render(
      <MemoryRouter initialEntries={["/admin/people/users"]}>
        <ToastProvider>
          <SectionHeadProvider title="People" tabs={PEOPLE_TABS}>
            <UsersTab />
          </SectionHeadProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    await screen.findByText("grace");

    fireEvent.change(screen.getByPlaceholderText("Filter by name or email"), {
      target: { value: "priya" },
    });

    const dl = stubDownload();
    fireEvent.click(screen.getByRole("button", { name: "Export" }));

    expect(dl.content()).toContain("priya");
    expect(dl.content()).not.toContain("grace");
    dl.restore();
  });
});

describe("UsersTab — head (handoff §A.18)", () => {
  it("publishes the active/admins sub-line, Export/Invite actions and the Users tab count", async () => {
    mocked.listUsers.mockResolvedValue({
      items: [
        user({ id: "u-1", username: "grace", role: "admin" }),
        user({ id: "u-2", username: "priya", disabled: true }),
      ],
      next_cursor: null,
    });
    render(
      <MemoryRouter initialEntries={["/admin/people/users"]}>
        <ToastProvider>
          <SectionHeadProvider title="People" tabs={PEOPLE_TABS}>
            <UsersTab />
          </SectionHeadProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    await screen.findByText("grace");

    // grace is disabled=false → active; priya is disabled → not active.
    // Only grace is an admin.
    expect(screen.getByText("1 active · 1 admins")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Export/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Invite user/ })).toBeTruthy();
    expect(screen.getByRole("tab", { name: /Users/ })).toHaveTextContent("2");
  });

  it("Refresh re-fetches — the page has no poll, so this is the only way back to a fresh read", async () => {
    mocked.listUsers.mockResolvedValue({ items: [user()], next_cursor: null });
    render(
      <MemoryRouter initialEntries={["/admin/people/users"]}>
        <ToastProvider>
          <SectionHeadProvider title="People" tabs={PEOPLE_TABS}>
            <UsersTab />
          </SectionHeadProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    await screen.findByText("grace");
    expect(mocked.listUsers).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));

    await waitFor(() => expect(mocked.listUsers).toHaveBeenCalledTimes(2));
  });
});
