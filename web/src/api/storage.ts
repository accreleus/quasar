// User-scoped storage API (P5-01 / P5-05).
// GET /v1/me/storage is live; returns 200 + {items:[]} for a user with no homes.

import { apiFetch } from "./client";
import type { MyStorageResponse } from "./types";

/** GET /v1/me/storage — the caller's own per-app storage usage. */
export function getMyStorage(token: string, signal?: AbortSignal): Promise<MyStorageResponse> {
  return apiFetch<MyStorageResponse>("/me/storage", { token, signal });
}
