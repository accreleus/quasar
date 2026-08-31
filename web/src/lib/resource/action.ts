/**
 * useAdminAction — one write, its pending state, and its toast.
 *
 * The counterpart to useResource: loads belong to the resource, writes belong
 * here. Both halves of the pattern the admin pages hand-rolled ~60 times over:
 *
 *     setSaving(true);
 *     try { await adminApi.doThing(token, id); onOk(); addToast({variant:"success", …}); }
 *     catch (e) { addToast({variant:"danger", title: e instanceof ApiError ? e.message : "…"}); }
 *     finally { setSaving(false); }
 *
 * The per-page failure copy is preserved deliberately — `failure` is required,
 * and its function form receives the raw error so a page can still branch on
 * `ApiError.code` (a 409 `home_in_use` wants different copy from a 500).
 */

import { useCallback, useRef, useState } from "react";
import { ApiError } from "../../api/client";
import { useToast } from "../../components/Toast";

/** Toast copy: a title, or a title with a second line. Several admin writes
 *  put the summary in the title and the server's explanation in the body
 *  (a job's eta_note, a mapped error code), so both shapes are first-class. */
export type ToastCopy = string | { title: string; body?: string };

export interface AdminActionOptions<A extends unknown[], R> {
  /** Success-toast copy. Function form for interpolation; omit for no toast
   *  (a write whose result is obvious on screen does not need one). */
  success?: ToastCopy | ((result: R, ...args: A) => ToastCopy | null);
  /** Copy when the error is not an ApiError. The function form takes over
   *  entirely, including for ApiErrors — use it to branch on `.code`. */
  failure: ToastCopy | ((error: unknown, ...args: A) => ToastCopy);
  /** Runs on success before the toast: close the modal, patch local state. */
  onSuccess?: (result: R, ...args: A) => void;
  /** Runs on failure instead of the default toast-only handling. The toast
   *  still fires; this is for extra state (re-opening a confirm, say). */
  onFailure?: (error: unknown, ...args: A) => void;
}

export interface AdminAction<A extends unknown[]> {
  /**
   * Never throws — the toast is the error report. Resolves true on success.
   * Concurrent runs are allowed (per-row actions in a table); `pending` then
   * reflects the most recently started one.
   */
  run: (...args: A) => Promise<boolean>;
  /** Args of the newest unfinished run, else null. Per-row gating reads
   *  `pending?.[0].id === row.id`; whole-form gating reads `pending != null`. */
  pending: A | null;
}

function normalise(copy: ToastCopy): { title: string; body?: string } {
  return typeof copy === "string" ? { title: copy } : copy;
}

export function useAdminAction<A extends unknown[], R>(
  fn: (...args: A) => Promise<R>,
  opts: AdminActionOptions<A, R>,
): AdminAction<A> {
  const { addToast } = useToast();
  const [pending, setPending] = useState<A | null>(null);
  const running = useRef(0);

  // Read options through a ref so `run` stays stable across renders even when
  // the caller passes inline closures (all of them do).
  const optsRef = useRef(opts);
  optsRef.current = opts;
  const fnRef = useRef(fn);
  fnRef.current = fn;

  const run = useCallback(async (...args: A): Promise<boolean> => {
    const { success, failure, onSuccess, onFailure } = optsRef.current;
    running.current++;
    setPending(args);
    try {
      const result = await fnRef.current(...args);
      onSuccess?.(result, ...args);
      const copy = typeof success === "function" ? success(result, ...args) : success;
      if (copy) addToast({ variant: "success", ...normalise(copy) });
      return true;
    } catch (error: unknown) {
      onFailure?.(error, ...args);
      const copy =
        typeof failure === "function"
          ? failure(error, ...args)
          : error instanceof ApiError
            ? error.message
            : failure;
      addToast({ variant: "danger", ...normalise(copy) });
      return false;
    } finally {
      running.current--;
      if (running.current === 0) setPending(null);
    }
  }, [addToast]);

  return { run, pending };
}
