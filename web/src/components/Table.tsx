/**
 * Table — qtable + table-wrap primitives (UI-04).
 * Renders a data table using design-system tokens.
 */
import { Fragment } from "react";
import type { CSSProperties, ReactNode } from "react";

// Chevron used by the optional expand/collapse column. Rotates 90deg when open
// (host-observability round 2 — no dedicated mockup for expandable table rows;
// extrapolated from the styleguide's inline-SVG, stroke-based icon convention).
function ExpandChevron({ open }: { open: boolean }) {
  return (
    <svg
      className={`qtable-expand-chevron${open ? " open" : ""}`}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      aria-hidden="true"
    >
      <path d="M5 3l6 5-6 5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export type SortDir = "asc" | "desc";

export interface TableColumn<T> {
  key: string;
  header: ReactNode;
  /** Label used when a scoped table collapses into mobile row cards. */
  mobileLabel?: string;
  /** Render the cell for a given row */
  render: (row: T) => ReactNode;
  /** Optional CSS width hint, e.g. "120px" */
  width?: string;
  /** Opt-in: renders a clickable sort control in the header (UI audit #10). */
  sortable?: boolean;
  /** Right-aligns both the header and every cell (`.qtable td.right`/`th.right`
   *  — numeric columns: counts, byte sizes, dates read right-to-left). */
  align?: "right";
}

interface TableProps<T> {
  columns: TableColumn<T>[];
  rows: T[];
  /** Provide a unique key per row */
  rowKey: (row: T) => string;
  /** Optional empty-state content */
  empty?: ReactNode;
  /** Optional row click handler — adds `clickable` class to <tr> */
  onRowClick?: (row: T) => void;
  /** Current sort column key — only meaningful when a column has `sortable`. */
  sortKey?: string | null;
  sortDir?: SortDir;
  /** Called with the clicked column's key when a sortable header is activated. */
  onSort?: (key: string) => void;
  /** Optional per-row inline style, e.g. dimming ended/failed rows. */
  rowStyle?: (row: T) => CSSProperties | undefined;
  /**
   * Opt-in expandable rows: when provided, a leading chevron column toggles a
   * full-width detail row rendered by `renderExpanded`. `isExpanded`/`onToggleExpand`
   * are required alongside it — expand state is owned by the caller (so it can
   * also be driven programmatically, e.g. auto-expanding a row on an action error).
   */
  renderExpanded?: (row: T) => ReactNode;
  isExpanded?: (row: T) => boolean;
  onToggleExpand?: (row: T) => void;
}

export function Table<T>({
  columns,
  rows,
  rowKey,
  empty,
  onRowClick,
  sortKey,
  sortDir,
  onSort,
  rowStyle,
  renderExpanded,
  isExpanded,
  onToggleExpand,
}: TableProps<T>) {
  const expandable = Boolean(renderExpanded);
  return (
    <div className="table-wrap">
      <table className="qtable">
        <thead>
          <tr>
            {expandable && <th className="qtable-expand-th" aria-hidden="true" />}
            {columns.map((col) => {
              if (!col.sortable) {
                return (
                  <th
                    key={col.key}
                    className={col.align === "right" ? "right" : undefined}
                    style={col.width ? { width: col.width } : undefined}
                  >
                    {col.header}
                  </th>
                );
              }
              const active = sortKey === col.key;
              const ariaSort = active ? (sortDir === "asc" ? "ascending" : "descending") : "none";
              return (
                <th
                  key={col.key}
                  className={col.align === "right" ? "right" : undefined}
                  style={col.width ? { width: col.width } : undefined}
                  aria-sort={ariaSort}
                >
                  <button
                    type="button"
                    className="th-sort-btn"
                    onClick={() => onSort?.(col.key)}
                  >
                    {col.header}
                    <span className="th-sort-ind" aria-hidden="true">
                      {active ? (sortDir === "asc" ? "▲" : "▼") : ""}
                    </span>
                  </button>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && empty ? (
            <tr>
              <td
                colSpan={columns.length + (expandable ? 1 : 0)}
                style={{ textAlign: "center", color: "var(--text-3)", padding: "var(--s8)" }}
              >
                {empty}
              </td>
            </tr>
          ) : (
            rows.map((row) => {
              const expanded = expandable && Boolean(isExpanded?.(row));
              return (
                <Fragment key={rowKey(row)}>
                  <tr
                    className={onRowClick ? "clickable" : undefined}
                    onClick={onRowClick ? () => onRowClick(row) : undefined}
                    style={rowStyle?.(row)}
                  >
                    {expandable && (
                      <td className="qtable-expand-cell">
                        <button
                          type="button"
                          className="qtable-expand-btn"
                          aria-expanded={expanded}
                          aria-label={expanded ? "Collapse row" : "Expand row"}
                          onClick={(e) => {
                            e.stopPropagation();
                            onToggleExpand?.(row);
                          }}
                        >
                          <ExpandChevron open={expanded} />
                        </button>
                      </td>
                    )}
                    {columns.map((col) => (
                      <td
                        key={col.key}
                        className={col.align === "right" ? "right" : undefined}
                        data-label={col.mobileLabel ?? (typeof col.header === "string" ? col.header : undefined)}
                      >
                        {col.render(row)}
                      </td>
                    ))}
                  </tr>
                  {expandable && expanded && (
                    <tr className="qtable-expand-row">
                      <td colSpan={columns.length + 1}>{renderExpanded!(row)}</td>
                    </tr>
                  )}
                </Fragment>
              );
            })
          )}
        </tbody>
      </table>
    </div>
  );
}
