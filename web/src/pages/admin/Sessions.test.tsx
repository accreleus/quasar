// Sessions list (handoff §A.2): the poll's lifecycle, the toolbar's three
// narrowings, and the row anatomy.

import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as adminApi from "../../api/admin";
import type { AdminSession, Host } from "../../api/types";
import { ToastProvider } from "../../components/Toast";
import { Sessions } from "./Sessions";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");

const HOSTS = [
  { id: "h1", node_name: "quasar-node-1" },
  { id: "h2", node_name: "quasar-node-2" },
] as Host[];

vi.mock("../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => ({ hosts: HOSTS }),
}));

const mocked = vi.mocked(adminApi);
const POLL_MS = 5000;

function makeSession(overrides: Partial<AdminSession>): AdminSession {
  return {
    id: "00000000-0000-0000-0000-00000000000a",
    user_id: "11111111-1111-1111-1111-111111111111",
    app_id: "app",
    host_id: "h1",
    state: "running",
    state_detail: null,
    created_at: "2024-01-01T00:00:00Z",
    started_at: "2024-01-01T00:00:00Z",
    ended_at: null,
    stream: { width: 1920, height: 1080, fps: 60, codec: "av1" },
    ...overrides,
  } as AdminSession;
}

const RUNNING = makeSession({
  id: "aaaaaaaa-0000-0000-0000-000000000001",
  username: "ada",
  app_name: "Hades II",
  host_name: "quasar-node-1",
  host_id: "h1",
  negotiated_codec: "video/AV1",
  latest_metrics: {
    agent: { source: "agent", ts_unix_ms: 1000, metrics: { bitrate_kbps: 24_000 } },
    browser: { source: "browser", ts_unix_ms: 1000, metrics: { fps: 59.6, rtt_ms: 14 } },
  },
});

const FAILED = makeSession({
  id: "bbbbbbbb-0000-0000-0000-000000000002",
  state: "failed",
  username: "bob",
  app_name: "Blender",
  host_name: "quasar-node-2",
  host_id: "h2",
});

const SLOW = makeSession({
  id: "cccccccc-0000-0000-0000-000000000003",
  username: "cy",
  app_name: "Diablo IV",
  host_name: "quasar-node-2",
  host_id: "h2",
  latest_metrics: {
    browser: { source: "browser", ts_unix_ms: 1000, metrics: { fps: 41, rtt_ms: 84 } },
  },
});

function setVisibility(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, value: hidden });
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    value: hidden ? "hidden" : "visible",
  });
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <Sessions />
      </ToastProvider>
    </MemoryRouter>,
  );
}

async function mount(items: AdminSession[] = []) {
  mocked.listAllSessions.mockResolvedValue({ items, next_cursor: null });
  const view = renderPage();
  await act(async () => {});
  return view;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  mocked.listAllSessions.mockResolvedValue({ items: [], next_cursor: null });
  setVisibility(false);
});

afterEach(() => {
  vi.useRealTimers();
});

describe("Sessions — polling", () => {
  it("polls once per interval", async () => {
    await mount();
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(POLL_MS);
    });
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(2);
  });

  it("stops polling while hidden and resumes when visible", async () => {
    await mount();

    setVisibility(true);
    mocked.listAllSessions.mockClear();
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS * 3);
    });
    expect(mocked.listAllSessions).not.toHaveBeenCalled();

    setVisibility(false);
    await act(async () => {});
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(1);
  });

  it("clears its timer on unmount", async () => {
    const { unmount } = await mount();
    unmount();
    mocked.listAllSessions.mockClear();
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS * 3);
    });
    expect(mocked.listAllSessions).not.toHaveBeenCalled();
  });

  it("holds the timer down while Auto-refresh is off, and takes it back up", async () => {
    await mount();
    const toggle = screen.getByRole("switch", { name: "Auto-refresh" });
    expect(toggle).toHaveAttribute("aria-checked", "true");

    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(toggle).toHaveAttribute("aria-checked", "false");

    mocked.listAllSessions.mockClear();
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS * 4);
    });
    expect(mocked.listAllSessions).not.toHaveBeenCalled();

    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(1); // one read on resume
  });

  it("keeps Auto-refresh off across a tab switch", async () => {
    await mount();
    await act(async () => {
      fireEvent.click(screen.getByRole("switch", { name: "Auto-refresh" }));
    });

    mocked.listAllSessions.mockClear();
    setVisibility(true);
    setVisibility(false); // the browser says "poll again"; the operator said no
    await act(async () => {
      vi.advanceTimersByTime(POLL_MS * 3);
    });
    expect(mocked.listAllSessions).not.toHaveBeenCalled();
  });

  it("still serves the Refresh button while Auto-refresh is off", async () => {
    await mount();
    await act(async () => {
      fireEvent.click(screen.getByRole("switch", { name: "Auto-refresh" }));
    });

    mocked.listAllSessions.mockClear();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    });
    expect(mocked.listAllSessions).toHaveBeenCalledTimes(1);
  });
});

