/**
 * StatusChip — generic status→tone chip with no built-in vocabulary: callers'
 * status vocabularies neither overlap nor agree on presentation, so each
 * supplies its own config map. Unknown status never crashes — neutral chip
 * with the raw string (the open-enum behaviour ReadinessCheck documents),
 * unless the caller supplies `fallback`.
 */
import type { ReactNode } from "react";
import { Chip, type ChipVariant } from "./Chip";

export interface StatusChipEntry {
  label: ReactNode;
  variant: ChipVariant;
  /** Show a filled dot before the label — see `Chip`'s `dot` prop. */
  dot?: boolean;
}

export type StatusChipConfig = Record<string, StatusChipEntry>;

const DEFAULT_FALLBACK = (status: string): StatusChipEntry => ({
  label: status,
  variant: "neutral",
});

export interface StatusChipProps {
  /** The raw status value to look up in `config`. */
  status: string;
  /** Status -> {label, variant, dot} map, owned by the caller. */
  config: StatusChipConfig;
  /** Used instead of the neutral/raw-text default when `status` isn't in `config`. */
  fallback?: StatusChipEntry;
  /** Forwarded to `Chip`. */
  className?: string;
}

export function StatusChip({ status, config, fallback, className }: StatusChipProps) {
  // Object.hasOwn, not a plain `config[status]` index: `status` is caller/
  // server-controlled text, and a plain index lookup resolves inherited
  // Object.prototype members for names like "constructor"/"toString" —
  // `variant` would then be `undefined` (not a StatusChipEntry) and slip
  // past the `??` fallback instead of being caught by it.
  const entry = (Object.hasOwn(config, status) ? config[status] : undefined) ?? fallback ?? DEFAULT_FALLBACK(status);
  return (
    <Chip variant={entry.variant} dot={entry.dot} className={className}>
      {entry.label}
    </Chip>
  );
}
