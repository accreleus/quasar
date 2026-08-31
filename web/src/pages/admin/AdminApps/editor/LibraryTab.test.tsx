// Library tab — the "Seen, not published" list (steam-library-discovery spec
// §8.2, Phase 4). Property under test: it is a read and a button, not a
// review queue (no "pending"/"waiting" language), the un-ignore action writes
// rule='allow' against the RIGHT (appId, external_id) pair, and `suppressed_by:
// "other"` is rendered exactly as uncertain as the backend states it — never a
// more confident guess.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { LibraryTab } from "./LibraryTab";
import { ToastProvider } from "../../../../components/Toast";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { LibraryUnpublishedItem } from "../../../../api/types";

vi.mock("../../../../api/admin");

const mocked = vi.mocked(adminApi);

function item(over: Partial<LibraryUnpublishedItem> = {}): LibraryUnpublishedItem {
  return {
    external_source: "steam",
    external_id: "228980",
    name: "Steamworks Common Redistributables",
    suppressed_by: "builtin_prefix",
    users: 3,
    last_seen_at: "2026-07-28T00:00:00Z",
    has_tile: false,
    ...over,
  };
}

// The list is loaded by the editor page (the tab bar carries its count), so the
// tab takes it as data and asks for a reload after an un-ignore.
function renderPanel(items: LibraryUnpublishedItem[] = []) {
  const reload = vi.fn();
  render(
    <ToastProvider>
      <LibraryTab
        appId="steam-provider-1"
        token="tok"
        items={items}
        loading={false}
        error={null}
        reload={reload}
      />
    </ToastProvider>,
  );
  return { reload };
}

beforeEach(() => {
  vi.resetAllMocks();
});

describe("Library tab — it is a read and a button, not a review queue", () => {
  it("never implies anything is pending approval or awaiting review", async () => {
    renderPanel([item()]);

    await screen.findByText("Steamworks Common Redistributables", {}, { timeout: 5000 });
    expect(screen.queryByText(/pending approval/i)).toBeNull();
    expect(screen.queryByText(/awaiting/i)).toBeNull();
    // The explicit, positive denial IS what rules out the review-queue framing
    // — "not a review queue" necessarily contains the words "review queue",
    // so asserting their absence would be self-defeating; assert the denial
    // sentence itself instead.
    expect(screen.getByText(/not a review queue/i)).toBeTruthy();
    expect(
      screen.getByText(/discovery works correctly whether or not it is ever opened/i),
    ).toBeTruthy();
  });

  it("shows an empty state, not an error, when nothing is suppressed", async () => {
    renderPanel([]);

    await screen.findByText(/nothing suppressed right now/i, {}, { timeout: 5000 });
  });
});

describe("Library tab — honest suppressed_by labelling", () => {
  it("names the built-in denylist layer plainly", async () => {
    renderPanel([item({ suppressed_by: "builtin_appid" })]);

    await screen.findByText(/Built-in denylist \(this appid\)/i, {}, { timeout: 5000 });
  });

  it("renders 'other' as uncertain, never a confident guess", async () => {
    renderPanel([item({ suppressed_by: "other" })]);

    await screen.findByText(/reason unclear/i, {}, { timeout: 5000 });
  });

  // "appdetails" is a DISTINCT enum value from "other" in the Phase 4 contract
  // (LibrarySuppressedBy) — the opt-in third-party lookup is a more confident
  // rung than the leftover-uncertainty case, so it gets its own label rather
  // than being folded back into "other".
  it("names the third-party appdetails lookup as its own case, distinct from 'other'", async () => {
    renderPanel([item({ suppressed_by: "appdetails" })]);

    await screen.findByText(/appdetails lookup/i, {}, { timeout: 5000 });
    expect(screen.queryByText(/reason unclear/i)).toBeNull();
  });
});

describe("Library tab — the un-ignore action (spec §8.2)", () => {
  it("writes rule='allow' against the panel's appId and the item's own external_id/source", async () => {
    mocked.setLibraryRule.mockResolvedValue({
      rule: {
        external_source: "steam",
        external_id: "228980",
        rule: "allow",
        note: "",
        created_by: "admin-1",
        created_at: "2026-07-29T00:00:00Z",
      },
      disabled: false,
      revoked: 0,
    });
    renderPanel([item()]);

    fireEvent.click(await screen.findByRole("button", { name: "Un-ignore" }, { timeout: 5000 }));

    await waitFor(
      () =>
        expect(mocked.setLibraryRule).toHaveBeenCalledWith("tok", "steam-provider-1", "228980", {
          rule: "allow",
          external_source: "steam",
        }),
      { timeout: 5000 },
    );
  });

  it("tells the operator publishing happens on the NEXT scan, not immediately", async () => {
    mocked.setLibraryRule.mockResolvedValue({
      rule: { external_source: "steam", external_id: "228980", rule: "allow", note: "", created_by: null, created_at: "" },
      disabled: false,
      revoked: 0,
    });
    renderPanel([item()]);

    fireEvent.click(await screen.findByRole("button", { name: "Un-ignore" }, { timeout: 5000 }));

    await screen.findByText(/will publish on the next scan/i, {}, { timeout: 5000 });
  });

  it("surfaces a failure rather than pretending the un-ignore worked", async () => {
    mocked.setLibraryRule.mockRejectedValue(new ApiError(500, "internal", "could not write appid rule"));
    renderPanel([item()]);

    fireEvent.click(await screen.findByRole("button", { name: "Un-ignore" }, { timeout: 5000 }));

    await screen.findByText("could not write appid rule", {}, { timeout: 5000 });
  });
});
