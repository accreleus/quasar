// Session oversight (handoff §A.2): every stream on the fleet, live and recent.

import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

import * as adminApi from "../../api/admin";
import { ACTIVE_SESSION_STATES } from "../../api/sessionStates";
import type { AdminSession } from "../../api/types";
import { Button } from "../../components/Button";
import { IconRefresh } from "../../components/icons";
import { Modal } from "../../components/Modal";
import { PageHeader } from "../../components/PageHeader";
import { ResourceStates } from "../../components/ResourceStates";
import { useFleetContext } from "../../lib/fleet/FleetContext";
import { browserMetrics } from "../../lib/fleet/sessionMetrics";
import { useAdminAction } from "../../lib/resource/action";
import { useResource } from "../../lib/resource/react";
import { fpsSeriesKey } from "./overview/LiveSessionsCard";
import { seriesFor, useSeries } from "./overview/history";
import { SessionRow } from "./sessions/SessionRow";
import { SessionsToolbar } from "./sessions/SessionsToolbar";
import {
  filterSessions,
  segmentCounts,
  type SegmentCounts,
  type SessionSegment,
} from "./sessions/sessionFilters";
import { sortSessions, type SessionSortKey, type SortDir } from "./sessions/sessionSort";

const POLL_MS = 5000;

interface SortableHeader {
  key: SessionSortKey;
  label: string;
}

