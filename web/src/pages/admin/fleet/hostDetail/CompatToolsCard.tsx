/**
 * The host's "Compatibility tools" card (mock #1067, host detail, after Services):
 * each GE-Proton tool installed, installing or failed on this host, and the disk
 * it holds. Read-only: tools are added and removed in Library ▸ Compatibility.
 * Authorization is server-enforced (`admin(...)` on the route); this card is UX only.
 */

import { useEffect } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { HostCompatTools } from "../../../../api/types";
import { Chip } from "../../../../components/Chip";
import { bytes } from "../../../../lib/format/bytes";
import { ResourceStates } from "../../../../components/ResourceStates";
import { useResource } from "../../../../lib/resource/react";
import { toolChip, toolNote, usedBytes } from "../hostCompat";

/** An install changes under the admin's eyes; a chained, visibility-aware poll. */
const POLL_MS = 5_000;

export function CompatToolsCard({ hostId }: { hostId: string }) {
  const res = useResource<HostCompatTools>(
    { label: "compatibility tools", pollMs: POLL_MS, fetch: (ctx) => adminApi.getHostCompatTools(ctx.token, hostId, ctx.signal) },
    [hostId],
  );
  // The poll re-arms after any failure, so a refusal that will not clear (access gone, host
  // deleted) pauses it; a transient failure, on the first load too, keeps its retry.
  const gone = res.error instanceof ApiError && (res.error.status === 403 || res.error.status === 404);
  const { pause } = res;
  useEffect(() => {
    if (gone) pause();
  }, [gone, pause]);
  const data = res.data;

  if (!data) {
    return res.errorMessage ? (
      <div className="card host-services">
        <div className="panel-head">
          <span className="panel-title">Compatibility tools</span>
        </div>
        <div className="host-services-note">
          <p className="note warn">{res.errorMessage}</p>
        </div>
      </div>
    ) : null;
  }

  const used = usedBytes(data.tools);

  return (
    <div className="card host-services">
      <div className="panel-head">
        <div>
          <span className="panel-title">Compatibility tools</span>
          <div className="hint host-services-hint">
            Installed on this host. Adding a tool in Library ▸ Compatibility installs it on every host.
          </div>
        </div>
        {used > 0 && (
          <div className="acts">
            <Chip>{bytes(used)} used</Chip>
          </div>
        )}
      </div>

      {res.errorMessage && (
        <div className="host-services-note">
          <ResourceStates loading={false} error={res.errorMessage} />
        </div>
      )}
      {!data.supported && (
        <div className="host-services-note">
          <p className="note">This host's agent does not offer compatibility tools. Update it from Fleet ▸ Releases.</p>
        </div>
      )}

      <div className="table-wrap">
        <table className="qtable">
          <thead>
            <tr>
              <th>Tool</th>
              <th>State</th>
              <th className="right">Disk</th>
            </tr>
          </thead>
          <tbody>
            {data.tools.length === 0 && (
              <tr className="qtable-empty-row">
                <td colSpan={3}>No tools installed on this host.</td>
              </tr>
            )}
            {data.tools.map((t, i) => {
              const chip = toolChip(t);
              const note = toolNote(t, data.free_bytes);
              return (
                <tr key={`${t.tool_id ?? "unlisted"}-${t.tag}-${i}`}>
                  <td>
                    <div className="stack">
                      <span className="primary">{t.tag}</span>
                      {t.steam_name && t.steam_name !== t.tag && <span className="sub">{t.steam_name}</span>}
                    </div>
                  </td>
                  <td>
                    <div className="stack">
                      <span>
                        <Chip variant={chip.variant} dot={chip.dot}>{chip.label}</Chip>
                      </span>
                      {note && (
                        <span className="sub">
                          {note}
                          {t.state === "failed" && t.run_id && (
                            <>
                              {" "}
                              <Link
                                className="cap-link"
                                to="/admin/fleet/jobs"
                                aria-label={`See the Compatibility tools job for ${t.tag}`}
                              >
                                See the job
                              </Link>
                            </>
                          )}
                        </span>
                      )}
                    </div>
                  </td>
                  <td className="right num">{t.state === "installed" && t.bytes != null ? bytes(t.bytes) : "—"}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <div className="card-pad host-services-foot">
        <p className="hint">Valve's own Proton builds are downloaded by Steam in each home and are not listed here.</p>
      </div>
    </div>
  );
}
