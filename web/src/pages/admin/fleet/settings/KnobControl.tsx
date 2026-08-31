// The typed control for one config knob — switch / render_node picker / enum
// select / text|number input. Split out of KnobRow so the branch-per-type
// block reads as one thing; owns no state, just renders `value` and reports
// edits through `onChange`.

import type { ConfigKnob } from "../../../../api/types";
import type { SettingValue } from "./knobs";

export interface RenderNodeOption {
  value: string;
  label: string;
}

export function KnobControl({
  knob,
  value,
  onChange,
  renderNodeOptions,
}: {
  knob: ConfigKnob;
  value: SettingValue | undefined;
  onChange: (v: SettingValue) => void;
  /** Only read when `knob.key === "render_node"`. */
  renderNodeOptions: RenderNodeOption[];
}) {
  if (knob.type === "bool") {
    return (
      <button
        className="switch"
        role="switch"
        aria-checked={Boolean(value)}
        aria-label={knob.key}
        onClick={() => onChange(!value)}
        type="button"
      />
    );
  }

  if (knob.key === "render_node") {
    return (
      <select
        className="select"
        aria-label={knob.key}
        style={{ width: 260 }}
        value={String(value ?? "")}
        onChange={(e) => onChange(e.target.value)}
      >
        {renderNodeOptions.map((opt) => (
          <option key={opt.value} value={opt.value}>{opt.label}</option>
        ))}
        {value != null && !renderNodeOptions.some((opt) => opt.value === String(value)) && (
          <option value={String(value)}>{String(value)} (custom)</option>
        )}
      </select>
    );
  }

  if (knob.type === "enum") {
    return (
      <select
        className="select"
        aria-label={knob.key}
        style={{ width: 260 }}
        value={String(value ?? "")}
        onChange={(e) => onChange(e.target.value)}
      >
        {knob.enum?.map((opt) => (
          <option key={opt} value={opt}>{opt}</option>
        ))}
      </select>
    );
  }

  const unit = knob.type === "int" && knob.key === "idle_timeout_secs" ? "secs"
    : knob.type === "int" && knob.key === "abr_floor_kbps" ? "kbps"
    : undefined;

  return (
    <div className="row gap2 center">
      <input
        className={knob.key === "home_root" ? "input mono" : "input"}
        aria-label={knob.key}
        style={{ width: knob.type === "string" ? 260 : 110 }}
        type={knob.type === "string" ? "text" : "number"}
        min={knob.min}
        max={knob.max}
        placeholder={knob.key === "home_root" ? "/var/lib/quasar/homes" : undefined}
        value={value == null ? "" : String(value)}
        onChange={(e) => {
          const raw = e.target.value;
          if (knob.type === "string") {
            onChange(raw);
            return;
          }
          if (raw === "") return;
          const n = Number(raw);
          if (Number.isFinite(n)) onChange(n);
        }}
      />
      {unit && <span className="hint">{unit}</span>}
    </div>
  );
}