export function Sessions() {
  const navigate = useNavigate();
  const { hosts } = useFleetContext();

  const [segment, setSegment] = useState<SessionSegment>("all");
  const [query, setQuery] = useState("");
  const [hostId, setHostId] = useState("");
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [sortKey, setSortKey] = useState<SessionSortKey | null>(null);
  const [sortDir, setSortDir] = useState<SortDir>("asc");
  const [stopTarget, setStopTarget] = useState<AdminSession | null>(null);

  // Each segment is served by its own wire filter rather than by narrowing one
  // page of `all`, so a busy fleet can push neither a live nor a failed session
  // off the page.
  const listState =
    segment === "live" ? "active" : segment === "failed" ? "failed" : "all";
  const res = useResource(
    {
      label: "sessions",
      pollMs: POLL_MS,
      initialData: [] as AdminSession[],
      fetch: async (ctx) =>
        (await adminApi.listAllSessions(ctx.token, undefined, { state: listState })).items,
    },
    [listState],
  );
  const sessions = res.data ?? [];

  // The switch owns the timer, not the mount: re-applied when the resource is
  // rebuilt on a segment change, and both calls are no-ops when already there.
  // A segment switch therefore costs one load even while paused — the new
  // resource loads on start(), before this effect can pause it.
  useEffect(() => {
    if (autoRefresh) res.resume();
    else res.pause();
  }, [autoRefresh, res.pause, res.resume]);

  const stop = useAdminAction<[AdminSession], void>(
    async (session) => {
      await res.mutate((ctx) => adminApi.forceStopSession(ctx.token, session.id));
      await res.refresh({ silent: true });
    },
    { failure: "could not stop session", onSuccess: () => setStopTarget(null) },
  );
  const stopping = stop.pending?.[0].id ?? null;

  // One point per applied load — `latest_metrics` carries a level, so a trend
  // is only what this page has watched since it was opened.
  const series = useSeries(
    Object.fromEntries(sessions.map((s) => [fpsSeriesKey(s.id), browserMetrics(s)?.fps])),
    res.updatedAt,
  );

  const visible = useMemo(
    () => sortSessions(filterSessions(sessions, { segment, q: query, hostId }), sortKey, sortDir),
    [sessions, segment, query, hostId, sortKey, sortDir],
  );

  // A narrowed segment holds only its own rows, so only its own count is
  // current. The other two are held from the last `all` read: a moment stale,
  // but a blank there would read as "none".
  const counts = useSegmentCounts(sessions, listState);

  function handleSort(key: SessionSortKey) {
    if (key !== sortKey) {
      setSortKey(key);
      setSortDir("asc");
    } else {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    }
  }

  const now = res.updatedAt ?? Date.now();
  const empty = visible.length === 0;

  return (
    <section className="page">
      <PageHeader
        title="Sessions"
        sub="Every stream on the fleet, live and recent"
        actions={
          <Button variant="ghost" onClick={() => void res.refresh()}>
            <IconRefresh />
            Refresh
          </Button>
        }
      />

      <SessionsToolbar
        segment={segment}
        onSegment={setSegment}
        counts={counts}
        query={query}
        onQuery={setQuery}
        hosts={hosts}
        hostId={hostId}
        onHostId={setHostId}
        autoRefresh={autoRefresh}
        onAutoRefresh={setAutoRefresh}
      />

      <div className="card">
        <ResourceStates loading={res.loading} error={res.errorMessage} />
        {!res.loading && empty ? (
          <div className="empty">
            <h3>No sessions</h3>
            <p>Sessions appear here once users start streaming.</p>
          </div>
        ) : (
          <div className="table-wrap">
            <table className="qtable">
              <thead>
                <tr>
                  {/* The state moved into the Session cell's sub-label in v3, so
                      the state sort rides that column's header. */}
                  <SortTh
                    header={{ key: "state", label: "Session" }}
                    sortKey={sortKey}
                    sortDir={sortDir}
                    onSort={handleSort}
                  />
                  <SortTh
                    header={{ key: "user", label: "User" }}
                    sortKey={sortKey}
                    sortDir={sortDir}
                    onSort={handleSort}
                  />
                  {/* The mock stacks a GPU name here; the wire carries no GPU
                      on a session, so the column is Host alone (§9 omission). */}
                  <th>Host</th>
                  <th className="right">FPS</th>
                  <th>Trend</th>
                  <th className="right">Latency</th>
                  <th className="right">Bitrate</th>
                  <th>Codec</th>
                  {/* Duration is start time seen from now, so it sorts on it. */}
                  <SortTh
                    header={{ key: "started", label: "Duration" }}
                    sortKey={sortKey}
                    sortDir={sortDir}
                    onSort={handleSort}
                    align="right"
                  />
                  <th className="right" />
                </tr>
              </thead>
              <tbody>
                {visible.map((session) => (
                  <SessionRow
                    key={session.id}
                    session={session}
                    points={seriesFor(series, fpsSeriesKey(session.id))}
                    now={now}
                    onOpen={() => navigate(`/admin/sessions/${session.id}`)}
                    onTerminate={() => setStopTarget(session)}
                    terminable={ACTIVE_SESSION_STATES.has(session.state)}
                    terminating={stopping === session.id}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {stopTarget && (
        <Modal
          open
          onClose={() => setStopTarget(null)}
          title="Terminate session"
          footer={
            <>
              <Button variant="ghost" onClick={() => setStopTarget(null)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={stopping === stopTarget.id}
                onClick={() => void stop.run(stopTarget)}
              >
                {stopping === stopTarget.id ? "Terminating…" : "Terminate"}
              </Button>
            </>
          }
        >
          <p className="sec">
            Terminate session <strong>{stopTarget.id.slice(0, 8)}</strong>? This immediately ends
            the session and frees its GPU slot.
          </p>
        </Modal>
      )}
    </section>
  );
}

/**
 * Counts for the segmented control. The current segment's own count is always
 * live; the others are held from the last unfiltered read, so switching to Live
 * does not blank All and Failed.
 */
function useSegmentCounts(
  sessions: readonly AdminSession[],
  listState: "all" | "active" | "failed",
): SegmentCounts {
  const held = useRef<SegmentCounts>({ all: 0, live: 0, failed: 0 });
  const counted = segmentCounts(sessions);
  if (listState === "all") {
    held.current = counted;
    return counted;
  }
  // The server classified every held row, so the narrowed segment's count is
  // the row count — not a re-derivation from `state`.
  if (listState === "active") return { ...held.current, live: sessions.length };
  return { ...held.current, failed: sessions.length };
}

function SortTh({
  header,
  sortKey,
  sortDir,
  onSort,
  align,
}: {
  header: SortableHeader;
  sortKey: SessionSortKey | null;
  sortDir: SortDir;
  onSort: (key: SessionSortKey) => void;
  align?: "right";
}) {
  const active = sortKey === header.key;
  return (
    <th
      className={align === "right" ? "right" : undefined}
      aria-sort={active ? (sortDir === "asc" ? "ascending" : "descending") : "none"}
    >
      <button type="button" className="th-sort-btn" onClick={() => onSort(header.key)}>
        {header.label}
        <span className="th-sort-ind" aria-hidden="true">
          {active ? (sortDir === "asc" ? "▲" : "▼") : ""}
        </span>
      </button>
    </th>
  );
}