describe("Sessions — segments", () => {
  it("gives every segment its own wire filter, so no page can hide a row", async () => {
    await mount([RUNNING, FAILED]);
    expect(mocked.listAllSessions).toHaveBeenLastCalledWith("tok", undefined, { state: "all" });

    await act(async () => {
      fireEvent.click(screen.getByRole("tab", { name: "Live sessions" }));
    });
    expect(mocked.listAllSessions).toHaveBeenLastCalledWith("tok", undefined, { state: "active" });

    await act(async () => {
      fireEvent.click(screen.getByRole("tab", { name: "Failed sessions" }));
    });
    expect(mocked.listAllSessions).toHaveBeenLastCalledWith("tok", undefined, { state: "failed" });
  });

  it("renders the failed page the server returned, without re-deriving it", async () => {
    await mount([RUNNING, FAILED]);
    mocked.listAllSessions.mockResolvedValue({ items: [FAILED], next_cursor: null });

    await act(async () => {
      fireEvent.click(screen.getByRole("tab", { name: "Failed sessions" }));
    });
    expect(screen.getByText("Blender")).toBeInTheDocument();
    expect(screen.queryByText("Hades II")).not.toBeInTheDocument();
  });

  it("counts each segment on the control", async () => {
    await mount([RUNNING, FAILED, SLOW]);
    expect(within(screen.getByRole("tab", { name: "All sessions" })).getByText("3")).toBeInTheDocument();
    expect(within(screen.getByRole("tab", { name: "Live sessions" })).getByText("2")).toBeInTheDocument();
    expect(within(screen.getByRole("tab", { name: "Failed sessions" })).getByText("1")).toBeInTheDocument();
  });

  it("holds the other counts from the last unfiltered read rather than blanking them", async () => {
    await mount([RUNNING, FAILED, SLOW]);
    mocked.listAllSessions.mockResolvedValue({ items: [RUNNING, SLOW], next_cursor: null });
    await act(async () => {
      fireEvent.click(screen.getByRole("tab", { name: "Live sessions" }));
    });

    // Live is counted from the page just fetched; All and Failed survive from
    // the `all` read, because a blank there reads as "none".
    expect(within(screen.getByRole("tab", { name: "Live sessions" })).getByText("2")).toBeInTheDocument();
    expect(within(screen.getByRole("tab", { name: "All sessions" })).getByText("3")).toBeInTheDocument();
    expect(within(screen.getByRole("tab", { name: "Failed sessions" })).getByText("1")).toBeInTheDocument();
  });
});

describe("Sessions — filters", () => {
  it("narrows on user, app or host", async () => {
    await mount([RUNNING, FAILED, SLOW]);
    const search = screen.getByLabelText("Filter by user, app or host");

    await act(async () => {
      fireEvent.change(search, { target: { value: "diablo" } });
    });
    expect(screen.getByText("Diablo IV")).toBeInTheDocument();
    expect(screen.queryByText("Hades II")).not.toBeInTheDocument();
  });

  it("narrows on a host, and offers every fleet host", async () => {
    await mount([RUNNING, FAILED, SLOW]);
    const select = screen.getByLabelText("Filter by host");
    expect(within(select).getByRole("option", { name: "node-2" })).toBeInTheDocument();

    await act(async () => {
      fireEvent.change(select, { target: { value: "h2" } });
    });
    expect(screen.queryByText("Hades II")).not.toBeInTheDocument();
    expect(screen.getByText("Diablo IV")).toBeInTheDocument();
  });

  it("says so when nothing matches", async () => {
    await mount([RUNNING]);
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Filter by user, app or host"), {
        target: { value: "nothing" },
      });
    });
    expect(screen.getByText("No sessions")).toBeInTheDocument();
    expect(screen.getByText("Sessions appear here once users start streaming.")).toBeInTheDocument();
  });
});

