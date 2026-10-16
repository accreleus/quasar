// The topbar search field stays on the game page.

/** The topbar field also shows on the game page; typing there filters the library. */
export const searchOnGamePage = (pathname: string, newDesign: boolean): boolean =>
  newDesign && pathname.startsWith("/app/game/");
