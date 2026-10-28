// Where the user is and what the search trigger promises there. Split from the
// settings index so the shells can label the trigger without loading the index.

export type Place = { kind: "account" } | { kind: "home" } | { kind: "admin"; area: string; path: string };

/** The admin place needs an admin: the console index is never searched for anyone else. */
export function placeOf(pathname: string, isAdmin: boolean): Place {
  if (isAdmin && pathname.startsWith("/admin")) return { kind: "admin", area: pathname.split("/")[2] || "overview", path: pathname.replace(/\/+$/, "") };
  if (pathname.startsWith("/app/account")) return { kind: "account" };
  return { kind: "home" };
}

export function settingsTriggerLabel(kind: Place["kind"]): string {
  if (kind === "admin") return "Search hosts, sessions, apps, users, settings…";
  if (kind === "account") return "Search settings and games…";
  return "Search games and settings…";
}

/** The trigger on a phone, where the full label cannot fit on one line. */
export function settingsShortLabel(kind: Place["kind"]): string {
  return kind === "admin" ? "Search the console\u2026" : kind === "account" ? "Search settings\u2026" : "Search games\u2026";
}

export function settingsPlaceholder(kind: Place["kind"]): string {
  if (kind === "admin") return "Search hosts, sessions, apps, users, pages or settings";
  if (kind === "account") return "Search settings and games";
  return "Search games and settings";
}
