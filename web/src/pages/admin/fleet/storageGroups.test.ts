import { describe, expect, it } from "vitest";
import {
  distinctHostCount,
  distinctProviders,
  groupHomesByUser,
  homeState,
  isAppOrphaned,
  isHostOrphaned,
  NO_USER_KEY,
} from "./storageGroups";
import type { AdminHome } from "../../../api/types";

function makeHome(overrides: Partial<AdminHome>): AdminHome {
  return {
    id: "home-id",
    user_id: "user-1",
    app_id: "app-1",
    host_id: "host-1",
    username: "alice",
    app_name: "Steam",
    host_name: "tower",
    provider: "local",
    ref: "quasar-home-user-1-app-1",
    bytes_used: 0,
    created_at: "2026-01-01T00:00:00Z",
    last_used_at: "2026-01-01T00:00:00Z",
    gc_after: null,
    ...overrides,
  };
}

describe("groupHomesByUser", () => {
  it("returns an empty list for no homes", () => {
    expect(groupHomesByUser([])).toEqual([]);
  });

  it("aggregates total bytes and home count per user", () => {
    const homes = [
      makeHome({ id: "h1", user_id: "u-alice", username: "alice", app_id: "a1", app_name: "Steam", bytes_used: 100 }),
      makeHome({ id: "h2", user_id: "u-alice", username: "alice", app_id: "a2", app_name: "Portal 2", bytes_used: 50 }),
      makeHome({ id: "h3", user_id: "u-bob", username: "bob", app_id: "a1", app_name: "Steam", bytes_used: 10 }),
    ];
    const groups = groupHomesByUser(homes);
    expect(groups).toHaveLength(2);
    const alice = groups.find((g) => g.userId === "u-alice")!;
    expect(alice.username).toBe("alice");
    expect(alice.totalBytes).toBe(150);
    expect(alice.homes).toHaveLength(2);
  });

  it("sorts real-user groups by total bytes descending", () => {
    const homes = [
      makeHome({ id: "h1", user_id: "u-small", username: "small", bytes_used: 10 }),
      makeHome({ id: "h2", user_id: "u-big", username: "big", bytes_used: 1000 }),
      makeHome({ id: "h3", user_id: "u-mid", username: "mid", bytes_used: 500 }),
    ];
    const order = groupHomesByUser(homes).map((g) => g.username);
    expect(order).toEqual(["big", "mid", "small"]);
  });

  it("sorts a group's own homes by bytes_used descending", () => {
    const homes = [
      makeHome({ id: "h1", user_id: "u1", app_name: "small-app", bytes_used: 5 }),
      makeHome({ id: "h2", user_id: "u1", app_name: "big-app", bytes_used: 500 }),
    ];
    const [group] = groupHomesByUser(homes);
    expect(group.homes.map((h) => h.app_name)).toEqual(["big-app", "small-app"]);
  });

  it("buckets homes with no linked user under NO_USER_KEY and pins it last", () => {
    const homes = [
      // Enormous orphaned total — must NOT outrank the real user below.
      makeHome({ id: "h1", user_id: null, username: null, app_id: null, app_name: null, bytes_used: 999_999 }),
      makeHome({ id: "h2", user_id: "u-real", username: "real-user", bytes_used: 1 }),
    ];
    const groups = groupHomesByUser(homes);
    expect(groups.map((g) => g.key)).toEqual(["u-real", NO_USER_KEY]);
    expect(groups[1].totalBytes).toBe(999_999);
    expect(groups[1].username).toBeNull();
  });

  it("omits the NO_USER_KEY bucket entirely when every home has a user", () => {
    const homes = [makeHome({ id: "h1", user_id: "u1" })];
    expect(groupHomesByUser(homes).some((g) => g.key === NO_USER_KEY)).toBe(false);
  });
});

describe("orphan predicates", () => {
  it("isAppOrphaned is true only when app_id is null", () => {
    expect(isAppOrphaned(makeHome({ app_id: null }))).toBe(true);
    expect(isAppOrphaned(makeHome({ app_id: "a1" }))).toBe(false);
  });

  it("isHostOrphaned is true only when host_id is null", () => {
    expect(isHostOrphaned(makeHome({ host_id: null }))).toBe(true);
    expect(isHostOrphaned(makeHome({ host_id: "h1" }))).toBe(false);
  });

  it("an orphaned app still counts its bytes in the user's group total", () => {
    const homes = [
      makeHome({ id: "h1", user_id: "u1", username: "alice", app_id: "a1", app_name: "Steam", bytes_used: 100 }),
      makeHome({ id: "h2", user_id: "u1", username: "alice", app_id: null, app_name: null, bytes_used: 250 }),
    ];
    const [group] = groupHomesByUser(homes);
    expect(group.totalBytes).toBe(350);
    expect(group.homes).toHaveLength(2);
    expect(group.homes.some(isAppOrphaned)).toBe(true);
  });
});

describe("homeState", () => {
  it("is Active when nothing is wrong", () => {
    expect(homeState(makeHome({}))).toEqual({ label: "Active", variant: "success" });
  });

  it("is Pending cleanup once gc_after is set, even over an orphan fact", () => {
    expect(homeState(makeHome({ gc_after: "2026-01-02T00:00:00Z", app_id: null }))).toEqual({
      label: "Pending cleanup",
      variant: "warning",
    });
  });

  it("is No linked user when user_id is null", () => {
    expect(homeState(makeHome({ user_id: null, username: null }))).toEqual({
      label: "No linked user",
      variant: "neutral",
    });
  });

  it("is App deleted when app_id is null but a user is still linked", () => {
    expect(homeState(makeHome({ app_id: null, app_name: null }))).toEqual({
      label: "App deleted",
      variant: "neutral",
    });
  });

  it("is Host deleted when host_id is null but app and user are intact", () => {
    expect(homeState(makeHome({ host_id: null, host_name: null }))).toEqual({
      label: "Host deleted",
      variant: "neutral",
    });
  });
});

describe("distinctProviders", () => {
  it("dedupes and sorts", () => {
    const homes = [
      makeHome({ id: "h1", provider: "local" }),
      makeHome({ id: "h2", provider: "auto" }),
      makeHome({ id: "h3", provider: "local" }),
    ];
    expect(distinctProviders(homes)).toEqual(["auto", "local"]);
  });

  it("is empty for no homes", () => {
    expect(distinctProviders([])).toEqual([]);
  });
});

describe("distinctHostCount", () => {
  it("counts distinct non-null host ids", () => {
    const homes = [
      makeHome({ id: "h1", host_id: "host-a" }),
      makeHome({ id: "h2", host_id: "host-b" }),
      makeHome({ id: "h3", host_id: "host-a" }),
      makeHome({ id: "h4", host_id: null, host_name: null }),
    ];
    expect(distinctHostCount(homes)).toBe(2);
  });
});
