// Resource core — every race the 25 hand-rolled admin loaders got right or
// wrong, asserted once. No DOM, no renderer, no providers: the point of the
// framework-free core is that these are ten-line tests instead of a page render
// with 45 lines of scaffolding and a prayer about timer interleaving.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { createResource } from "./core";
import { ApiError } from "../../api/client";

/** A fetch whose settling this test controls, one call at a time. */
function controllable<T>() {
  const calls: { resolve: (v: T) => void; reject: (e: unknown) => void; signal: AbortSignal }[] = [];
  const fetch = (ctx: { signal: AbortSignal }) =>
    new Promise<T>((resolve, reject) => {
      calls.push({ resolve, reject, signal: ctx.signal });
    });
  return { calls, fetch };
}

/** Let queued .then callbacks run. */
const flush = () => Promise.resolve().then(() => undefined);

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("load lifecycle", () => {
  it("does not fetch before start(), and not at all without a token", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });

    r.start();
    expect(calls).toHaveLength(0); // no token
    expect(r.getState().status).toBe("idle");

    r.setToken("tok");
    await flush();
    expect(calls).toHaveLength(1);
    expect(r.getState().status).toBe("loading");
  });

  it("reports loading before first data and refreshing after it", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();

    expect(r.getState().status).toBe("loading");
    calls[0].resolve(["a"]);
    await flush();
    expect(r.getState()).toMatchObject({ status: "ready", data: ["a"], errorMessage: null });

    void r.refresh();
    expect(r.getState().status).toBe("refreshing");
  });

  it("maps an ApiError to its message and anything else to the label fallback", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "runtime presets", fetch });
    r.setToken("tok");
    r.start();

    calls[0].reject(new ApiError(409, "preset_in_use", "preset is in use"));
    await flush();
    expect(r.getState().errorMessage).toBe("preset is in use");

    void r.refresh();
    calls[1].reject(new TypeError("network down"));
    await flush();
    expect(r.getState().errorMessage).toBe("could not load runtime presets");
  });

  it("refresh() never rejects — a load failure lands in state", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    const settled = r.refresh();
    calls[1].reject(new ApiError(500, "internal", "boom"));
    await expect(settled).resolves.toBeUndefined();
    expect(r.getState().status).toBe("error");
  });

  it("keeps data through a failed refresh and clears the error on recovery (I3)", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a", "b"]);
    await flush();
    const firstLoadAt = r.getState().updatedAt;

    void r.refresh();
    calls[1].reject(new ApiError(502, "internal", "gateway"));
    await flush();
    expect(r.getState()).toMatchObject({ status: "error", data: ["a", "b"], errorMessage: "gateway" });
    expect(r.getState().updatedAt).toBe(firstLoadAt);

    void r.refresh();
    calls[2].resolve(["a"]);
    await flush();
    expect(r.getState()).toMatchObject({ status: "ready", data: ["a"], error: null, errorMessage: null });
  });
});

describe("initialData", () => {
  it("seeds data without claiming to have loaded", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, initialData: [] as string[] });
    r.setToken("tok");
    r.start();

    // Seeded, but still "loading" — a page shows its loading line, not "none yet".
    expect(r.getState()).toMatchObject({ status: "loading", data: [] });
    calls[0].resolve(["a"]);
    await flush();
    expect(r.getState()).toMatchObject({ status: "ready", data: ["a"] });
  });

  it("lets setData land after a failed first load", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, initialData: [] as string[] });
    r.setToken("tok");
    r.start();
    calls[0].reject(new ApiError(500, "internal", "boom"));
    await flush();

    // The drawer saved a row even though the list never loaded — without the
    // seed this write would have been dropped on the floor.
    r.setData((items) => [...items, "saved"]);
    expect(r.getState().data).toEqual(["saved"]);
  });

  it("resets to the seed on a token change", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, initialData: [] as string[] });
    r.setToken("tok-a");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.setToken("tok-b");
    expect(r.getState()).toMatchObject({ status: "loading", data: [] });
  });

  it("does not evaluate a conditional cadence against the seed", async () => {
    const { calls, fetch } = controllable<{ busy: boolean }>();
    const pollMs = vi.fn((data: { busy: boolean }) => (data.busy ? 4000 : null));
    const r = createResource({ label: "images", fetch, pollMs, initialData: { busy: true } });
    r.setToken("tok");
    r.start();

    expect(pollMs).not.toHaveBeenCalled(); // no real data yet
    calls[0].resolve({ busy: false });
    await flush();
    expect(pollMs).toHaveBeenCalledWith({ busy: false });
  });
});

