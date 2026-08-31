// #515 — a failed listInvites used to be caught into an empty array
// (`.catch(() => setInvites([]))`), so a control-plane hiccup rendered as
// "No invites minted yet." — indistinguishable from a genuinely empty
// instance. Migrated onto lib/resource/useResource + ResourceStates; these
// tests pin the three load-path outcomes plus the mutate-rejects-to-caller law.
//
// v3 (handoff §A.19): the mint form moved from an always-visible card into a
// modal behind the head's "Mint invite" action; the table's State column is
// derived by invitesState.ts (unit-tested separately) instead of the old
// inline inviteStatus(); "Copy link" only appears for an invite this page has
// seen the plaintext link for (freshly minted).

import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi, beforeEach } from "vitest";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { Invite, MintInviteResponse, SettingsResponse } from "../../../api/types";
import { ToastProvider } from "../../../components/Toast";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { PEOPLE_TABS } from "../../../components/shell/sectionTabs";
import { InvitesTab } from "./InvitesTab";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../api/admin");

const mocked = vi.mocked(adminApi);

function invite(over: Partial<Invite> = {}): Invite {
  return {
    id: "inv-1",
    code_prefix: "a1b2c3d4",
    role: "user",
    max_uses: 1,
    used_count: 0,
    expires_at: null,
    revoked_at: null,
    note: null,
    created_at: "2026-08-01T00:00:00Z",
    created_by_user_id: "admin-1",
    created_by_username: "salty2011",
    ...over,
  } as Invite;
}

function renderPage() {
  return render(
    <ToastProvider>
      <InvitesTab />
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getSettings.mockResolvedValue({
    settings: { registration_mode: "invite_only" },
  } as SettingsResponse);
});

