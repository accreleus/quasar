// Route modules split out of the entry chunk, each preloadable ahead of need
// (lib/preloadable.ts) by whatever usually leads to it.
import { preloadable } from "../lib/preloadable";

export const sessionPage = preloadable(() => import("./app/SessionPage"));
export const gamePage = preloadable(() => import("./app/game/GamePage"));
export const accountArea = preloadable(() => import("./app/account/area"));
export const loginPage = preloadable(() => import("./LoginPage"));
