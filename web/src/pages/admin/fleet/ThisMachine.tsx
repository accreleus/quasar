/**
 * "This machine" in Releases ▸ Installed: the services of the control plane's own owned
 * machine (design_handoff_v3 fleet-rh06-v3.html, `rhInstalled`; screenshots rh06/
 * inv-control-only, inv-control-only-unknown, inv-control-only-error). The rows come from
 * thisMachine.ts.
 */

import type { ThisMachine } from "./thisMachine";
import { elapsedWords } from "../../../lib/format/relativeTime";

export function ThisMachineBlock({ machine, now }: { machine: ThisMachine; now: number }) {
  if (machine.kind === "none") return null;
  return (
    <div className="rel-machine">
      <div className="eyebrow">This machine</div>
      {machine.title && <div className="hint rel-machine-title">{machine.title}</div>}
      {machine.kind === "not_answering" && machine.since != null && (
        <div className="note warn rel-machine-note" role="status">
          <strong>Could not read this machine’s services.</strong> Its recovery actor has not
          answered for {elapsedWords(machine.since, now)}; below is its last report. The control
          plane is running — it is serving this page.
        </div>
      )}
      {machine.kind === "not_reported" && (
        <div className="note rel-machine-note" role="status">
          This machine’s recovery actor has not reported its services yet.
        </div>
      )}
      <div className="rel-machine-rows">
        {machine.rows.map((row) => (
          <div className="rel-fact" key={row.key}>
            <span>{row.label}</span>
            {row.hintOnly ? (
              <span className="rel-machine-none">{row.hint}</span>
            ) : (
              <span>
                {row.value == null ? (
                  <span className="rel-machine-none">—</span>
                ) : row.numeric ? (
                  <span className="num">{row.value}</span>
                ) : (
                  row.value
                )}
                <div className="hint rel-machine-hint">{row.hint}</div>
              </span>
            )}
          </div>
        ))}
      </div>
      {machine.external && (
        <p className="hint rel-machine-foot">
          Quasar only uses your database. It never dumps, restores or upgrades it.
        </p>
      )}
    </div>
  );
}
