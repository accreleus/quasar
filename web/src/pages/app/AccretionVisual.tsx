/**
 * The accretion visual on the launch screen (handoff-v3 §D): jets, two orbits,
 * the accretion disc, the core and a scanline, back to front, all on one 4.8 s
 * loop. Purely decorative — every layer is a styled <div>, the keyframes live
 * in SessionLoader.css, and the whole thing is hidden from assistive tech
 * (the status block below it says what is happening).
 */
export function AccretionVisual() {
  return (
    <div className="sl-quasar" aria-hidden="true">
      <div className="jet jet-left" />
      <div className="jet jet-right" />
      <div className="orbit orbit-b" />
      <div className="orbit orbit-a" />
      <div className="disc" />
      <div className="core" />
      <div className="scanline" />
    </div>
  );
}
