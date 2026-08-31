import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "../../api/admin";
import type { AdminActivityItem, AdminActivityResponse } from "../../api/admin";
import { ToastProvider } from "../../components/Toast";
import { Audit } from "./Audit";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const mocked = vi.mocked(adminApi);

// Fixed "now" so day-group labels (Today / Yesterday) are deterministic.
const NOW = new Date(2026, 7, 8, 15, 0, 0);

function item(over: Partial<AdminActivityItem>): AdminActivityItem {
  return {
    id: 1,
    actor_user_id: "u-1",
    actor_username: "salty2011",
    action: "host.drain",
    target_type: "host",
    target_id: "h-1234567890",
    details: { reason: "maintenance" },
    created_at: new Date(2026, 7, 8, 9, 0, 0).toISOString(),
    severity: "info",
    ...over,
  };
}

const TODAY_WARN = item({
  id: 1,
  action: "host.drain",
  severity: "warn",
  actor_username: "salty2011",
  actor_user_id: "u-1",
  target_type: "host",
  target_id: "h-1234567890",
  created_at: new Date(2026, 7, 8, 9, 0, 0).toISOString(),
});
const TODAY_ERR_SYSTEM = item({
  id: 2,
  action: "session.failed",
  severity: "err",
  actor_username: null,
  actor_user_id: null,
  target_type: "session",
  target_id: "s-abcdef1234",
  created_at: new Date(2026, 7, 8, 13, 0, 0).toISOString(),
});
const YESTERDAY_INFO = item({
  id: 3,
  action: "app.update",
  severity: "info",
  actor_username: "priya",
  actor_user_id: "u-2",
  target_type: "app",
  target_id: "a-1",
  created_at: new Date(2026, 7, 7, 10, 0, 0).toISOString(),
});

function page(items: AdminActivityItem[], next_cursor: number | null = null): AdminActivityResponse {
  return { items, next_cursor };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <Audit />
      </ToastProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
});

describe("Audit — loading rows and day cards", () => {
  it("renders a card per local day, newest first, with the right entry count", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, TODAY_ERR_SYSTEM, YESTERDAY_INFO]));
    renderPage();

    await screen.findByText("Today · 8 August 2026");
    expect(screen.getByText("Yesterday · 7 August 2026")).toBeInTheDocument();
    expect(screen.getByText("2 entries")).toBeInTheDocument();
    expect(screen.getByText("1 entry")).toBeInTheDocument();

    // Raw action strings render as mono chips; the humanised label sits in Detail.
    expect(screen.getByText("host.drain")).toBeInTheDocument();
    expect(screen.getByText("Drained host")).toBeInTheDocument();
    // System actor shows the S tile and "system" label.
    expect(screen.getByText("system")).toBeInTheDocument();
    expect(screen.getByText("salty2011")).toBeInTheDocument();
  });

  it("renders the empty state when the range has no entries", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([]));
    renderPage();
    await screen.findByText("No entries in this range.");
  });

  it("surfaces a load failure as an inline error", async () => {
    const { ApiError } = await import("../../api/client");
    mocked.listAdminActivity.mockRejectedValue(new ApiError(500, "internal", "database is on fire"));
    renderPage();
    await screen.findByText("database is on fire");
  });
});

describe("Audit — segment, search and range filters", () => {
  it("segment is a client-side predicate: no refetch, rows narrow immediately", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, TODAY_ERR_SYSTEM, YESTERDAY_INFO]));
    renderPage();
    await screen.findByText("host.drain");
    expect(mocked.listAdminActivity).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("tab", { name: "System" }));
    await waitFor(() => {
      expect(screen.queryByText("host.drain")).not.toBeInTheDocument();
    });
    expect(screen.getByText("session.failed")).toBeInTheDocument();
    // Still exactly one network call — the filter is entirely client-side.
    expect(mocked.listAdminActivity).toHaveBeenCalledTimes(1);
  });

  it("the Errors segment count reflects the loaded set", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, TODAY_ERR_SYSTEM, YESTERDAY_INFO]));
    renderPage();
    await screen.findByText("host.drain");
    expect(screen.getByRole("tab", { name: /Errors/ })).toHaveTextContent("1");
  });

  it("a segment that empties the loaded set reads \"No entries match this filter.\", not the range copy", async () => {
    // Only one error row exists; System has zero (both loaded rows have an actor).
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, YESTERDAY_INFO]));
    renderPage();
    await screen.findByText("host.drain");

    fireEvent.click(screen.getByRole("tab", { name: "System" }));
    await screen.findByText("No entries match this filter.");
    expect(screen.queryByText("No entries in this range.")).not.toBeInTheDocument();
  });

  it("search debounces 300ms then re-fetches with q", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));
    renderPage();
    await screen.findByText("host.drain");
    mocked.listAdminActivity.mockClear();
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));

    fireEvent.change(screen.getByPlaceholderText("Filter by actor, action or target"), {
      target: { value: "drain" },
    });
    // Not yet — inside the debounce window.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200);
    });
    expect(mocked.listAdminActivity).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(150);
    });
    await waitFor(() => expect(mocked.listAdminActivity).toHaveBeenCalled());
    const [, opts] = mocked.listAdminActivity.mock.calls.at(-1)!;
    expect(opts).toMatchObject({ q: "drain" });
  });

  it("changing the range re-fetches with a different since", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));
    renderPage();
    await screen.findByText("host.drain");
    const firstSince = mocked.listAdminActivity.mock.calls[0][1]?.since;
    mocked.listAdminActivity.mockClear();
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));

    fireEvent.change(screen.getByLabelText("Time range"), { target: { value: "7d" } });
    await waitFor(() => expect(mocked.listAdminActivity).toHaveBeenCalled());
    const secondSince = mocked.listAdminActivity.mock.calls.at(-1)![1]?.since;
    expect(secondSince).not.toBe(firstSince);
  });
});

