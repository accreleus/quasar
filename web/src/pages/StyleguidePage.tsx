// /styleguide — canonical in-app v3 design reference. Every example on this
// page composes the real component the rest of the app renders; build new
// pages by pulling from here, not by eyeballing another page's markup. See
// docs/superpowers/specs/2026-08-28-ui-v3-design.md and
// design_handoff_v3/screens/assets/console-v3.css for the source contract.
import { QuasarMark } from "../components/QuasarMark";
import { SegmentedControl } from "../components/SegmentedControl";
import { useTheme } from "../settings/ThemeContext";
import type { Theme, Density } from "../settings/ThemeContext";
import { ComponentsSection } from "./styleguide/ComponentsSection";
import { PreviewSection } from "./styleguide/PreviewSection";
import { TokensSection } from "./styleguide/TokensSection";
import { TypeSection } from "./styleguide/TypeSection";
import "./styleguide/styleguide.css";

export function StyleguidePage() {
  const { theme, density, setTheme, setDensity } = useTheme();

  return (
    <div className="sg-page">
      <div className="sg-controls">
        <span className="sg-controls-label">Theme</span>
        <SegmentedControl
          aria-label="Theme"
          options={
            [
              { value: "dark", label: "Dark" },
              { value: "light", label: "Light" },
            ] as Array<{ value: Theme; label: string }>
          }
          value={theme}
          onChange={(v) => setTheme(v as Theme)}
        />
        <span className="sg-controls-label" style={{ marginLeft: "var(--s4)" }}>
          Density
        </span>
        <SegmentedControl
          aria-label="Density"
          options={
            [
              { value: "comfortable", label: "Comfortable" },
              { value: "dense", label: "Dense" },
            ] as Array<{ value: Density; label: string }>
          }
          value={density}
          onChange={(v) => setDensity(v as Density)}
        />
      </div>

      <div className="sg-hero">
        <QuasarMark size={72} className="sg-qmark" />
        <h1>
          <span className="wordmark">Quasar</span> design system
        </h1>
        <p className="sg-lede">
          The v3 visual language for the console and the streaming client. Every swatch and
          control below is the real token or the real component, so this page never drifts from
          what ships.
        </p>
      </div>

      <TokensSection />
      <TypeSection />
      <ComponentsSection />
      <PreviewSection />
    </div>
  );
}
