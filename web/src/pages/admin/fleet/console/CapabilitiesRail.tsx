// The console page's "Reported capabilities" rail card (handoff-v3-spec §A.6):
// connectors, per-output facts, audio sinks, and the input-device count.

import type { ConsoleCapabilities } from "../../../../api/types";
import { passedThroughPaths, type InputDevicesValue } from "./inputDevices";

export function CapabilitiesRail({
  capabilities,
  hasCapabilities,
  inputDevicesValue,
}: {
  capabilities: ConsoleCapabilities | null;
  hasCapabilities: boolean;
  inputDevicesValue: InputDevicesValue;
}) {
  const devices = capabilities?.input_devices ?? [];
  const passedCount = passedThroughPaths(inputDevicesValue, devices).size;
  return (
    <div className="card card-pad">
      <div className="eyebrow">Reported capabilities</div>
      {hasCapabilities ? (
        <div className="col gap2" style={{ marginTop: 10, fontSize: "var(--t-xs)", color: "var(--text-3)" }}>
          <div>Connectors: <span className="mono">{capabilities!.connectors.join(", ") || "—"}</span></div>
          {capabilities!.outputs?.map((output) => (
            <div
              key={output.id}
              style={{
                borderLeft: "2px solid var(--line-2)",
                paddingLeft: 9,
                display: "flex",
                flexDirection: "column",
                gap: 2,
              }}
            >
              <span className="cell-id" style={{ alignSelf: "flex-start" }}>{output.id}</span>
              <span className="mono">{output.render_node ?? "no render node"}</span>
              <span>{output.connected ? "connected" : "disconnected"}</span>
              <span>
                {output.active_mode
                  ? `active ${output.active_mode.width}×${output.active_mode.height} @ ${(output.active_mode.refresh_millihz / 1000).toFixed(3)} Hz`
                  : "inactive"}
              </span>
              {/* Preferred mode: only without an active_mode, to avoid repeating it. */}
              {!output.active_mode && (
                <span>
                  {output.modes
                    .filter((mode) => mode.preferred)
                    .map((mode) => `${mode.width}×${mode.height} @ ${(mode.refresh_millihz / 1000).toFixed(3)} Hz`)
                    .join(", ") || `${output.modes.length} mode(s)`}
                </span>
              )}
            </div>
          ))}
          <div>
            Audio sinks: {capabilities!.audio_sinks.map((s) => s.label).join(", ") || "—"}
          </div>
          <div>
            Input devices: {devices.length} reported · {passedCount} passed through
          </div>
        </div>
      ) : (
        <p className="hint" style={{ marginTop: 10 }}>Host reported no capabilities (agent offline or older).</p>
      )}
    </div>
  );
}
