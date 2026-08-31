/**
 * The session detail hero (handoff §A.3). The six facts mix two clocks on
 * purpose: resolution and codec are what the server resolved, the rest is the
 * newest telemetry sample, and the gap between them is the finding.
 */

import type { AdminSession } from "../../../api/types";
import { Button } from "../../../components/Button";
import { IconDownload } from "../../../components/icons";
import { codecDisplayName, normaliseCodec } from "../../../lib/codecDisplay";
import { agentMetrics, browserMetrics } from "../../../lib/fleet/sessionMetrics";
import { bitrate } from "../../../lib/format/bitrate";
import { durationBetween } from "../../../lib/format/duration";
import { sessionDotClass } from "./SessionRow";

export interface SessionHeroProps {
  session: AdminSession;
  /** The poll's clock, so the duration matches the rest of the page. */
  now: number;
  /** False for a terminal session — §A.3 hides Terminate then. */
  terminable: boolean;
  terminating: boolean;
  onTerminate: () => void;
  onExportTrace: () => void;
  exporting: boolean;
}

function fact(label: string, value: string) {
  return { label, value };
}

export function SessionHero({
  session,
  now,
  terminable,
  terminating,
  onTerminate,
  onExportTrace,
  exporting,
}: SessionHeroProps) {
  const browser = browserMetrics(session);
  const agent = agentMetrics(session);
  const stream = session.stream;

  // `external_*` is present only while the ladder has moved the encoded size off
  // the launch size; absent means "at the launch size", never "unknown".
  const width = stream?.external_width ?? stream?.width;
  const height = stream?.external_height ?? stream?.height;

  const facts = [
    fact("Resolution", width && height ? `${width}×${height}` : "—"),
    fact("Codec", codecDisplayName(normaliseCodec(session.negotiated_codec) ?? stream?.codec) ?? "—"),
    fact("Frame rate", browser?.fps === undefined ? "—" : `${Math.round(browser.fps)} fps`),
    fact("Latency", browser?.rtt_ms === undefined ? "—" : `${Math.round(browser.rtt_ms)} ms`),
    fact("Bitrate", bitrate(agent?.bitrate_kbps)),
    fact("Duration", durationBetween(session.started_at, session.ended_at, now) || "—"),
  ];

  // Full host name here, unlike the list rows: nothing is competing for the
  // width, so there is no reason to strip the `quasar-` prefix.
  const subject = [
    session.username ?? session.user_id.slice(0, 8),
    session.host_name ?? "unassigned",
    session.state,
  ].join(" · ");

  return (
    <div className="card">
      <div
        className="page-head"
        style={{ margin: 0, padding: "var(--card-pad) var(--card-pad) var(--s4)" }}
      >
        <div>
          <div className="rowflex shdr-title">
            <i className={`sdot ${sessionDotClass(session)}`} title={session.state} />
            <h1>{session.app_name ?? "Unnamed app"}</h1>
          </div>
          <div className="sub" style={{ marginTop: 5 }}>
            {subject}
          </div>
        </div>
        <div className="toolbar" style={{ marginBottom: 0 }}>
          <Button variant="ghost" onClick={onExportTrace} disabled={exporting}>
            <IconDownload />
            {exporting ? "Exporting…" : "Export trace"}
          </Button>
          {terminable && (
            <Button variant="danger" onClick={onTerminate} disabled={terminating}>
              {terminating ? "Terminating…" : "Terminate"}
            </Button>
          )}
        </div>
      </div>
      <div
        className="six"
        style={{
          padding: "var(--s4) var(--card-pad) var(--card-pad)",
          borderTop: "1px solid var(--line)",
        }}
      >
        {facts.map((f) => (
          <div key={f.label}>
            <div className="eyebrow">{f.label}</div>
            <div
              className="num"
              style={{ fontSize: "var(--t-lg)", color: "var(--text)", marginTop: 5 }}
            >
              {f.value}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
