/**
 * The vocabulary a rail is described in — shared by the two IA modules that
 * produce rails (`pages/admin/adminNav.ts`, `pages/app/account/accountNav.ts`)
 * and the one component that consumes them (`./Rail.tsx`).
 *
 * It lives here, in neither of the producers, because a type declared beside
 * one producer and structurally re-declared beside the other is how the two
 * rails drift: they would still compile against each other right up until one
 * of them grew a field. One declaration, three importers.
 */

import type { IconName } from "../icons";

/** Marker kinds a rail row may carry. */
export type RailBadge = "live" | "fault";

export interface RailItem {
  id: string;
  label: string;
  icon: IconName;
  to: string;
  badge?: RailBadge;
  /** Is this the lit row for `path`? Exactly one item may answer true. */
  match: (path: string) => boolean;
}

export interface RailSection {
  /** Uppercase 10px `.rail-sec` heading. The admin rail passes none. */
  title?: string;
  items: RailItem[];
}

/** Counts behind the two markers. A zero (or a missing count) draws nothing —
 *  an always-on zero is what trains an operator to ignore the badge. */
export interface RailBadgeCounts {
  live?: number;
  fault?: number;
}
