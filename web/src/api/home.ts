// The Home layout: GET/PUT/DELETE /v1/me/home (the caller) and /v1/admin/home
// (the instance default). semantics: control-api.md §Home layout
import { apiFetch } from "./client";
import type { components } from "./schema";

export type HomeLayout = components["schemas"]["HomeLayout"];
export type HomeSection = components["schemas"]["HomeSection"];
export type HomeScope = "/me/home" | "/admin/home";

export function getHomeLayout(token: string, scope: HomeScope, signal?: AbortSignal): Promise<HomeLayout> {
  return apiFetch<HomeLayout>(scope, { token, signal });
}

/** Whole replace; answers the layout now in effect for that scope. */
export function putHomeLayout(token: string, scope: HomeScope, sections: HomeSection[]): Promise<HomeLayout> {
  return apiFetch<HomeLayout>(scope, { method: "PUT", body: { sections }, token });
}

/** Clears the override or default; answers the layout that now applies. */
export function clearHomeLayout(token: string, scope: HomeScope): Promise<HomeLayout> {
  return apiFetch<HomeLayout>(scope, { method: "DELETE", token });
}
