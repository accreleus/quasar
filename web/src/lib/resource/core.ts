/**
 * Resource — the admin pages' shared load / poll / cancel / error / mutate
 * state machine. No React imports: a plain subscribe/getState object,
 * unit-testable with fake timers; `useResource` (./react.ts) is the only
 * React-aware code.
 *
 * The invariants callers depend on:
 *
 *   I1  Latest generation wins. Every load, mutation and local write bumps a
 *       counter; a load applies (success or failure) only if its generation is
 *       still current. Aborting is network hygiene, never the correctness
 *       mechanism — api/admin.ts functions that ignore `signal` stay safe.
 *   I2  Polls cannot stack. No setInterval: the next poll is a setTimeout
 *       armed in the previous load's settle path, so overlapping timers are
 *       unrepresentable.
 *   I3  Errors never destroy data. A failed refresh/poll records the error and
 *       keeps the last good `data`; the next success clears it.
 *   I4  Mutations beat polls. mutate() supersedes the in-flight load, suspends
 *       polling, and applies its cache write against current data — a GET that
 *       started before the write can never stomp it.
 *   I5  stop() is silent. Aborts, cancels the timer, keeps state, notifies
 *       nobody — no transition, no rejection on unmount mid-flight.
 *   I6  A token change is a principal boundary: in-flight work discarded,
 *       data + error cleared.
 *
 * Error routing is deliberately asymmetric: load failures belong to state (an
 * inline banner), mutation failures to the caller (a toast) — mutate() rejects
 * raw, refresh() never rejects.
 */

import { ApiError } from "../../api/client";

/** Everything a fetch or a mutation is handed. `token` is non-null: a resource
 *  with no token neither loads nor mutates. */
export interface FetchCtx<T = unknown> {
  token: string;
  signal: AbortSignal;
  /**
   * The data currently held, if any. For composed reads where one part is
   * best-effort: catch that part's failure and carry `current`'s value
   * forward, so a transient failure in a secondary call does not blank a
   * header that was fine a second ago.
   *
   * Do not use it to accumulate (a feed, a paged list) — this models the
   * current value of one resource, not a growing prefix.
   */
  current: T | undefined;
}

export interface ResourceSpec<T> {
  /** Used for the default failure message: `could not load ${label}`. */
  label: string;
  /**
   * One logical read. May compose several HTTP calls (a Promise.all fan-out, a
   * dependent chain) — compose inside here rather than splitting into two
   * resources when the page wants one timer and one error surface. Throw to
   * fail; return partial data only when partiality is part of `T`.
   */
  fetch: (ctx: FetchCtx<T>) => Promise<T>;
  /**
   * Poll cadence in ms, re-evaluated after every settled load. Omitted or null
   * means no polling. The function form makes conditional polling declarative
   * (`data => data.images.some(inFlight) ? 4000 : null`).
   *
   * Note the asymmetry after a failure with no data yet: a numeric cadence
   * still re-arms (so a page that failed its first load recovers on its own),
   * but the function form cannot be evaluated without data and stops. Such a
   * resource recovers on an explicit refresh() or a visibility resume.
   */
  pollMs?: number | ((data: T) => number | null | undefined);
  /**
   * Seed value, for list resources that want `[]` rather than undefined before
   * the first load. It is not "data": `status` still reports "loading", so a
   * page shows its loading line, not an empty state.
   *
   * It also closes a real gap. `setData` cannot write into a resource that has
   * no data, so with no seed a drawer's onSaved after a FAILED initial load
   * would silently drop the saved row. With a seed the write always lands.
   */
  initialData?: T;
}

export type ResourceStatus = "idle" | "loading" | "refreshing" | "ready" | "error";

