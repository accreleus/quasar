// useLibraryExtras: the app list's per-title fields joined with the caller's
// play history (amendment 38), null until both load and after either fails.

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import * as libraryApi from "../../../api/library";
import type { App } from "../../../api/types";
import { LibraryCatalogProvider, useLibraryCatalog } from "../libraryCatalog";
import { buildExtras, useLibraryExtras } from "./libraryExtras";

vi.mock("../../../api/library");
const mocked = vi.mocked(libraryApi);

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.c", username: "u", role: "user" } as never,
  token: "tok",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

const portal = {
  id: "a1",
  name: "Portal 2",
  added_at: "2026-08-01T12:00:00Z",
  genres: ["Action", "Adventure"],
  developer: "Valve",
  publisher: "Valve",
  release_date: "18 Apr, 2011",
  controller_support: "full",
  logo_url: "/v1/artwork/l.png",
  icon_url: "/v1/artwork/i.png",
} as unknown as App;
// An older control plane: none of the amendment 38 fields.
const bare = { id: "a2", name: "Bare" } as unknown as App;
const history = [{ app_id: "a1", last_played_at: "2026-10-15T21:04:10Z", total_seconds: 151200, sessions: 42 }];

function wrapper({ children }: { children: ReactNode }) {
  return (
    <AuthContext.Provider value={auth}>
      <LibraryCatalogProvider>{children}</LibraryCatalogProvider>
    </AuthContext.Provider>
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("buildExtras", () => {
  it("joins the app fields with the play history", () => {
    expect(buildExtras([portal, bare], history)).toEqual({
      a1: {
        genres: ["Action", "Adventure"],
        added_at: "2026-08-01T12:00:00Z",
        play: { last_played_at: "2026-10-15T21:04:10Z", total_seconds: 151200, sessions: 42 },
        icon_url: "/v1/artwork/i.png",
        logo_url: "/v1/artwork/l.png",
        developer: "Valve",
        publisher: "Valve",
        release_date: "18 Apr, 2011",
        controller_support: "full",
      },
      a2: {
        genres: [],
        added_at: null,
        play: null,
        icon_url: null,
        logo_url: null,
        developer: null,
        publisher: null,
        release_date: null,
        controller_support: null,
      },
    });
  });
});

describe("useLibraryExtras", () => {
  it("is the joined map once both reads load, from one shared fetch each", async () => {
    mocked.listApps.mockResolvedValue({ items: [portal, bare], next_cursor: null });
    mocked.getPlayHistory.mockResolvedValue({ items: history });
    const { result } = renderHook(() => [useLibraryExtras(), useLibraryExtras()] as const, { wrapper });
    await waitFor(() => expect(result.current[0]).not.toBeNull());
    expect(result.current[0]?.a1.play?.sessions).toBe(42);
    expect(result.current[1]).toEqual(result.current[0]);
    expect(mocked.listApps).toHaveBeenCalledTimes(1);
    expect(mocked.getPlayHistory).toHaveBeenCalledTimes(1);
  });

  it("is null while the play history loads", async () => {
    mocked.listApps.mockResolvedValue({ items: [portal], next_cursor: null });
    mocked.getPlayHistory.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => ({ x: useLibraryExtras(), c: useLibraryCatalog() }), { wrapper });
    await waitFor(() => expect(result.current.c.settled).toBe(true));
    expect(result.current.x).toBeNull();
  });

  it("is null when the play history fails, as on a control plane without the route", async () => {
    mocked.listApps.mockResolvedValue({ items: [portal], next_cursor: null });
    mocked.getPlayHistory.mockRejectedValue(new Error("404"));
    const { result } = renderHook(() => ({ x: useLibraryExtras(), c: useLibraryCatalog() }), { wrapper });
    await waitFor(() => expect(result.current.c.settled).toBe(true));
    await waitFor(() => expect(mocked.getPlayHistory).toHaveBeenCalled());
    await act(async () => {});
    expect(result.current.x).toBeNull();
  });

  it("is null when the library fails", async () => {
    mocked.listApps.mockRejectedValue(new Error("boom"));
    mocked.getPlayHistory.mockResolvedValue({ items: history });
    const { result } = renderHook(() => ({ x: useLibraryExtras(), c: useLibraryCatalog() }), { wrapper });
    await waitFor(() => expect(result.current.c.error).not.toBeNull());
    expect(result.current.x).toBeNull();
  });

  it("is null outside a catalogue provider", () => {
    const { result } = renderHook(() => useLibraryExtras(), {
      wrapper: ({ children }) => <AuthContext.Provider value={auth}>{children}</AuthContext.Provider>,
    });
    expect(result.current).toBeNull();
  });

  it("reloads the play history with the library", async () => {
    mocked.listApps.mockResolvedValue({ items: [portal], next_cursor: null });
    mocked.getPlayHistory.mockResolvedValue({ items: [] });
    const { result } = renderHook(() => ({ x: useLibraryExtras(), c: useLibraryCatalog() }), { wrapper });
    await waitFor(() => expect(result.current.x).not.toBeNull());
    expect(result.current.x?.a1.play).toBeNull();

    mocked.getPlayHistory.mockResolvedValue({ items: history });
    act(() => result.current.c.reload());
    await waitFor(() => expect(result.current.x?.a1.play?.total_seconds).toBe(151200));
    expect(mocked.listApps).toHaveBeenCalledTimes(2);
  });
});
