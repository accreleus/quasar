/**
 * The Sessions toolbar (handoff §A.2): segment, search, host, auto-refresh.
 *
 * Presentational — every value and every setter comes from the page, so the
 * filter state has one home and the toolbar can be rendered in a test without
 * a router, a token or a poll.
 */

import type { Host } from "../../../api/types";
import { IconSearch } from "../../../components/icons";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { shortHost } from "../overview/LiveSessionsCard";
import type { SegmentCounts, SessionSegment } from "./sessionFilters";

export interface SessionsToolbarProps {
  segment: SessionSegment;
  onSegment: (segment: SessionSegment) => void;
  counts: SegmentCounts;
  query: string;
  onQuery: (query: string) => void;
  hosts: readonly Host[];
  hostId: string;
  onHostId: (hostId: string) => void;
  autoRefresh: boolean;
  onAutoRefresh: (on: boolean) => void;
}

/** The mock's mono count beside a segment label. */
function Count({ n }: { n: number }) {
  return (
    <span className="num" style={{ opacity: 0.7 }}>
      {n}
    </span>
  );
}

export function SessionsToolbar({
  segment,
  onSegment,
  counts,
  query,
  onQuery,
  hosts,
  hostId,
  onHostId,
  autoRefresh,
  onAutoRefresh,
}: SessionsToolbarProps) {
  return (
    <div className="toolbar">
      {/* Manual activation: switching segment changes the `state` the list is
          fetched with, and automatic activation would fire one request per
          arrow keystroke. */}
      <SegmentedControl<SessionSegment>
        activation="manual"
        aria-label="Filter sessions"
        value={segment}
        onChange={onSegment}
        options={[
          { value: "all", label: <>All <Count n={counts.all} /></>, ariaLabel: "All sessions" },
          { value: "live", label: <>Live <Count n={counts.live} /></>, ariaLabel: "Live sessions" },
          { value: "failed", label: <>Failed <Count n={counts.failed} /></>, ariaLabel: "Failed sessions" },
        ]}
      />

      <div className="search">
        <IconSearch />
        <input
          placeholder="Filter by user, app or host"
          aria-label="Filter by user, app or host"
          value={query}
          onChange={(e) => onQuery(e.target.value)}
        />
      </div>

      <select
        className="select"
        aria-label="Filter by host"
        value={hostId}
        onChange={(e) => onHostId(e.target.value)}
      >
        <option value="">All hosts</option>
        {hosts.map((host) => (
          <option key={host.id} value={host.id}>
            {shortHost(host.node_name)}
          </option>
        ))}
      </select>

      <div className="right">
        <span style={{ fontSize: "var(--t-xs)", color: "var(--text-3)" }}>Auto-refresh</span>
        <button
          type="button"
          className="switch"
          role="switch"
          aria-checked={autoRefresh}
          aria-label="Auto-refresh"
          onClick={() => onAutoRefresh(!autoRefresh)}
        />
      </div>
    </div>
  );
}