export interface ResourceState<T> {
  status: ResourceStatus;
  /** Last successfully applied data. Survives refresh/poll failures (I3);
   *  cleared only by a token change (I6). */
  data: T | undefined;
  /** The raw thrown error of the most recent failed load — kept unwrapped so a
   *  page can branch on `ApiError.code` / `.status`. Null after any success. */
  error: unknown;
  /** `error instanceof ApiError ? error.message : "could not load <label>"` —
   *  the exact derivation every page wrote by hand. */
  errorMessage: string | null;
  /** Epoch ms of the last applied load, null before the first. */
  updatedAt: number | null;
  /** Monotonic, bumped by every applied load and every local write. Exposed
   *  for tests and debugging, not for rendering. */
  generation: number;
}

export interface Resource<T> {
  // ── read side (useSyncExternalStore contract) ───────────────────────────
  /** Stable between transitions, replaced on each one. */
  getState(): ResourceState<T>;
  /** Fires synchronously after each transition. Unsubscribing is idempotent
   *  and a listener never fires after its unsubscribe returns. */
  subscribe(listener: () => void): () => void;

  // ── lifecycle ───────────────────────────────────────────────────────────
  /** Begin. Kicks the first load when a token is present and arms polling.
   *  Idempotent. */
  start(): void;
  /** Abort in-flight work and cancel the poll timer, keeping state and
   *  notifying nobody (I5). Idempotent. */
  stop(): void;
  /** Same token is a no-op. A different token discards in-flight work, clears
   *  data and error, and reloads if started. Null goes idle (I6). */
  setToken(token: string | null): void;
  /** Page-visibility integration. False cancels the poll timer (an in-flight
   *  load still applies); true triggers one silent refresh, after which polling
   *  resumes from that load's settle. Repeated true is a no-op while a load is
   *  already in flight for it. */
  setVisible(visible: boolean): void;
  /**
   * Suspend polling on the operator's say-so (an "Auto-refresh" switch), a
   * separate axis from setVisible: a paused resource must stay paused across a
   * tab switch. Only the timer stops — data, errors, in-flight loads and
   * refresh() all survive. Idempotent.
   */
  pause(): void;
  /** Undo pause(): take one silent read now, and re-arm polling from its
   *  settle — the same shape as becoming visible again. Idempotent. */
  resume(): void;

  // ── imperative ──────────────────────────────────────────────────────────
  /**
   * Load now. `silent` (the poll's own mode) never touches `status`; the
   * default shows "loading" before first data and "refreshing" after it.
   * Resolves when this load applies, fails, or is superseded — and never
   * rejects: load failures land in state, not on the caller.
   */
  refresh(opts?: { silent?: boolean }): Promise<void>;
  /**
   * Run a write. Rejects with the raw error so the caller can toast it.
   * `apply` is the post-success cache write (`items => items.filter(...)`),
   * skipped when there is no data yet.
   *
   * Concurrent mutations are allowed and are not serialised — pages already
   * gate their buttons — and they apply in resolution order. A mutation never
   * auto-revalidates; chain `refresh({ silent: true })` when the server's own
   * view is wanted afterwards.
   */
  mutate<R>(run: (ctx: FetchCtx<T>) => Promise<R>, apply?: (data: T, result: R) => T): Promise<R>;
  /** Local cache write with a server-confirmed value (a drawer's onSaved).
   *  Bumps the generation, so a GET already in flight is discarded rather than
   *  allowed to stomp the fresher write. No-op before first data. */
  setData(updater: (data: T) => T): void;
}

function friendly(error: unknown, label: string): string {
  return error instanceof ApiError ? error.message : `could not load ${label}`;
}

