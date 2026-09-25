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

export function homeSeedLabel(seed: AdminSession["home_seed"]): string {
  if (!seed) return "No verified initial home outcome";
  switch (seed.mode) {
    case "reflink": return seed.reason === "seeded" ? "Reflink clone completed" : "Unknown home outcome";
    case "copy": return seed.reason === "seeded" ? "Full copy completed · no reflink storage saving" : "Unknown home outcome";
    case "existing": return seed.reason === "existing_home" ? "Existing home preserved" : "Unknown home outcome";
    case "cold": return ["template_unavailable", "source_disabled", "host_templates_disabled", "host_setting_invalid", "policy_unavailable", "storage_unavailable", "clone_failed", "policy_changed"].includes(seed.reason)
      ? `Cold start · ${seed.reason.replaceAll("_", " ")} · no reflink storage saving`
      : "Unknown home outcome";
    default: return "Unknown home outcome";
  }
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
      <div className="page-head shdr-head">
        <div>
          <div className="rowflex shdr-title">
            <i className={`sdot ${sessionDotClass(session)}`} title={session.state} />
            <h1>{session.app_name ?? "Unnamed app"}</h1>
          </div>
          <div className="sub">
            {subject}
          </div>
        </div>
        <div className="toolbar">
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
      <div className="six shdr-facts">
        {facts.map((f) => (
          <div key={f.label}>
            <div className="eyebrow">{f.label}</div>
            <div className="num t-lg text-1 mt1">
              {f.value}
            </div>
          </div>
        ))}
      </div>
      <div className="sub shdr-home">
        Initial managed home · {homeSeedLabel(session.home_seed)}
      </div>
    </div>
  );
}
