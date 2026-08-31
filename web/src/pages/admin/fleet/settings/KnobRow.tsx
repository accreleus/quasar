// One `.cset` row (handoff-v3-spec §A.7): label + restart/overridden chips +
// help + "Default X · reset to default" on the left, the typed control on the
// right. "Current value" and the stale-effective warning have no mock
// counterpart (no mockup models the agent's true running value vs. an
// unapplied override) — kept as a `.hint` under the control so that
// information survives the restyle; see HostSettings.tsx's file comment.

import type { ConfigKnob } from "../../../../api/types";
import { Chip } from "../../../../components/Chip";
import { KnobControl, type RenderNodeOption } from "./KnobControl";
import { knobHelp, knobLabel, valueLabel, type SettingValue } from "./knobs";

export function KnobRow({
  knob,
  value,
  changed,
  currentValue,
  showsStaleEffective,
  effectiveValue,
  defaultValue,
  onChange,
  onReset,
  renderNodeOptions,
}: {
  knob: ConfigKnob;
  value: SettingValue | undefined;
  changed: boolean;
  /** What the agent is actually running — distinct from `value`, the editable
   *  draft, which can be a not-yet-saved override. */
  currentValue: SettingValue | undefined;
  showsStaleEffective: boolean;
  effectiveValue: SettingValue | undefined;
  defaultValue: SettingValue | undefined;
  onChange: (v: SettingValue) => void;
  onReset: () => void;
  renderNodeOptions: RenderNodeOption[];
}) {
  return (
    <div className="cset">
      <div>
        <h3 className="row gap2 center">
          {knobLabel(knob)}
          {knob.class === "restart" && <Chip variant="warning" className="chip-sm">restart</Chip>}
          {changed && <Chip variant="accent" className="chip-sm">overridden</Chip>}
        </h3>
        <p className="hint">{knobHelp(knob)}</p>
        <span className="cell-id" style={{ display: "inline-block", marginTop: 4 }}>{knob.env_var}</span>
        {changed && (
          <p className="hint" style={{ marginTop: 4 }}>
            Default <span className="mono">{valueLabel(defaultValue)}</span> ·{" "}
            <a href="#" onClick={(e) => { e.preventDefault(); onReset(); }}>reset to default</a>
          </p>
        )}
      </div>
      <div>
        <KnobControl knob={knob} value={value} onChange={onChange} renderNodeOptions={renderNodeOptions} />
        <p className="hint" style={{ marginTop: 6 }}>Current value is {valueLabel(currentValue)}</p>
        {showsStaleEffective && (
          <p className="host-setting-effective" style={{ marginTop: 6 }}>
            Agent is still running <strong>{valueLabel(effectiveValue)}</strong>. Restart pending.
          </p>
        )}
      </div>
    </div>
  );
}
