/**
 * TierBadge — stream-quality tier badge.
 * Matches the `.tier` / `.tier-hi` / `.tier-low` classes.
 */

export type TierLevel = "hi" | "mid" | "low";

interface TierBadgeProps {
  children: string;
  level?: TierLevel;
}

export function TierBadge({ children, level }: TierBadgeProps) {
  const classes = [
    "tier",
    level === "hi" ? "tier-hi" : level === "low" ? "tier-low" : "",
  ]
    .filter(Boolean)
    .join(" ");

  return <span className={classes}>{children}</span>;
}
