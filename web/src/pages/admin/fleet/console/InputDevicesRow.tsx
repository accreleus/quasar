// The full-width "Input devices" `.cset` (handoff-v3-spec §A.6): segmented
// Auto/Specific/None, class chips, and a per-device table. See the file
// comment on HostConsole.tsx and inputDevices.ts for what is and isn't real
// wire behaviour here — the class grouping is a client-side label heuristic,
// and only "Specific devices" mode has a real per-device on/off to edit.

import { SegmentedControl } from "../../../../components/SegmentedControl";
import { Chip } from "../../../../components/Chip";
import { classifyDevice, passedThroughPaths, type DeviceClass, type InputDevicesValue } from "./inputDevices";

type Mode = "auto" | "specific" | "none";

const CLASS_ORDER: DeviceClass[] = ["Keyboard", "Mouse", "Controller", "Touch", "Tablet", "Audio jack"];
const CLASS_PLURAL: Record<DeviceClass, string> = {
  Keyboard: "Keyboards",
  Mouse: "Mice",
  Controller: "Controllers",
  Touch: "Touch",
  Tablet: "Tablets",
  "Audio jack": "Audio jacks",
  Other: "Other",
};

function modeOf(value: InputDevicesValue): Mode {
  if (Array.isArray(value)) return value.length === 0 ? "none" : "specific";
  return "auto";
}

export function InputDevicesRow({
  value,
  devices,
  onChange,
}: {
  value: InputDevicesValue;
  devices: { path: string; label: string }[];
  onChange: (v: InputDevicesValue) => void;
}) {
  const mode = modeOf(value);
  const passed = passedThroughPaths(value, devices);
  const editable = mode === "specific";
  const current = Array.isArray(value) ? value : [];

  const setPaths = (paths: string[]) => onChange(paths);

  const toggleDevice = (path: string) => {
    if (!editable) return;
    setPaths(passed.has(path) ? current.filter((p) => p !== path) : [...current, path]);
  };

  const classDevices = (cls: DeviceClass) => devices.filter((d) => classifyDevice(d.label) === cls);

  const toggleClass = (cls: DeviceClass) => {
    if (!editable) return;
    const paths = classDevices(cls).map((d) => d.path);
    const allOn = paths.length > 0 && paths.every((p) => passed.has(p));
    setPaths(allOn ? current.filter((p) => !paths.includes(p)) : [...new Set([...current, ...paths])]);
  };

  return (
    <div className="cset" style={{ gridTemplateColumns: "1fr", alignItems: "stretch" }}>
      <div>
        <h3>Input devices</h3>
        <p className="hint">
          What the host reports, and what gets passed through to the container. Pick device
          classes to stay broad, or select individual devices.
        </p>
      </div>
      <div style={{ marginTop: "var(--s3)" }}>
        <div style={{ marginBottom: "var(--s4)" }}>
          <SegmentedControl<Mode>
            aria-label="Input device selection"
            value={mode}
            onChange={(next) => {
              if (next === "auto") onChange("auto");
              else if (next === "none") onChange([]);
              else setPaths(mode === "auto" ? devices.map((d) => d.path) : current);
            }}
            options={[
              { value: "auto", label: "Auto · by class" },
              { value: "specific", label: "Specific devices" },
              { value: "none", label: "None" },
            ]}
          />
        </div>

        <div className="row gap2" style={{ flexWrap: "wrap", marginBottom: "var(--s4)" }}>
          {CLASS_ORDER.map((cls) => {
            const paths = classDevices(cls).map((d) => d.path);
            const on = mode === "none" ? false : paths.length > 0 && paths.every((p) => passed.has(p));
            return (
              <button
                key={cls}
                type="button"
                className={`chip${on ? " chip-accent" : ""}`}
                style={{ height: 26, cursor: editable ? "pointer" : "default", opacity: editable || on ? 1 : 0.6 }}
                disabled={!editable}
                onClick={() => toggleClass(cls)}
              >
                {on ? "✓ " : ""}{CLASS_PLURAL[cls]}
              </button>
            );
          })}
        </div>

        <div className="table-wrap" style={{ border: "1px solid var(--line)", borderRadius: "var(--r-sm)" }}>
          <table className="qtable">
            <thead>
              <tr>
                <th style={{ width: 34 }} />
                <th>Device</th>
                <th>Class</th>
                <th>Path</th>
                <th className="right">State</th>
              </tr>
            </thead>
            <tbody>
              {devices.map((d) => {
                const on = passed.has(d.path);
                return (
                  <tr key={d.path}>
                    <td>
                      <label className="check" style={{ minHeight: 0 }}>
                        <input
                          type="checkbox"
                          checked={on}
                          disabled={!editable}
                          onChange={() => toggleDevice(d.path)}
                          aria-label={`Pass through ${d.label}`}
                        />
                      </label>
                    </td>
                    <td className="primary">{d.label}</td>
                    <td>{classifyDevice(d.label)}</td>
                    <td><span className="cell-id">{d.path}</span></td>
                    <td className="right">
                      {on
                        ? <Chip variant="success">passed through</Chip>
                        : <span className="hint">not passed</span>}
                    </td>
                  </tr>
                );
              })}
              {devices.length === 0 && (
                <tr><td colSpan={5} className="hint">No input devices reported.</td></tr>
              )}
            </tbody>
          </table>
        </div>
        <p className="hint" style={{ marginTop: 9 }}>
          Class rules follow hot-plug: a controller connected later is passed through
          automatically. Individually selected devices are pinned by path and will not follow a
          re-enumeration.
        </p>
      </div>
    </div>
  );
}