describe("Audit — expand / collapse", () => {
  it("clicking a row reveals the detail readout; clicking again hides it", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));
    const { container } = renderPage();
    await screen.findByText("host.drain");

    expect(container.querySelector(".aud-pre")).not.toBeInTheDocument();
    fireEvent.click(screen.getByText("host.drain"));
    expect(container.querySelector(".aud-pre")).toHaveTextContent(/action\s+host\.drain/);

    fireEvent.click(screen.getByText("host.drain"));
    expect(container.querySelector(".aud-pre")).not.toBeInTheDocument();
  });

  it("Expand all opens every visible row and flips to Collapse all", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, TODAY_ERR_SYSTEM]));
    const { container } = renderPage();
    await screen.findByText("host.drain");

    fireEvent.click(screen.getByRole("button", { name: "Expand all" }));
    expect(container.querySelectorAll(".aud-pre")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Collapse all" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Collapse all" }));
    expect(container.querySelectorAll(".aud-pre")).toHaveLength(0);
  });
});

describe("Audit — copy entry", () => {
  // TODAY_WARN's created_at is a fixed local wall-clock time (2026-08-08
  // 09:00:00), and auditTime() is hour12:false — so the composed string is a
  // plain literal, not something reconstructed from Intl at assertion time.
  const EXPECTED_TEXT = [
    "09:00:00  salty2011  host.drain  host h-123456",
    "action  host.drain",
    "target  host h-123456",
    "actor   salty2011",
    "",
    JSON.stringify({ reason: "maintenance" }, null, 2),
  ].join("\n");
  // IconCheck's path `d` — the tick glyph the row's icon button swaps to.
  const TICK_PATH = 'svg path[d="M3.2 8.4l3 3 6.6-7"]';

  it("row copy: writes the composed entry text, swaps the icon to a tick, and reverts after 1200ms", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));
    const { container } = renderPage();
    await screen.findByText("host.drain");

    expect(container.querySelector(`.icon-btn ${TICK_PATH}`)).not.toBeInTheDocument();
    fireEvent.click(screen.getByTitle("Copy entry"));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toBe(EXPECTED_TEXT);
    expect(container.querySelector(`.icon-btn ${TICK_PATH}`)).toBeInTheDocument();

    // Still showing the tick well short of 1200ms (shouldAdvanceTime lets a
    // little real time leak into the fake clock via the `waitFor` above, so
    // this checks comfortably inside the window rather than at its edge).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(600);
    });
    expect(container.querySelector(`.icon-btn ${TICK_PATH}`)).toBeInTheDocument();

    // ...and back to the copy glyph once well past 1200ms total.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });
    expect(container.querySelector(`.icon-btn ${TICK_PATH}`)).not.toBeInTheDocument();
  });

  it("readout copy: the ghost button reads Copied, then reverts to Copy after 1200ms", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));
    renderPage();
    await screen.findByText("host.drain");
    fireEvent.click(screen.getByText("host.drain")); // expand the row

    fireEvent.click(await screen.findByRole("button", { name: "Copy" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0][0]).toBe(EXPECTED_TEXT);
    expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1300);
    });
    expect(screen.getByRole("button", { name: "Copy" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Copied" })).not.toBeInTheDocument();
  });

  it("does not claim a copy when the browser has no clipboard", async () => {
    // An insecure origin has no navigator.clipboard at all; the row used to
    // swap to the tick anyway, telling the operator something was copied.
    Object.assign(navigator, { clipboard: undefined });
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN]));
    const { container } = renderPage();
    await screen.findByText("host.drain");

    fireEvent.click(screen.getByTitle("Copy entry"));
    await act(async () => {});
    expect(container.querySelector(`.icon-btn ${TICK_PATH}`)).not.toBeInTheDocument();
  });
});