describe("Sessions — rows", () => {
  it("renders the mock's cells off latest_metrics", async () => {
    await mount([RUNNING]);
    const row = screen.getByText("Hades II").closest("tr")!;

    expect(within(row).getByText("aaaaaaaa")).toBeInTheDocument();
    expect(within(row).getByText("ada")).toBeInTheDocument();
    expect(within(row).getByText("11111111")).toBeInTheDocument(); // user id sub-label
    expect(within(row).getByText("node-1")).toBeInTheDocument();
    expect(within(row).getByText("60")).toBeInTheDocument(); // browser fps, rounded
    expect(within(row).getByText("14 ms")).toBeInTheDocument(); // browser rtt
    expect(within(row).getByText("24.0 Mb/s")).toBeInTheDocument(); // agent bitrate
    expect(within(row).getByText("AV1")).toBeInTheDocument();
  });

  it("names the state in the sub-label only when it is not simply running", async () => {
    await mount([RUNNING, FAILED]);
    expect(screen.getByText(/bbbbbbbb · failed/)).toBeInTheDocument();
    expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
  });

  it("carries the full ids as titles, for log correlation", async () => {
    await mount([RUNNING]);
    const cells = screen.getByText("Hades II").closest("tr")!.children;
    expect(cells[0]).toHaveAttribute("title", RUNNING.id);
    expect(cells[1]).toHaveAttribute("title", RUNNING.user_id);
    expect(cells[2]).toHaveAttribute("title", "h1");
  });

  it("falls back to a truncated app id when the app row is gone", async () => {
    await mount([makeSession({ id: "dddddddd-0000-0000-0000-000000000004", app_id: "app-99999999", app_name: undefined })]);
    expect(screen.getByText("app-9999")).toBeInTheDocument();
  });

  it("says nothing rather than zero where the wire reported nothing", async () => {
    await mount([FAILED]);
    const row = screen.getByText("Blender").closest("tr")!;
    expect(within(row).getAllByText("—").length).toBeGreaterThanOrEqual(3);
  });

  it("offers Terminate on a live session and not on a terminal one", async () => {
    await mount([RUNNING, FAILED]);

    fireEvent.click(screen.getByRole("button", { name: "Actions for Hades II" }));
    expect(screen.getByRole("menuitem", { name: "Terminate" })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });

    fireEvent.click(screen.getByRole("button", { name: "Actions for Blender" }));
    expect(screen.queryByRole("menuitem", { name: "Terminate" })).not.toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Open" })).toBeInTheDocument();
  });

  it("confirms before terminating, then refreshes so the row is not stale", async () => {
    await mount([RUNNING]);
    mocked.forceStopSession.mockResolvedValue(undefined);

    fireEvent.click(screen.getByRole("button", { name: "Actions for Hades II" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Terminate" }));
    expect(mocked.forceStopSession).not.toHaveBeenCalled();

    mocked.listAllSessions.mockClear();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Terminate" }));
    });
    expect(mocked.forceStopSession).toHaveBeenCalledWith("tok", RUNNING.id);
    expect(mocked.listAllSessions).toHaveBeenCalled();
  });
});

/** The username line of each row, in render order. */
function userNames(): (string | null)[] {
  return screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.children[1].querySelector(".primary")?.textContent ?? null);
}

describe("Sessions — sorting", () => {
  it("sorts by user, and reverses on a second click", async () => {
    await mount([SLOW, RUNNING, FAILED]); // cy, ada, bob
    const header = screen.getByRole("button", { name: /^User/ });

    await act(async () => {
      fireEvent.click(header);
    });
    expect(userNames()).toEqual(["ada", "bob", "cy"]);

    await act(async () => {
      fireEvent.click(header);
    });
    expect(userNames()).toEqual(["cy", "bob", "ada"]);
  });
});