describe("InvitesTab — load failure renders the error state, not an empty collection (#515)", () => {
  it("renders an error banner when listInvites rejects", async () => {
    mocked.listInvites.mockRejectedValue(new ApiError(500, "internal", "database is on fire"));
    renderPage();

    await screen.findByText("database is on fire");
    expect(screen.getByRole("alert")).toHaveTextContent("database is on fire");
  });

  it("renders the empty-collection copy (no error) for a genuinely empty, successful load", async () => {
    mocked.listInvites.mockResolvedValue({ invites: [] });
    renderPage();

    await screen.findByText("No invites minted yet.");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("renders the invites table for a successful load with data", async () => {
    mocked.listInvites.mockResolvedValue({ invites: [invite({ created_by_username: "priya" })] });
    renderPage();

    await screen.findByText("a1b2c3d4");
    expect(screen.getByText("priya")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a revoke failure rejects to the caller (mutate law) and leaves the row visible", async () => {
    mocked.listInvites.mockResolvedValue({ invites: [invite()] });
    mocked.revokeInvite.mockRejectedValue(new ApiError(500, "internal", "could not revoke"));
    const { container } = renderPage();

    await screen.findByText("a1b2c3d4");
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    fireEvent.click(await screen.findByRole("button", { name: "Revoke invite" }));

    // The failure surfaces as a toast (the caller's job) …
    await screen.findByText("could not revoke");
    // … not as the ResourceStates load-error banner — this was a write
    // failure, not a load failure — and the row is still there.
    expect(container.querySelector("p.form-error")).toBeNull();
    expect(screen.getAllByText("a1b2c3d4").length).toBeGreaterThan(0);
  });

  it("a successful revoke refreshes the list", async () => {
    mocked.listInvites
      .mockResolvedValueOnce({ invites: [invite()] })
      .mockResolvedValueOnce({ invites: [invite({ revoked_at: "2026-08-24T00:00:00Z" })] });
    mocked.revokeInvite.mockResolvedValue(undefined);
    renderPage();

    await screen.findByText("a1b2c3d4");
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    fireEvent.click(await screen.findByRole("button", { name: "Revoke invite" }));

    await waitFor(() => expect(mocked.listInvites).toHaveBeenCalledTimes(2));
    await screen.findByText("Revoked");
  });
});

describe("InvitesTab — registration mode card (handoff §A.19)", () => {
  it("shows the once-only note and the current mode in the segmented control", async () => {
    mocked.listInvites.mockResolvedValue({ invites: [] });
    renderPage();

    await screen.findByText(/shown/i);
    expect(screen.getByText(/cannot be retrieved afterwards/i)).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Invite only" })).toHaveAttribute("aria-selected", "true");
  });

  it("PATCHes only the new registration_mode when a segment is picked", async () => {
    mocked.listInvites.mockResolvedValue({ invites: [] });
    mocked.updateSettings.mockResolvedValue({ settings: { registration_mode: "open" } } as SettingsResponse);
    renderPage();

    await screen.findByRole("tab", { name: "Invite only" });
    fireEvent.click(screen.getByRole("tab", { name: "Open" }));

    await waitFor(() =>
      expect(mocked.updateSettings).toHaveBeenCalledWith("tok", { registration_mode: "open" }),
    );
  });
});

describe("InvitesTab — non-pending rows have no Revoke, and expired dims the row", () => {
  it("shows a disabled menu instead of Copy link / Revoke for a redeemed invite", async () => {
    mocked.listInvites.mockResolvedValue({
      invites: [invite({ used_count: 1, max_uses: 1 })],
    });
    renderPage();

    await screen.findByText("a1b2c3d4");
    expect(screen.getByText("Redeemed")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
    expect(screen.getByRole("button", { name: "No actions available for this invite" })).toBeDisabled();
  });

  it("colours an expired invite's Expires cell in the danger colour", async () => {
    mocked.listInvites.mockResolvedValue({
      invites: [invite({ expires_at: "2020-01-01T00:00:00Z" })],
    });
    renderPage();

    await screen.findByText("Expired");
    // fmtDate always includes the 4-digit year regardless of locale (CI-safe —
    // see lib/format.test.ts's "locale-agnostic in CI" convention).
    const cell = screen.getByText(/2020/);
    expect(cell).toHaveStyle({ color: "var(--danger-text)" });
  });
});

describe("InvitesTab — mint flow (handoff §A.19: modal behind the head action)", () => {
  it("sends the form's role/max-uses/expiry/note as the mint request body, and offers Copy link for the freshly minted row", async () => {
    const minted: MintInviteResponse["invite"] = {
      id: "inv-new",
      code: "PLAINTEXT-CODE",
      invite_url: "https://quasar.example/register?invite=PLAINTEXT-CODE",
      role: "admin",
      max_uses: 1,
      used_count: 0,
      expires_at: null,
      created_at: "2026-08-29T00:00:00Z",
    };
    // The silent refresh after minting re-fetches the canonical list — reflect
    // the new row there too, the way the server would.
    mocked.listInvites
      .mockResolvedValueOnce({ invites: [] })
      .mockResolvedValueOnce({
        invites: [
          invite({
            id: "inv-new",
            code_prefix: "plaintex",
            role: "admin",
            note: "For the ops team",
            created_by_username: "salty2011",
          }),
        ],
      });
    mocked.mintInvite.mockResolvedValue({ invite: minted });

    render(
      <MemoryRouter initialEntries={["/admin/people/invites"]}>
        <ToastProvider>
          <SectionHeadProvider title="People" tabs={PEOPLE_TABS}>
            <InvitesTab />
          </SectionHeadProvider>
        </ToastProvider>
      </MemoryRouter>,
    );
    await screen.findByText("No invites minted yet.");

    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Mint invite/ }));
    const dialog = await screen.findByRole("dialog", { name: "Mint an invite" });

    fireEvent.change(within(dialog).getByLabelText("Role"), { target: { value: "admin" } });
    fireEvent.change(within(dialog).getByLabelText("Note (optional)"), {
      target: { value: "For the ops team" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Mint invite" }));

    await waitFor(() =>
      expect(mocked.mintInvite).toHaveBeenCalledWith("tok", {
        role: "admin",
        max_uses: 1,
        expires_at: null,
        note: "For the ops team",
      }),
    );

    // Role folds into the Code cell's sub-line (no dedicated Role column) so
    // a pending admin-granting invite reads clearly right after minting.
    await screen.findByText("Invite created");
    await waitFor(() =>
      expect(screen.getByTitle("Admin · For the ops team")).toBeTruthy(),
    );
  });
});
