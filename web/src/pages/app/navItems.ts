// The user area's navigation facts, component-free so userTabs.tsx can derive
// from `buildUserNav` without AppLayout and userTabs importing each other.
// AppLayout re-exports both names for existing import sites.

/** Routes that render a filterable app grid, and therefore want the search box.
 *  Account/Storage have no grid for it to filter. */
export const SEARCHABLE_ROUTES = ["/app", "/app/library"] as const;

/** The topbar pill nav for the user area. */
export function buildUserNav(): { to: string; label: string }[] {
  return [
    { to: "/app", label: "Home" },
    { to: "/app/library", label: "Library" },
  ];
}
