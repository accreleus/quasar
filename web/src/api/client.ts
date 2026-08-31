// Shared API client for the whole SPA (both /app and /admin areas use this).
//
// The API origin is always `location.origin`: the control plane is the public
// ingress and serves the built SPA itself, and Vite proxies /v1 to it in dev.
// No base URL is ever baked into the bundle.

import type { ApiErrorBody } from "./types";

const API_PREFIX = "/v1";

/**
 * Thrown for any non-2xx response. Callers branch on `code`, never on the human
 * message. Code semantics: control-api.md §Errors.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly liveSessions?: number;
  /** Present on `home_in_use` (409) when the conflicting session is known.
   *  Omitted, never empty — branch on presence, or you link to nowhere. */
  readonly sessionId?: string;
  /** Seconds from the `Retry-After` response HEADER, not the JSON envelope, and
   *  only on `503 capacity_exhausted`. Undefined otherwise. */
  readonly retryAfterSeconds?: number;

  constructor(
    status: number,
    code: string,
    message: string,
    liveSessions?: number,
    sessionId?: string,
    retryAfterSeconds?: number,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.liveSessions = liveSessions;
    this.sessionId = sessionId;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

export interface RequestOptions {
  method?: string;
  /** JSON-serialisable request body. */
  body?: unknown;
  /** Bearer token; when set, sent as `Authorization: Bearer <token>`. */
  token?: string | null;
  /** Merged after `Content-Type`/`Authorization`. The setup-claim call needs
   *  this: it authenticates with `X-Quasar-Setup-Token`, not a bearer token. */
  headers?: Record<string, string>;
  signal?: AbortSignal;
}

async function parseError(res: Response): Promise<ApiError> {
  let code = "internal";
  let message = res.statusText || "request failed";
  let liveSessions: number | undefined;
  let sessionId: string | undefined;
  try {
    const data = (await res.json()) as Partial<ApiErrorBody>;
    if (data?.error?.code) code = data.error.code;
    if (data?.error?.message) message = data.error.message;
    if (typeof data?.error?.live_sessions === "number") liveSessions = data.error.live_sessions;
    if (typeof data?.error?.session_id === "string") sessionId = data.error.session_id;
  } catch {
    // Non-JSON error body (e.g. a proxy 502) — keep the status-derived defaults.
  }
  // Retry-After is a response header, never part of the JSON envelope, and only
  // ever delta-seconds. Semantics: control-api.md §errors.
  let retryAfterSeconds: number | undefined;
  const retryAfterHeader = res.headers.get("Retry-After");
  // A blank header must never reach Number(): `Number("")` is 0, a valid
  // "retry immediately", so it would answer for the absent-header fallback.
  if (retryAfterHeader !== null && retryAfterHeader.trim() !== "") {
    const parsed = Number(retryAfterHeader);
    if (Number.isFinite(parsed) && parsed >= 0) retryAfterSeconds = parsed;
  }
  return new ApiError(res.status, code, message, liveSessions, sessionId, retryAfterSeconds);
}

/**
 * Issue a JSON request and return the parsed body. Throws {@link ApiError} on
 * any non-2xx. A bodyless response resolves to `undefined`.
 *
 * The empty-body check must key off the BODY, never a status allowlist: a
 * bodyless `202` then reaches `res.json()` and throws a SyntaxError, which is
 * not an ApiError, so callers report a generic failure for a request the server
 * already accepted.
 */
export async function apiFetch<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  if (opts.token) headers["Authorization"] = `Bearer ${opts.token}`;
  if (opts.headers) Object.assign(headers, opts.headers);

  const res = await fetch(`${API_PREFIX}${path}`, {
    method: opts.method ?? (opts.body !== undefined ? "POST" : "GET"),
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    signal: opts.signal,
  });

  if (!res.ok) throw await parseError(res);
  if (res.status === 204 || res.status === 205) return undefined as T;

  // Read as text first: an empty body is not valid JSON.
  const text = await res.text();
  if (text === "") return undefined as T;
  return JSON.parse(text) as T;
}