describe("generation guard (I1)", () => {
  it("discards a stale success that settles after a newer load", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();

    void r.refresh(); // supersedes call 0
    calls[1].resolve(["new"]);
    await flush();
    calls[0].resolve(["stale"]);
    await flush();

    expect(r.getState().data).toEqual(["new"]);
  });

  it("discards a stale failure so an aborted request cannot surface an error", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();

    void r.refresh();
    calls[1].resolve(["new"]);
    await flush();
    calls[0].reject(new DOMException("aborted", "AbortError"));
    await flush();

    expect(r.getState()).toMatchObject({ status: "ready", error: null, data: ["new"] });
  });

  it("aborts the superseded request's signal", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();

    expect(calls[0].signal.aborted).toBe(false);
    void r.refresh();
    expect(calls[0].signal.aborted).toBe(true);
    expect(calls[1].signal.aborted).toBe(false);
  });
});

describe("polling (I2)", () => {
  it("polls on a chained timeout and cannot stack behind a slow response", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    vi.advanceTimersByTime(5000); // poll #2 starts
    expect(calls).toHaveLength(2);

    vi.advanceTimersByTime(60_000); // and stays slow for a minute
    expect(calls).toHaveLength(2); // setInterval would have fired 12 more times
  });

  it("does not flip status on a poll tick", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    vi.advanceTimersByTime(5000);
    expect(r.getState().status).toBe("ready"); // never "refreshing"
  });

  it("keeps polling after a failure so a page recovers on its own", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].reject(new ApiError(502, "internal", "gateway"));
    await flush();

    vi.advanceTimersByTime(5000);
    expect(calls).toHaveLength(2);
    calls[1].resolve(["a"]);
    await flush();
    expect(r.getState()).toMatchObject({ status: "ready", errorMessage: null });
  });

  it("re-evaluates a conditional cadence against the freshest data", async () => {
    const { calls, fetch } = controllable<{ busy: boolean }>();
    const r = createResource({
      label: "images",
      fetch,
      pollMs: (data) => (data.busy ? 4000 : null),
    });
    r.setToken("tok");
    r.start();

    calls[0].resolve({ busy: true });
    await flush();
    vi.advanceTimersByTime(4000);
    expect(calls).toHaveLength(2);

    calls[1].resolve({ busy: false }); // work finished — stop polling
    await flush();
    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(2);
  });

  it("pauses while hidden and refetches immediately on resume", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.setVisible(false);
    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(1);

    r.setVisible(true);
    expect(calls).toHaveLength(2); // immediate, silent
    expect(r.getState().status).toBe("ready");
  });

  // The console's "Auto-refresh" switch. Separate from visibility because the
  // operator's choice must outlive a tab switch: turning polling off and then
  // alt-tabbing away and back has to come back off.
  it("schedules no tick while paused, and takes one read on resume", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.pause();
    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(1);

    r.resume();
    expect(calls).toHaveLength(2); // immediate and silent, like setVisible(true)
    expect(r.getState().status).toBe("ready");
    calls[1].resolve(["b"]);
    await flush();

    vi.advanceTimersByTime(5000);
    expect(calls).toHaveLength(3); // and the cadence is back
  });

  it("keeps a paused resource paused across a visibility round-trip", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.pause();
    r.setVisible(false);
    r.setVisible(true); // the browser says "poll again"; the operator said no
    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(1);
  });

  it("still serves an explicit refresh while paused, without re-arming", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.pause();
    void r.refresh();
    expect(calls).toHaveLength(2); // the manual Refresh button keeps working
    calls[1].resolve(["b"]);
    await flush();

    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(2); // but it did not restart the timer
  });

  it("is idempotent in both directions", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.resume(); // never paused — nothing to do, and no stray read
    expect(calls).toHaveLength(1);

    r.pause();
    r.pause();
    r.resume();
    r.resume();
    expect(calls).toHaveLength(2); // exactly one read from the one real resume
  });

  it("treats a repeated visible event as a no-op", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.setVisible(true);
    r.setVisible(true);
    r.setVisible(true);
    expect(calls).toHaveLength(1);
  });
});

