// Per-game stream settings have no control-api home yet (issue #720): nothing is
// read or written, so the page always opens on the device recommendation.

export interface GameSettings {
  /** The profile chosen for this game (a launch profile id). */
  profile_id: string;
  /** Wire codec or omitted for Auto. */
  codec?: "h264" | "h265" | "av1";
  /** "explicit" = the user set it; "remembered" = carried over from the last launch. */
  source: "explicit" | "remembered";
  updated_at?: string;
}

export function getGameSettings(_token: string, _appId: string): Promise<GameSettings | null> {
  return Promise.resolve(null);
}

export function putGameSettings(_token: string, _appId: string, s: GameSettings): Promise<GameSettings> {
  return Promise.resolve(s);
}

export function clearGameSettings(_token: string, _appId: string): Promise<void> {
  return Promise.resolve();
}
