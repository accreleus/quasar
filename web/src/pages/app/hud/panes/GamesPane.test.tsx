import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { GamesPane } from "./GamesPane";
import { ApiError } from "../../../../api/client";

const listApps = vi.fn();
const swapSession = vi.fn();
vi.mock("../../../../api/library", () => ({
  listApps: (...a: unknown[]) => listApps(...a),
  swapSession: (...a: unknown[]) => swapSession(...a),
}));
vi.mock("../../../../auth/context", () => ({ useAuth: () => ({ token: "t" }) }));

// NOTE: the brief's fixture used `title`; the real API (`App`/`AppListItem` in
// web/src/api/types.ts) serialises the display name as `name`, not `title` —
// verified against src/api/schema.d.ts and every existing consumer in
// src/pages/app/AppHomeNext.tsx (`app.name`). Using `title` here would test a
// shape the control plane never sends. Field corrected; assertions unchanged.
const apps = [
  { id: "a1", name: "Bench: Ball" },
  { id: "a2", name: "Purple App" },
];

beforeEach(() => {
  listApps.mockReset().mockResolvedValue({ items: apps });
  swapSession.mockReset();
});

describe("GamesPane", () => {
  it("marks the running app and does not offer it as a target", async () => {
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Bench: Ball")).toBeTruthy());
    expect(screen.getByText("PLAYING")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Bench: Ball/ })).toHaveProperty("disabled", true);
  });

  // This component's job stops at the REQUEST now (see the file header):
  // onSwapStart fires the instant a tile is clicked, carrying the id AND name
  // of the app the user asked for — before the request is even sent, and
  // regardless of what the response eventually says. Whether the swap
  // actually succeeds is the OWNER's poll to observe (useSwapTransition), not
  // this component's onSwapStart call.
  it("fires onSwapStart with the target id and name the instant a tile is clicked", async () => {
    swapSession.mockResolvedValue({ session: { id: "s1", app_id: "a1" } });
    const onSwapStart = vi.fn();
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={onSwapStart}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Purple App")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Purple App/ }));
    expect(onSwapStart).toHaveBeenCalledWith({ id: "a2", name: "Purple App" });
  });

  // A successful POST is the request being ACCEPTED, not the swap completing.
  // This component must not report anything further once swapSession resolves
  // — no onSwapDone-equivalent exists any more, so there is nothing left to
  // assert here beyond "it doesn't call onSwapRejected".
  it("reports nothing further once the request is accepted — no premature success signal", async () => {
    swapSession.mockResolvedValue({ session: { id: "s1", app_id: "a1" } });
    const onSwapRejected = vi.fn();
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={onSwapRejected}
      />,
    );
    await waitFor(() => expect(screen.getByText("Purple App")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Purple App/ }));
    await waitFor(() => expect(swapSession).toHaveBeenCalledWith("t", "s1", "a2"));
    expect(onSwapRejected).not.toHaveBeenCalled();
  });

  // The rail reads "what is running" from a prop, so once the owner adopts the
  // swap the badge, the disabled tile and the strip identity all move together.
  it("follows the owner's current app, so the badge moves with the swap", async () => {
    const { rerender } = render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Purple App")).toBeTruthy());
    expect(screen.getByRole("button", { name: /Bench: Ball/ })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: /Purple App/ })).toHaveProperty("disabled", false);

    rerender(
      <GamesPane
        sessionId="s1"
        currentAppId="a2"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    expect(screen.getByRole("button", { name: /Purple App/ })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: /Bench: Ball/ })).toHaveProperty("disabled", false);
    expect(screen.getByText("PLAYING").closest("button")?.getAttribute("aria-label")).toBe("Purple App");
  });

  // The owner is the single source of truth for "a swap is in flight" — every
  // tile (not just the current one) disables, since only one swap can run on
  // a session at a time.
  it("disables every tile while the owner reports a swap pending", async () => {
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Purple App")).toBeTruthy());
    expect(screen.getByRole("button", { name: /Purple App/ })).toHaveProperty("disabled", true);

    fireEvent.click(screen.getByRole("button", { name: /Purple App/ }));
    expect(swapSession).not.toHaveBeenCalled();
  });

  // Defect 2: the running app used to render in library order, so on an
  // 18-app rail it could be the 18th tile — invisible without a horizontal
  // scroll. Sorting it to the front matches the design reference itself
  // (session-overlay-stadia.html's `.qs.current` is literally the first
  // `.qs` in the markup), reads naturally left-to-right, and needs no
  // post-mount scroll call that could fight the drawer's own slide-in
  // animation. Stable sort: every other tile keeps its relative order.
  it("renders the running app first in the rail, regardless of its library-order position", async () => {
    listApps.mockResolvedValue({
      items: [
        { id: "a1", name: "Bench: Ball" },
        { id: "a2", name: "Purple App" },
        { id: "a3", name: "Zephyr" },
      ],
    });
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a3"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Zephyr")).toBeTruthy());

    const names = Array.from(document.querySelectorAll(".qs .nm")).map((el) => el.textContent);
    expect(names[0]).toBe("Zephyr");
    expect(screen.getByText("PLAYING").closest("button")).toBe(screen.getByRole("button", { name: /Zephyr/ }));
  });

  // Swap flow: once the owner adopts the swap (currentAppId prop moves to the
  // newly-current app), that app becomes the one sorted to the front — the
  // rail never gets "stuck" pointing at whichever app was running when it
  // first mounted.
  it("re-sorts to the front when the current app changes after a swap", async () => {
    listApps.mockResolvedValue({
      items: [
        { id: "a1", name: "Bench: Ball" },
        { id: "a2", name: "Purple App" },
        { id: "a3", name: "Zephyr" },
      ],
    });
    const { rerender } = render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Zephyr")).toBeTruthy());
    expect(
      Array.from(document.querySelectorAll(".qs .nm")).map((el) => el.textContent)[0],
    ).toBe("Bench: Ball");

    rerender(
      <GamesPane
        sessionId="s1"
        currentAppId="a3"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    expect(
      Array.from(document.querySelectorAll(".qs .nm")).map((el) => el.textContent)[0],
    ).toBe("Zephyr");
  });

  // Every tile must carry the same `coverClassAt` derivation the library grid
  // uses (libraryGrid.ts), so different apps get visibly different tiles — one
  // hardcoded gradient reads as a bug, not a design choice.
  it("gives different apps different cover-colour classes", async () => {
    listApps.mockResolvedValue({
      items: [
        { id: "a1", name: "Bench: Ball" },
        { id: "a2", name: "Purple App" },
        { id: "a3", name: "Zephyr" },
      ],
    });
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Zephyr")).toBeTruthy());

    const covers = Array.from(document.querySelectorAll<HTMLElement>(".qs .cv"));
    expect(covers).toHaveLength(3);
    const classLists = covers.map((el) =>
      Array.from(el.classList).find((c) => c.startsWith("cv-")),
    );
    // Every tile must carry a cv-* class, and with only 3 apps against a
    // 6-colour palette none should collide.
    expect(classLists.every(Boolean)).toBe(true);
    expect(new Set(classLists).size).toBe(3);
  });

  it("explains a cross-host target instead of surfacing the raw error code", async () => {
    swapSession.mockRejectedValue(new ApiError(409, "home_not_provisioned", "home not provisioned"));
    const onSwapRejected = vi.fn();
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={onSwapRejected}
      />,
    );
    await waitFor(() => expect(screen.getByText("Purple App")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Purple App/ }));
    await waitFor(() =>
      expect(onSwapRejected).toHaveBeenCalledWith(expect.stringMatching(/library lives on another host/i)),
    );
  });

  // Adaptive external resolution: the caption used to promise the resolution
  // "stays as launched", which is wrong once the stream size can be changed
  // mid-session — a user at 720p who swaps keeps 720p, not the launch size.
  it("promises the stream resolution stays as it is NOW, not as launched", async () => {
    render(
      <GamesPane
        sessionId="s1"
        currentAppId="a1"
        swapPending={false}
        onSwapStart={vi.fn()}
        onSwapRejected={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Bench: Ball")).toBeTruthy());
    expect(screen.getByText(/stream resolution stays as it is now/)).toBeTruthy();
    expect(screen.queryByText(/stays as launched/)).toBeNull();
  });
});
