// The home page's three non-grid surfaces: the post-session summary card, the
// bare states (failed / empty catalogue, empty filter) and the stop-session
// confirmation. Presentational — every decision is the page's.

import type { ReactNode } from "react";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import type { SessionSummary } from "../sessionSummary";

/** The card the session route hands back on the way out. */
export function SessionSummaryCard({
  summary,
  onDismiss,
}: {
  summary: SessionSummary;
  onDismiss: () => void;
}) {
  return (
    <section className="session-summary" aria-labelledby="session-summary-title">
      <div>
        <span className="eyebrow">Session complete</span>
        <h2 id="session-summary-title">{summary.appName}</h2>
        <p>{summary.recommendation}</p>
      </div>
      <dl>
        <div>
          <dt>Duration</dt>
          <dd>
            {Math.floor(summary.durationSeconds / 60)}m {summary.durationSeconds % 60}s
          </dd>
        </div>
        <div>
          <dt>FPS p50 / p95</dt>
          <dd>
            {summary.fpsP50?.toFixed(0) ?? "–"} / {summary.fpsP95?.toFixed(0) ?? "–"}
          </dd>
        </div>
        <div>
          <dt>Latency p50 / p95</dt>
          <dd>
            {summary.latencyP50Ms?.toFixed(0) ?? "–"} / {summary.latencyP95Ms?.toFixed(0) ?? "–"} ms
          </dd>
        </div>
        <div>
          <dt>End reason</dt>
          <dd>{summary.endReason}</dd>
        </div>
      </dl>
      <Button variant="ghost" onClick={onDismiss}>
        Dismiss summary
      </Button>
    </section>
  );
}

export interface HomeStateProps {
  tone: "error" | "empty";
  title: string;
  body: ReactNode;
  action?: ReactNode;
}

/**
 * The one panel every not-a-library state renders through (failed/empty
 * catalogue, empty filter). `role="alert"` for the failure (news, arrives after
 * the page settles); `role="status"` for empty states.
 */
export function HomeState({ tone, title, body, action }: HomeStateProps) {
  return (
    <div className="home-state" data-tone={tone} role={tone === "error" ? "alert" : "status"}>
      <h3>{title}</h3>
      <p>{body}</p>
      {action && <div className="home-state-actions">{action}</div>}
    </div>
  );
}

export function StopSessionModal({
  stopping,
  onCancel,
  onConfirm,
}: {
  stopping: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <Modal
      open
      onClose={onCancel}
      title="Stop session"
      footer={
        <>
          <Button variant="ghost" onClick={onCancel} disabled={stopping}>
            Cancel
          </Button>
          <Button variant="danger" disabled={stopping} onClick={onConfirm}>
            {stopping ? "Stopping…" : "Stop session"}
          </Button>
        </>
      }
    >
      <p className="sec">Stop this session? Your progress since the last save will be lost.</p>
    </Modal>
  );
}

export interface LibraryStatesProps {
  loading: boolean;
  /** The catalogue fetch's own words, or null. */
  error: string | null;
  /** Whether the catalogue holds anything at all, and how much of it survived
   *  the filter — three different sentences. */
  total: number;
  matched: number;
  search: string;
  /** True when a kind segment other than "All" is narrowing the grid. */
  filtered: boolean;
  isAdmin: boolean;
  onRetry: () => void;
  onAddApps: () => void;
  onClearFilter: () => void;
}

/**
 * Loading, and the three bare states the grid can be replaced by (§2.7): a
 * failed catalogue, an empty one, and a filter that matched nothing. Each one
 * says what happened and offers the action that resolves it — a single grey
 * sentence was a dead end, and the empty case gave an admin (the one person who
 * can fix it) the same dead end it gave everybody else.
 */
export function LibraryStates({
  loading,
  error,
  total,
  matched,
  search,
  filtered,
  isAdmin,
  onRetry,
  onAddApps,
  onClearFilter,
}: LibraryStatesProps) {
  if (loading) return <p className="muted home-loading">Loading…</p>;
  if (error) {
    return (
      <HomeState
        tone="error"
        title="Your library didn't load"
        body={<>The server didn't return your catalogue. It reported: “{error}”.</>}
        action={
          <Button variant="primary" onClick={onRetry}>
            Try again
          </Button>
        }
      />
    );
  }
  if (total === 0) {
    // Role-varied: an admin can add apps, a user can only ask. The /admin link
    // is UX only — the admin API enforces role regardless (CLAUDE.md #6).
    return (
      <HomeState
        tone="empty"
        title="Your library is empty"
        body={
          isAdmin
            ? "No apps are in the catalogue yet. Add one in the admin area and it shows up here."
            : "No apps have been added yet. Ask an admin for the ones you want to play."
        }
        action={
          isAdmin ? (
            <Button variant="primary" onClick={onAddApps}>
              Add apps
            </Button>
          ) : null
        }
      />
    );
  }
  if (matched === 0) {
    return (
      <HomeState
        tone="empty"
        title="Nothing matches"
        body={
          search
            ? `No app matches “${search}” in this filter.`
            : "No app in the library has this kind."
        }
        action={
          filtered ? (
            <Button variant="ghost" onClick={onClearFilter}>
              Show all apps
            </Button>
          ) : null
        }
      />
    );
  }
  return null;
}
