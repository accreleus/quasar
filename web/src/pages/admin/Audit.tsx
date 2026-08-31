// Audit log (handoff-v3-spec §A.20): segments, range, expandable console
// readouts, client-side CSV export.
// Not a section page (CLAUDE.md UI-work note) — it renders its own PageHeader.

import { useEffect, useMemo, useRef, useState } from "react";
import * as adminApi from "../../api/admin";
import type { AdminActivityItem, AdminActivityResponse } from "../../api/admin";
import { Button } from "../../components/Button";
import { IconDownload, IconSearch } from "../../components/icons";
import { PageHeader } from "../../components/PageHeader";
import { ResourceStates } from "../../components/ResourceStates";
import { SegmentedControl } from "../../components/SegmentedControl";
import { useAdminAction } from "../../lib/resource/action";
import { useResource } from "../../lib/resource/react";
import "../../styles/admin/audit.css";
import { AuditRow } from "./audit/AuditRow";
import { toCsv } from "./audit/auditCsv";
import {
  applySegment,
  queryFor,
  segmentCounts,
  type AuditRange,
  type AuditSegment,
} from "./audit/auditFilters";
import { groupByDay } from "./audit/dayGroups";

const SEARCH_DEBOUNCE_MS = 300;

function downloadCsv(csv: string): void {
  const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `quasar-audit-${new Date().toISOString().slice(0, 10)}.csv`;
  a.click();
  // Revoking synchronously can race the browser's own download kick-off in
  // some engines (the click has been dispatched but not yet acted on) and
  // cancel it. A 0ms timeout just needs to run after the current task.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function Audit() {
  const [segment, setSegment] = useState<AuditSegment>("all");
  const [range, setRange] = useState<AuditRange>("24h");
  const [rawQuery, setRawQuery] = useState("");
  const [query, setQuery] = useState("");
  const [expandedIds, setExpandedIds] = useState<Set<number>>(new Set());

  useEffect(() => {
    const t = setTimeout(() => setQuery(rawQuery.trim()), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [rawQuery]);

  // The range's `since` is computed once per [range, query] and reused for
  // every cursor page of that same query. Recomputing `since` fresh on each
  // "Load more" (relative to the then-current wall clock) would slide the
  // window a "Last 24 hours" meant when the page opened, out from under a
  // cursor issued against the earlier one.
  const sinceAnchor = useMemo(() => new Date(), [range, query]);

  const res = useResource<AdminActivityResponse>(
    {
      label: "audit log",
      initialData: { items: [], next_cursor: null },
      fetch: (ctx) =>
        adminApi.listAdminActivity(ctx.token, queryFor({ q: query, range }, sinceAnchor), ctx.signal),
    },
    [query, range],
  );

  const items = res.data?.items ?? [];
  const nextCursor = res.data?.next_cursor ?? null;

  // A synchronous ref, not React state: a rapid double-click can fire twice
  // before a `disabled` prop driven by state has re-rendered, and each call
  // would otherwise fetch and append the same cursor page.
  const loadingMoreRef = useRef(false);
  const loadMore = useAdminAction<[], void>(
    async () => {
      if (nextCursor == null || loadingMoreRef.current) return;
      loadingMoreRef.current = true;
      try {
        await res.mutate(
          (ctx) =>
            adminApi.listAdminActivity(
              ctx.token,
              { ...queryFor({ q: query, range }, sinceAnchor), cursor: nextCursor },
              ctx.signal,
            ),
          (data, result) => ({
            items: [...data.items, ...result.items],
            next_cursor: result.next_cursor,
          }),
        );
      } finally {
        loadingMoreRef.current = false;
      }
    },
    { failure: "could not load more audit entries" },
  );

  const counts = useMemo(() => segmentCounts(items), [items]);
  const visible = useMemo(() => applySegment(items, segment), [items, segment]);
  const groups = useMemo(() => groupByDay(visible), [visible]);

  const allExpanded =
    visible.length > 0 && visible.every((item) => expandedIds.has(item.id));

  function toggleRow(id: number) {
    setExpandedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function toggleExpandAll() {
    setExpandedIds(allExpanded ? new Set() : new Set(visible.map((item) => item.id)));
  }

  return (
    <section className="page admin-audit-page">
      <PageHeader
        title="Audit log"
        sub="Who changed what, and what the system did about it"
        actions={
          <Button variant="ghost" onClick={() => downloadCsv(toCsv(visible))}>
            <IconDownload />
            Export CSV
          </Button>
        }
      />

      <div className="toolbar">
        <SegmentedControl
          aria-label="Filter audit entries"
          value={segment}
          onChange={setSegment}
          options={[
            { value: "all", label: "All" },
            { value: "operator", label: "Operator" },
            { value: "system", label: "System" },
            {
              value: "errors",
              label: (
                <>
                  Errors <span className="num" style={{ opacity: 0.7 }}>{counts.errors}</span>
                </>
              ),
            },
          ]}
        />
        <div className="search">
          <IconSearch />
          <input
            type="search"
            aria-label="Filter by actor, action or target"
            placeholder="Filter by actor, action or target"
            value={rawQuery}
            onChange={(e) => setRawQuery(e.target.value)}
          />
        </div>
        <select
          className="select"
          aria-label="Time range"
          value={range}
          onChange={(e) => setRange(e.target.value as AuditRange)}
        >
          <option value="24h">Last 24 hours</option>
          <option value="7d">Last 7 days</option>
          <option value="30d">Last 30 days</option>
        </select>
        <div className="right">
          <Button variant="ghost" size="sm" onClick={toggleExpandAll}>
            {allExpanded ? "Collapse all" : "Expand all"}
          </Button>
        </div>
      </div>

      <ResourceStates
        loading={res.loading}
        error={res.errorMessage}
        isEmpty={visible.length === 0}
        empty={items.length === 0 ? "No entries in this range." : "No entries match this filter."}
      />

      {!res.loading && groups.map((group) => (
        <div className="card" key={group.key}>
          <div className="panel-head">
            <span className="eyebrow">{group.label}</span>
            <div className="acts">
              <span className="chip">
                {group.items.length} {group.items.length === 1 ? "entry" : "entries"}
              </span>
            </div>
          </div>
          <div className="table-wrap">
            <table className="qtable audit">
              <thead>
                <tr>
                  <th />
                  <th>Time</th>
                  <th>Actor</th>
                  <th>Action</th>
                  <th>Target</th>
                  <th>Detail</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {group.items.map((item: AdminActivityItem) => (
                  <AuditRow
                    key={item.id}
                    item={item}
                    expanded={expandedIds.has(item.id)}
                    onToggle={() => toggleRow(item.id)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ))}

      {nextCursor != null && (
        <div className="audit-more">
          <Button
            variant="ghost"
            disabled={loadMore.pending != null}
            onClick={() => void loadMore.run()}
          >
            {loadMore.pending != null ? "Loading…" : "Load more"}
          </Button>
        </div>
      )}
    </section>
  );
}
