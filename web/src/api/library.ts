// Library + session API calls. Pure functions over the shared client; state
// lives in React components.

import { apiFetch } from "./client";
import type {
  AppsResponse,
  HighlightsResponse,
  LaunchResponse,
  ProfilePreferencesResponse,
  ProfilesResponse,
  SessionResponse,
  SignalingCoords,
} from "./types";

export interface MySessionsResponse {
  items: import("./types").Session[];
  next_cursor: string | null;
}

/** The backend filters to the authenticated user; /v1/me/sessions is not
 *  registered in the control-plane route table. */
export function getMySessions(token: string): Promise<MySessionsResponse> {
  return apiFetch<MySessionsResponse>("/sessions", { token });
}

export function listApps(token: string): Promise<AppsResponse> {
  return apiFetch<AppsResponse>("/apps", { token });
}

/** Ranked and labelled server-side. Render `items` in the order given; never
 *  re-derive or re-sort it, or two rankers drift apart. Empty is a normal 200. */
export function getHighlights(token: string): Promise<HighlightsResponse> {
  return apiFetch<HighlightsResponse>("/me/highlights", { token });
}

export function favouriteApp(token: string, appId: string): Promise<void> {
  return apiFetch<void>(`/me/favourites/${appId}`, { method: "PUT", token });
}

/** Unconditional: never 404s. */
export function unfavouriteApp(token: string, appId: string): Promise<void> {
  return apiFetch<void>(`/me/favourites/${appId}`, { method: "DELETE", token });
}

export interface LaunchRequest {
  app_id: string;
  profile_id?: string;
  /** Always send `true`: the host caps each PeerConnection at one offer, so a
   *  mic m-line absent from the first offer can never be added mid-session, and
   *  an unused sendonly slot costs nothing. Granting is server-side and an
   *  ungranted request is not an error — read `session.stream.mic` for what was
   *  actually granted. */
  mic?: boolean;
  stream?: {
    width?: number;
    height?: number;
    fps?: number;
    bitrate_kbps?: number;
    /** Wire vocabulary (`h265`, not the catalog's `hevc`); `toWireCodec` in
     *  launchOptions.ts is the only place the web app maps between them. Omit
     *  for "Auto" and let the server resolve the codec. */
    codec?: "h264" | "h265" | "av1";
  };
}

/** Pass `appId` for the already-filtered launch menu; never filter client-side,
 *  since only the server knows the allow-list. A UX filter, not the gate: POST
 *  /v1/sessions rejects a disallowed `profile_id` with 409 regardless. */
export function getProfiles(token: string, appId?: string): Promise<ProfilesResponse> {
  const path = appId ? `/me/profiles?app_id=${encodeURIComponent(appId)}` : "/me/profiles";
  return apiFetch<ProfilesResponse>(path, { token });
}

export function getProfilePreferences(token: string): Promise<ProfilePreferencesResponse> {
  return apiFetch<ProfilePreferencesResponse>("/me/profile-preferences", { token });
}

export function updateProfilePreferences(
  token: string,
  defaultProfileId: string | null,
): Promise<{ default_profile_id: string | null }> {
  return apiFetch<{ default_profile_id: string | null }>("/me/profile-preferences", {
    method: "PATCH",
    body: { default_profile_id: defaultProfileId },
    token,
  });
}

/** Pass `signal` to abort on unmount: a request that outlives its component
 *  still lands server-side and creates a session nobody is waiting for. */
export function launchSession(
  token: string,
  req: LaunchRequest,
  signal?: AbortSignal,
): Promise<LaunchResponse> {
  return apiFetch<LaunchResponse>("/sessions", { method: "POST", body: req, token, signal });
}

export function getSession(token: string, id: string): Promise<SessionResponse> {
  return apiFetch<SessionResponse>(`/sessions/${id}`, { token });
}

/** Replaces the running app without tearing down the WebRTC session. Pinned to
 *  this session's host, no-resize. A target whose library lives on another host
 *  cannot be swapped into (409 home_not_provisioned); launch it directly. */
export function swapSession(
  token: string,
  sessionId: string,
  appId: string,
): Promise<SessionResponse> {
  return apiFetch<SessionResponse>(`/sessions/${encodeURIComponent(sessionId)}/swap`, {
    method: "POST",
    body: { app_id: appId },
    token,
  });
}

/**
 * Partial update of a running session; an omitted field is left alone.
 * Full validation rules: control-api.md §session-display-update.
 *
 *  - render_width/render_height and stream_width/stream_height are each
 *    both-or-neither. Never send one alone.
 *  - stream_* change what is ENCODED, not what the app draws.
 *
 * Values are ephemeral: agent-held, never persisted, and absent from the
 * Session the 202 returns, so a caller showing the current setting must keep
 * its own last-acked value. Every rejection is a no-op, which is what makes
 * reverting to that value correct.
 */
export function updateSessionDisplay(
  token: string,
  sessionId: string,
  body: {
    render_width?: number;
    render_height?: number;
    ui_scale?: number;
    stream_width?: number;
    stream_height?: number;
  },
): Promise<SessionResponse> {
  return apiFetch<SessionResponse>(`/sessions/${encodeURIComponent(sessionId)}/display`, {
    method: "PATCH",
    body,
    token,
  });
}

/** Mint fresh single-use coordinates for the same live session. */
export function mintSignalingToken(
  token: string,
  id: string,
): Promise<{ signaling: SignalingCoords }> {
  return apiFetch<{ signaling: SignalingCoords }>(`/sessions/${id}/signaling-token`, {
    method: "POST",
    token,
  });
}

/** 202 (stopping) or 200 (already terminal), both with the session body. */
export function stopSession(token: string, id: string): Promise<SessionResponse> {
  return apiFetch<SessionResponse>(`/sessions/${id}`, { method: "DELETE", token });
}

export interface BrowserStatSample {
  ts_unix_ms: number;
  metrics: Record<string, number>;
}

/** Fire-and-forget. Rate-limited; batches must stay <= 64 samples. */
export function postSessionStats(
  token: string,
  id: string,
  samples: BrowserStatSample[],
): Promise<void> {
  return apiFetch<void>(`/sessions/${id}/stats`, {
    method: "POST",
    body: { samples },
    token,
  });
}