describe("mutations (I4)", () => {
  it("applies its cache write and discards the poll that was already in flight", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a", "b"]);
    await flush();

    vi.advanceTimersByTime(5000); // poll #2 in flight, carries the pre-write list
    const done = r.mutate(
      () => Promise.resolve("ok"),
      (items) => items.filter((i) => i !== "b"),
    );
    calls[1].resolve(["a", "b"]); // the stale poll settles
    await done;
    await flush();

    expect(r.getState().data).toEqual(["a"]);
  });

  it("suspends polling until the mutation settles", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    let release!: () => void;
    const done = r.mutate(() => new Promise<void>((res) => (release = () => res())));
    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(1); // no poll while the write is open

    release();
    await done;
    vi.advanceTimersByTime(5000);
    expect(calls).toHaveLength(2);
  });

  it("rejects with the raw error so the caller can toast it", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    const err = new ApiError(409, "home_in_use", "home is in use");
    await expect(r.mutate(() => Promise.reject(err))).rejects.toBe(err);
    // A mutation failure is the caller's business, never load state.
    expect(r.getState()).toMatchObject({ status: "ready", error: null, errorMessage: null });
  });

  it("refuses to run without a token", async () => {
    const { fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.start();
    await expect(r.mutate(() => Promise.resolve("x"))).rejects.toThrow("not authenticated");
  });

  it("setData discards an in-flight load so a stale GET cannot stomp the write", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    void r.refresh();
    r.setData((items) => [...items, "saved"]); // drawer onSaved
    calls[1].resolve(["a"]); // the GET that predates the save
    await flush();

    expect(r.getState().data).toEqual(["a", "saved"]);
  });
});

describe("lifecycle boundaries (I5, I6)", () => {
  it("stop() is silent: no transition, no listener call, no rejection", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    const listener = vi.fn();
    r.subscribe(listener);
    void r.refresh();
    listener.mockClear();

    r.stop();
    calls[1].resolve(["stomp"]);
    await flush();
    vi.advanceTimersByTime(60_000);

    expect(listener).not.toHaveBeenCalled();
    expect(r.getState().data).toEqual(["a"]);
    expect(calls).toHaveLength(2); // timer cancelled
  });

  it("unsubscribing stops delivery", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();

    const listener = vi.fn();
    const off = r.subscribe(listener);
    off();
    calls[0].resolve(["a"]);
    await flush();
    expect(listener).not.toHaveBeenCalled();
  });

  it("a new token clears data and discards the previous principal's response", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok-a");
    r.start();

    r.setToken("tok-b");
    expect(r.getState().data).toBeUndefined();
    expect(r.getState().status).toBe("loading");

    calls[0].resolve(["operator-a-data"]); // arrives late, wrong principal
    await flush();
    expect(r.getState().data).toBeUndefined();

    calls[1].resolve(["operator-b-data"]);
    await flush();
    expect(r.getState().data).toEqual(["operator-b-data"]);
  });

  it("a null token goes idle and stops polling", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch, pollMs: 5000 });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.setToken(null);
    expect(r.getState()).toMatchObject({ status: "idle", data: undefined });
    vi.advanceTimersByTime(60_000);
    expect(calls).toHaveLength(1);
  });

  it("setting the same token again is a no-op", async () => {
    const { calls, fetch } = controllable<string[]>();
    const r = createResource({ label: "things", fetch });
    r.setToken("tok");
    r.start();
    calls[0].resolve(["a"]);
    await flush();

    r.setToken("tok");
    expect(calls).toHaveLength(1);
    expect(r.getState().data).toEqual(["a"]);
  });
});
