// Type section — /styleguide. Michroma is the wordmark font only (never
// body text); IBM Plex Sans carries headings and UI; IBM Plex Mono carries
// metrics, ids and code. QuasarMark + .wordmark are the real brand lockup
// (shell/Topbar.tsx, shell/HomeShell.tsx), not a re-drawn copy.
import { QuasarMark } from "../../components/QuasarMark";

const SCALE: Array<{ tag: string; token: string; sample: string; heading?: boolean }> = [
  { tag: "h1 · 1.9rem", token: "--t-h1", sample: "Fleet capacity", heading: true },
  { tag: "h2 · 1.45rem", token: "--t-h2", sample: "Active sessions", heading: true },
  { tag: "h3 · 1.15rem", token: "--t-h3", sample: "Runtime specification", heading: true },
  { tag: "lg · 1.0625rem", token: "--t-lg", sample: "The stream is live." },
  { tag: "base · .9375rem", token: "--t-base", sample: "The agent reports encode latency and bitrate once a second." },
  { tag: "sm · .8125rem", token: "--t-sm", sample: "Last heartbeat 12:51:26 · agent v0.1.0" },
  { tag: "xs · .6875rem", token: "--t-xs", sample: "Active sessions" },
];

export function TypeSection() {
  return (
    <section className="sg-block" id="sg-type">
      <h2>Type</h2>
      <p className="sg-desc">
        Michroma sets the wordmark only, uppercase with wide tracking. IBM Plex Sans carries every
        heading and body string. IBM Plex Mono carries ids, telemetry and numbers.
      </p>

      <div className="sg-comp-label">Wordmark</div>
      <div className="sg-comp-block sg-specimen" style={{ display: "flex", alignItems: "center", gap: "var(--s3)" }}>
        <QuasarMark size={32} />
        <span className="wordmark" style={{ fontSize: "1.1rem" }}>Quasar</span>
      </div>

      <div className="sg-comp-label">Plex scale</div>
      <div className="sg-comp-block sg-specimen">
        {SCALE.map((row) => (
          <div key={row.token} className="sg-type-row">
            <div className="sg-tag">{row.tag}</div>
            <div
              style={{
                fontFamily: "var(--font-display)",
                fontSize: `var(${row.token})`,
                fontWeight: row.heading ? 600 : 400,
                letterSpacing: row.heading ? "-.01em" : undefined,
              }}
            >
              {row.sample}
            </div>
          </div>
        ))}
        <div className="sg-type-row">
          <div className="sg-tag">mono · .8125rem</div>
          <div className="mono" style={{ fontSize: "var(--t-sm)" }}>
            session 743a921f · 6347.1 kbps · RTT 5.0 ms · σ 0.42
          </div>
        </div>
        <div className="sg-type-row">
          <div className="sg-tag">eyebrow · .6875rem</div>
          <div className="eyebrow">Active sessions</div>
        </div>
      </div>
    </section>
  );
}
