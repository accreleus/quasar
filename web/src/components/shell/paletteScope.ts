// What the palette promises to search, for the shells' trigger label; split
// from paletteSearch so the shells do not load the search itself.

/**
 * The entity kinds this palette can actually answer for.
 *
 * A kind counts only when the shell HANDED the list down — `undefined` means
 * "this area does not load it", which is different from an empty list — and
 * only when the viewer is allowed to see it. The trigger label and the input
 * placeholder are both built from this, so the console cannot advertise a
 * search it will never return a row for (the account area, for instance, has
 * apps and nothing else, admin or not).
 *
 * Apps are called "games" to a non-admin: it is the same list, but their
 * library is the only thing in it and "apps" is operator vocabulary.
 */
export function paletteScope(sources: {
  isAdmin: boolean;
  hosts?: readonly unknown[];
  sessions?: readonly unknown[];
  apps?: readonly unknown[];
  users?: readonly unknown[];
}): string[] {
  const kinds: string[] = [];
  if (sources.isAdmin && sources.hosts) kinds.push("hosts");
  if (sources.isAdmin && sources.sessions) kinds.push("sessions");
  if (sources.apps) kinds.push(sources.isAdmin ? "apps" : "games");
  if (sources.isAdmin && sources.users) kinds.push("users");
  return kinds;
}

/** The dialog input's placeholder. Jumping to a page always works, so that
 *  half of the promise is unconditional. */
export function palettePlaceholder(kinds: readonly string[]): string {
  return kinds.length ? `Search ${kinds.join(", ")} or jump to a page` : "Jump to a page";
}

/** The topbar trigger's shorter label for the same promise. */
export function paletteTriggerLabel(kinds: readonly string[]): string {
  return kinds.length ? `Search ${kinds.join(", ")}\u2026` : "Jump to a page";
}
