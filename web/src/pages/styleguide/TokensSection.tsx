// Tokens section — /styleguide (plan Task 30 step 2). Swatches read the
// actual computed value of each custom property off :root at render time, so
// this section can never drift from tokens.css: change a value there and the
// label here changes with it. No hex literals — every swatch is `var(--x)`.
interface TokenEntry {
  name: string;
  label: string;
}

function readVar(name: string): string {
  if (typeof window === "undefined") return "";
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

function TokenRow({ token }: { token: TokenEntry }) {
  // Not memoized: a theme/density toggle changes the resolved value of a
  // token without changing token.name, and this must reflect it on the next
  // render rather than showing the value it had when first mounted.
  const value = readVar(token.name);
  return (
    <div className="sg-sw">
      <div className="sg-chip-color" style={{ background: `var(${token.name})` }} />
      <div className="sg-meta">
        <div className="sg-nm">{token.label}</div>
        <div className="sg-tok">{token.name}</div>
        <div className="sg-hex">{value}</div>
      </div>
    </div>
  );
}

function TokenGrid({ tokens }: { tokens: TokenEntry[] }) {
  return (
    <div className="sg-grid" style={{ gridTemplateColumns: "repeat(auto-fit,minmax(150px,1fr))" }}>
      {tokens.map((t) => (
        <TokenRow key={t.name} token={t} />
      ))}
    </div>
  );
}

const SURFACES: TokenEntry[] = [
  { name: "--core", label: "Core" },
  { name: "--surf-canvas", label: "Canvas" },
  { name: "--surf-chrome", label: "Chrome" },
  { name: "--surf-panel", label: "Panel" },
  { name: "--surf-raised", label: "Raised" },
  { name: "--surf-control", label: "Control" },
  { name: "--surf-inset", label: "Inset" },
];

const TEXT: TokenEntry[] = [
  { name: "--text", label: "Text" },
  { name: "--text-2", label: "Text 2" },
  { name: "--text-3", label: "Text 3" },
  { name: "--text-4", label: "Text 4" },
];

const ACCENT: TokenEntry[] = [
  { name: "--accent", label: "Accent" },
  { name: "--action", label: "Action" },
  { name: "--accent-hover", label: "Accent hover" },
  { name: "--accent-press", label: "Accent press" },
  { name: "--accent-text", label: "Accent text" },
  { name: "--accent-soft", label: "Accent soft" },
  { name: "--accent-soft-2", label: "Accent soft 2" },
  { name: "--teal", label: "Teal (capacity)" },
];

const STATE: TokenEntry[] = [
  { name: "--success", label: "Success" },
  { name: "--warning", label: "Warning" },
  { name: "--danger", label: "Danger" },
  { name: "--info", label: "Info" },
];

const SPACING = ["--s1", "--s2", "--s3", "--s4", "--s5", "--s6", "--s7", "--s8"] as const;
const RADII: TokenEntry[] = [
  { name: "--r-control", label: "Control" },
  { name: "--r-panel", label: "Panel" },
  { name: "--r-feature", label: "Feature" },
  { name: "--r-pill", label: "Pill" },
];

export function TokensSection() {
  return (
    <section className="sg-block" id="sg-tokens">
      <h2>Tokens</h2>
      <p className="sg-desc">
        The v3 token contract, read live off <code className="mono">:root</code>. Surfaces are one
        hue family stepped by lightness, text and accent are a single violet, and the state quad
        keeps ops conventions. Change a value in tokens.css and this section changes with it.
      </p>

      <div className="sg-comp-label">Surfaces</div>
      <div className="sg-comp-block">
        <TokenGrid tokens={SURFACES} />
      </div>

      <div className="sg-comp-label">Text</div>
      <div className="sg-comp-block">
        <TokenGrid tokens={TEXT} />
      </div>

      <div className="sg-comp-label">Accent</div>
      <div className="sg-comp-block">
        <TokenGrid tokens={ACCENT} />
      </div>

      <div className="sg-comp-label">State</div>
      <div className="sg-comp-block">
        <TokenGrid tokens={STATE} />
      </div>

      <div className="sg-comp-label">Spacing</div>
      <div className="sg-comp-block">
        <div className="sg-specimen-row" style={{ alignItems: "flex-end" }}>
          {SPACING.map((token) => (
            <div key={token} className="sg-scale-cell">
              <div className="sg-scale-box" style={{ width: `var(${token})`, height: `var(${token})` }} />
              <div className="sg-lbl-sm">{token}</div>
            </div>
          ))}
        </div>
      </div>

      <div className="sg-comp-label">Radii</div>
      <div className="sg-comp-block">
        <div className="sg-specimen-row">
          {RADII.map((r) => (
            <div key={r.name} className="sg-scale-cell">
              <div
                style={{
                  width: 74,
                  height: 54,
                  background: "var(--surf-raised)",
                  border: "1px solid var(--line-2)",
                  borderRadius: `var(${r.name})`,
                }}
              />
              <div className="sg-lbl-sm">{r.label}</div>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
