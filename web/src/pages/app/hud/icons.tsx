// HUD glyphs — 18×18, stroke 1.5–1.7, `currentColor`, copied from
// design_handoff_v3/screens/session-overlay-v3.html so the shipped icon set is
// the drawn one, not a redraw. The Display tab has no mock glyph (the fourth
// tab is spec §9's addition), so it borrows the console's monitor shape at the
// same weight and viewBox.

export function IconTabGames() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <rect x="1.6" y="4.4" width="14.8" height="9.2" rx="3.4" />
      <path d="M5.4 7.6v2.8M4 9h2.8" strokeLinecap="round" />
      <circle cx="12" cy="8.2" r=".85" fill="currentColor" />
      <circle cx="13.4" cy="10.2" r=".85" fill="currentColor" />
    </svg>
  );
}

export function IconTabInput() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="2.4" y="6" width="13.2" height="8.4" rx="1.6" />
      <path d="M5.6 3.4h6.8" />
      <path d="M6.2 9.2v2.2M5.1 10.3h2.2" />
      <circle cx="11.8" cy="10.3" r=".8" fill="currentColor" />
    </svg>
  );
}

export function IconTabStats() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
      <path d="M2.8 15V9.6M9 15V4M15.2 15V7.2" />
    </svg>
  );
}

export function IconTabDisplay() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="2" y="3.4" width="14" height="9.4" rx="1.6" />
      <path d="M6.6 15.6h4.8M9 12.8v2.8" />
    </svg>
  );
}

export function IconCapture() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden="true">
      <circle cx="9" cy="9" r="5.4" />
      <path d="M9 1.6v2.4M9 14v2.4M1.6 9h2.4M14 9h2.4" />
      <circle cx="9" cy="9" r="1.5" fill="currentColor" stroke="none" />
    </svg>
  );
}

/** One glyph, two states: the slash path is dropped when the mic is live —
 *  the mock's `#micSlash` display toggle, expressed as JSX. */
export function IconMicrophone({ on }: { on: boolean }) {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="6.6" y="1.9" width="4.8" height="8.6" rx="2.4" />
      <path d="M4 8.4v.6a5 5 0 0 0 10 0v-.6" />
      <path d="M9 14v2.1" />
      {!on && <path d="M2.6 2.6l12.8 12.8" data-slash="true" />}
    </svg>
  );
}

export function IconFullscreen() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M3 6.5V3.5h3M15 6.5V3.5h-3M3 11.5v3h3M15 11.5v3h-3" />
    </svg>
  );
}

export function IconChevron() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M4.5 11 9 6.5l4.5 4.5" />
    </svg>
  );
}

export function IconExit() {
  return (
    <svg viewBox="0 0 18 18" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M7 3H4v12h3M11 5.5 14.5 9 11 12.5M14.5 9H7.5" />
    </svg>
  );
}

/** The capture CTA's 24×24 gamepad (InputPane column 1). */
export function IconGamepadLarge() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M7 8h10a4.5 4.5 0 0 1 4.4 5.5l-.7 3a2 2 0 0 1-3.6.6L15.5 15h-7l-1.6 2.1a2 2 0 0 1-3.6-.6l-.7-3A4.5 4.5 0 0 1 7 8Z" />
      <line x1="7" y1="11" x2="7" y2="13" />
      <line x1="6" y1="12" x2="8" y2="12" />
      <circle cx="16" cy="11.4" r=".55" fill="currentColor" />
      <circle cx="17.6" cy="13" r=".55" fill="currentColor" />
    </svg>
  );
}

/** Warning triangle for the Hz-mismatch flag (16×16 in the mock). */
export function IconHzWarn() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true">
      <path d="M8 2.8 14.2 13H1.8z" strokeLinejoin="round" />
      <path d="M8 6.6v3.1" strokeLinecap="round" />
      <circle cx="8" cy="11.3" r=".7" fill="currentColor" stroke="none" />
    </svg>
  );
}
