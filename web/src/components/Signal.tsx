/**
 * Signal — connection-quality glyph (UI-04).
 * Four rising bars, coloured by data-q attribute value.
 */

export type SignalQuality = "excellent" | "good" | "fair" | "poor";

interface SignalProps {
  quality: SignalQuality;
  /** Optional accessible label; defaults to the quality string */
  label?: string;
  /** Extra class (the HUD's `.v-signal`, which its readout preference hides). */
  className?: string;
  /** Hover text. The a11y name comes from `label`; this is for a mouse. */
  title?: string;
}

export function Signal({ quality, label, className, title }: SignalProps) {
  return (
    <span
      className={className ? `signal ${className}` : "signal"}
      data-q={quality}
      role="img"
      aria-label={label ?? quality}
      title={title}
    >
      <i />
      <i />
      <i />
      <i />
    </span>
  );
}