describe("Audit — Load more", () => {
  it("appends the next page and keeps the current filters", async () => {
    mocked.listAdminActivity.mockResolvedValueOnce(page([TODAY_WARN], 42));
    renderPage();
    await screen.findByText("host.drain");
    expect(screen.getByRole("button", { name: "Load more" })).toBeInTheDocument();

    mocked.listAdminActivity.mockResolvedValueOnce(page([YESTERDAY_INFO], null));
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));

    await screen.findByText("app.update");
    expect(screen.getByText("host.drain")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
    const [, secondOpts] = mocked.listAdminActivity.mock.calls.at(-1)!;
    expect(secondOpts).toMatchObject({ cursor: 42 });
  });

  it("reuses the same since across the initial load and a cursor page, even if wall-clock time moved on", async () => {
    mocked.listAdminActivity.mockResolvedValueOnce(page([TODAY_WARN], 42));
    renderPage();
    await screen.findByText("host.drain");
    const [, firstOpts] = mocked.listAdminActivity.mock.calls[0];

    // Advance real time between the initial load and the click — a fresh
    // `new Date()` at click time would compute a later `since` for the same
    // "Last 24 hours" range if the anchor were not memoised.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
    });

    mocked.listAdminActivity.mockResolvedValueOnce(page([YESTERDAY_INFO], null));
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("app.update");

    const [, secondOpts] = mocked.listAdminActivity.mock.calls.at(-1)!;
    expect(secondOpts?.since).toBe(firstOpts?.since);
  });

  it("a double-click fetches the cursor page only once", async () => {
    mocked.listAdminActivity.mockResolvedValueOnce(page([TODAY_WARN], 42));
    renderPage();
    await screen.findByText("host.drain");
    mocked.listAdminActivity.mockClear();
    mocked.listAdminActivity.mockResolvedValue(page([YESTERDAY_INFO], null));

    // Both clicks dispatch inside one synchronous act() — React does not get
    // a chance to re-render (and disable the button) between them, so this
    // reproduces the real race a fast double-click causes. Only the
    // synchronous ref check inside the action (not the `disabled` prop,
    // which is state-driven and a render behind) can catch it.
    const loadMoreBtn = screen.getByRole("button", { name: "Load more" });
    act(() => {
      fireEvent.click(loadMoreBtn);
      fireEvent.click(loadMoreBtn);
    });
    await screen.findByText("app.update");

    expect(mocked.listAdminActivity).toHaveBeenCalledTimes(1);
  });
});

/** jsdom's Blob has no `.text()` — stub the constructor itself so the CSV
 *  string handed to it is directly inspectable. */
function stubBlobAndUrl() {
  const BlobStub = vi.fn(function (this: unknown, parts: BlobPart[]) {
    Object.assign(this as object, { parts });
  }) as unknown as typeof Blob;
  vi.stubGlobal("Blob", BlobStub);
  const createObjectURL = vi.fn(() => "blob:mock");
  const revokeObjectURL = vi.fn();
  vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL });
  return { BlobStub, createObjectURL, revokeObjectURL };
}

function csvFromBlobCall(BlobStub: ReturnType<typeof vi.fn>): string {
  const parts = BlobStub.mock.calls[0][0] as BlobPart[];
  return String(parts[0]);
}

describe("Audit — Export CSV", () => {
  it("downloads a CSV built from the currently visible rows", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, TODAY_ERR_SYSTEM, YESTERDAY_INFO]));
    const { BlobStub, createObjectURL } = stubBlobAndUrl();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    renderPage();
    await screen.findByText("host.drain");

    fireEvent.click(screen.getByRole("button", { name: /Export CSV/ }));

    expect(createObjectURL).toHaveBeenCalledTimes(1);
    const lines = csvFromBlobCall(BlobStub as unknown as ReturnType<typeof vi.fn>).trim().split("\n");
    expect(lines[0]).toBe("time,actor,action,target_type,target_id,severity,details");
    expect(lines).toHaveLength(4); // header + 3 rows
    expect(clickSpy).toHaveBeenCalledTimes(1);

    clickSpy.mockRestore();
    vi.unstubAllGlobals();
  });

  it("exports only the segment-filtered rows when a segment is active", async () => {
    mocked.listAdminActivity.mockResolvedValue(page([TODAY_WARN, TODAY_ERR_SYSTEM, YESTERDAY_INFO]));
    const { BlobStub } = stubBlobAndUrl();
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    renderPage();
    await screen.findByText("host.drain");
    fireEvent.click(screen.getByRole("tab", { name: /Errors/ }));
    await waitFor(() => expect(screen.queryByText("host.drain")).not.toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: /Export CSV/ }));
    const lines = csvFromBlobCall(BlobStub as unknown as ReturnType<typeof vi.fn>).trim().split("\n");
    expect(lines).toHaveLength(2); // header + 1 error row

    clickSpy.mockRestore();
    vi.unstubAllGlobals();
  });
});
