// Search hands a chosen game to its page in one step, and the topbar search
// field stays on that page.

/** The game page's URL for a player's chosen palette game row, else undefined. */
export function paletteGameHref(item: { id: string }, isAdmin: boolean, newDesign: boolean): string | undefined {
  return newDesign && !isAdmin && item.id.startsWith("app-") ? `/app/game/${item.id.slice(4)}` : undefined;
}

/** The topbar field also shows on the game page; typing there filters the library. */
export const searchOnGamePage = (pathname: string, newDesign: boolean): boolean =>
  newDesign && pathname.startsWith("/app/game/");