export function createResource<T>(spec: ResourceSpec<T>): Resource<T> {
  let state: ResourceState<T> = {
    status: "idle",
    data: spec.initialData,
    error: null,
    errorMessage: null,
    updatedAt: null,
    generation: 0,
  };

  const listeners = new Set<() => void>();
  let gen = 0;
  let inflight: AbortController | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let started = false;
  let visible = true;
  let paused = false;
  let token: string | null = null;
  let mutations = 0;
  /** Whether a load has ever applied. Distinct from `data != null`, which a
   *  seeded `initialData` would make true before anything was fetched. */
  let loaded = false;

  function set(next: Partial<ResourceState<T>>): void {
    state = { ...state, ...next };
    for (const listener of [...listeners]) listener();
  }

  function clearTimer(): void {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  }

  /** I2 — the only place a poll is scheduled, and only ever from a settle. */
  function armPoll(): void {
    clearTimer();
    if (!started || !visible || paused || mutations > 0) return;
    const ms =
      typeof spec.pollMs === "function"
        ? !loaded || state.data === undefined
          ? null
          : spec.pollMs(state.data)
        : spec.pollMs;
    if (!ms || ms <= 0) return;
    timer = setTimeout(() => {
      timer = null;
      void load(true);
    }, ms);
  }

  function load(silent: boolean): Promise<void> {
    if (!started || token === null) return Promise.resolve();

    const g = ++gen; // I1 — anything already in flight is now stale
    inflight?.abort();
    const controller = new AbortController();
    inflight = controller;
    clearTimer();

    if (!silent) {
      set({ status: loaded ? "refreshing" : "loading" });
    }

    const ctx: FetchCtx<T> = { token, signal: controller.signal, current: state.data };
    return spec.fetch(ctx).then(
      (data) => {
        if (g !== gen) return; // superseded — discard, including the abort case
        loaded = true;
        set({
          status: "ready",
          data,
          error: null,
          errorMessage: null,
          updatedAt: Date.now(),
          generation: g,
        });
        armPoll();
      },
      (error: unknown) => {
        if (g !== gen) return;
        // I3 — data and updatedAt survive; the page keeps its rows.
        set({ status: "error", error, errorMessage: friendly(error, spec.label) });
        armPoll();
      },
    );
  }

  return {
    getState: () => state,

    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },

    start() {
      if (started) return;
      started = true;
      void load(false);
    },

    stop() {
      if (!started) return;
      started = false;
      clearTimer();
      inflight?.abort();
      inflight = null;
      gen++; // I5 — nothing already in flight may apply
    },

    setToken(next) {
      if (next === token) return;
      token = next;
      gen++;
      inflight?.abort();
      inflight = null;
      clearTimer();
      loaded = false;
      // I6 — a new principal starts from nothing (bar the caller's seed).
      set({
        status: next === null ? "idle" : "loading",
        data: spec.initialData,
        error: null,
        errorMessage: null,
        updatedAt: null,
      });
      if (next !== null && started) void load(false);
    },

    setVisible(next) {
      if (next === visible) return;
      visible = next;
      if (!visible) {
        clearTimer();
        return;
      }
      // Paused is the operator's axis and outranks the browser's: coming back
      // to a tab whose Auto-refresh is off must not fetch.
      if (started && token !== null && !paused) void load(true);
    },

    pause() {
      if (paused) return;
      paused = true;
      clearTimer();
    },

    resume() {
      if (!paused) return;
      paused = false;
      // Mirrors setVisible(true): the read comes first, and armPoll runs from
      // its settle, so resuming can never leave two timers behind.
      if (started && token !== null && visible) void load(true);
      else armPoll();
    },

    refresh(opts) {
      return load(opts?.silent ?? false);
    },

    async mutate<R>(run: (ctx: FetchCtx<T>) => Promise<R>, apply?: (data: T, result: R) => T): Promise<R> {
      if (token === null) throw new Error("not authenticated");
      // I4 — the write wins over anything the poll has in flight.
      gen++;
      inflight?.abort();
      inflight = null;
      clearTimer();
      // The superseded load will never settle into state, so a visible
      // "refreshing" would otherwise stick until the next poll.
      if (state.status === "refreshing") set({ status: "ready" });
      mutations++;
      try {
        const result = await run({
          token,
          signal: new AbortController().signal,
          current: state.data,
        });
        if (apply && state.data !== undefined) {
          set({ data: apply(state.data, result), generation: ++gen });
        }
        return result;
      } finally {
        mutations--;
        if (mutations === 0) armPoll();
      }
    },

    setData(updater) {
      if (state.data === undefined) return;
      gen++; // a GET that predates this write must not stomp it
      inflight?.abort();
      inflight = null;
      set({ data: updater(state.data), generation: gen });
      armPoll();
    },
  };
}
