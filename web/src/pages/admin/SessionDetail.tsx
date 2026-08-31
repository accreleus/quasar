// Per-session drill-down (handoff §A.3): hero, four charts, the two latest
// cards, and everything else behind the Diagnostics disclosure.

import { useMemo, useState } from "react";
import { useParams } from "react-router-dom";

import * as adminApi from "../../api/admin";
import { ACTIVE_SESSION_STATES } from "../../api/sessionStates";
import type { AdminSession, DiagnosticBundle, MetricPoint } from "../../api/types";
import { useAuth } from "../../auth/context";
import { Breadcrumbs } from "../../components/Breadcrumbs";
import { Button } from "../../components/Button";
import { EmptyState } from "../../components/LoadingState";
import { Modal } from "../../components/Modal";
import { ResourceStates } from "../../components/ResourceStates";
import { downloadJson } from "../../lib/download";
import { useAdminAction } from "../../lib/resource/action";
import { useResource } from "../../lib/resource/react";
import { DiagnosticsDisclosure } from "./sessions/DiagnosticsDisclosure";
import { LatestCards } from "./sessions/LatestCards";
import { SessionCharts } from "./sessions/SessionCharts";
import { SessionHero } from "./sessions/SessionHero";
import { sessionChartSeries, splitBySource } from "./sessions/chartSeries";

const POLL_MS = 5000;
/** ~10 minutes at the 5 s post rate on each side. */
const METRICS_LIMIT = 120;

interface SessionDetailData {
  points: MetricPoint[];
  /** Best-effort: derived from the session list, kept sticky across a failure. */
  session: AdminSession | null;
}

const EMPTY: SessionDetailData = { points: [], session: null };

export function SessionDetail() {
  const { id } = useParams<{ id: string }>();
  const { token } = useAuth();
  const [confirmStop, setConfirmStop] = useState(false);

  // The 5 s poll: metrics, plus the header off the session list (there is no
  // single-session GET on the admin surface). Only the metrics failure is a
  // page error — the header is best-effort and stays sticky via ctx.current.
  const res = useResource<SessionDetailData>(
    {
      label: "metrics",
      pollMs: POLL_MS,
      initialData: EMPTY,
      fetch: async (ctx) => {
        const { items } = await adminApi.getSessionMetrics(ctx.token, id!, { limit: METRICS_LIMIT });

        let session = ctx.current?.session ?? null;
        try {
          const { items: all } = await adminApi.listAllSessions(ctx.token);
          const found = all.find((s) => s.id === id);
          if (found) session = found;
        } catch {
          // best-effort; a header failure is not a page error
        }
        return { points: items, session };
      },
    },
    [id],
  );

  // The page's heaviest read, and every consumer of it lives inside the
  // disclosure: fetched once on first open, never polled. `diagnosticsOpened`
  // latches, so closing the disclosure keeps the bundle for Export trace.
  const [diagnosticsOpened, setDiagnosticsOpened] = useState(false);
  const bundleRes = useResource<DiagnosticBundle | null>(
    {
      label: "diagnostics",
      initialData: null,
      fetch: async (ctx) => (diagnosticsOpened ? adminApi.getDiagnosticBundle(ctx.token, id!) : null),
    },
    [id, diagnosticsOpened],
  );
  const bundle = bundleRes.data ?? null;

  const points = res.data?.points ?? [];
  const session = res.data?.session ?? null;

  /** The last effective-media snapshot the agent posted, if the bundle is in. */
  const effectiveMedia = useMemo(() => {
    const snapshot = [...(bundle?.events ?? [])]
      .reverse()
      .find((event) => event.type === "session.effective_media");
    return (snapshot?.payload as Record<string, unknown> | undefined) ?? null;
  }, [bundle]);

  const stop = useAdminAction<[], void>(
    async () => {
      await res.mutate((ctx) => adminApi.forceStopSession(ctx.token, id!));
      await res.refresh({ silent: true });
    },
    { failure: "could not stop session", onSuccess: () => setConfirmStop(false) },
  );

  const exportTrace = useAdminAction<[], void>(
    async () => {
      // Reuses the bundle the disclosure already loaded; only an export from a
      // page whose Diagnostics were never opened costs a request.
      downloadJson(`session-${id}.json`, bundle ?? (await adminApi.getDiagnosticBundle(token!, id!)));
    },
    { failure: "could not export the trace", success: "Trace exported" },
  );

  const charts = useMemo(() => sessionChartSeries(points), [points]);
  const { agent, browser } = useMemo(() => splitBySource(points), [points]);

  // The series are authoritative for "latest"; `latest_metrics` covers the case
  // where the metrics window has aged out but the list row still carries one.
  const latestAgent =
    agent.length > 0
      ? ({ source: "agent", ts_unix_ms: agent[agent.length - 1].ts, metrics: agent[agent.length - 1].m } as MetricPoint)
      : session?.latest_metrics?.agent;
  const latestBrowser =
    browser.length > 0
      ? ({ source: "browser", ts_unix_ms: browser[browser.length - 1].ts, metrics: browser[browser.length - 1].m } as MetricPoint)
      : session?.latest_metrics?.browser;

  const now = res.updatedAt ?? Date.now();
  const terminable = session ? ACTIVE_SESSION_STATES.has(session.state) : false;

  return (
    <section className="page">
      <Breadcrumbs
        items={[
          { label: "Sessions", to: "/admin/sessions" },
          { label: id?.slice(0, 8) ?? "", mono: true },
        ]}
      />

      <ResourceStates loading={res.loading} error={res.errorMessage} />

      {session && (
        <SessionHero
          session={session}
          now={now}
          terminable={terminable}
          terminating={stop.pending != null}
          onTerminate={() => setConfirmStop(true)}
          onExportTrace={() => void exportTrace.run()}
          exporting={exportTrace.pending != null}
        />
      )}

      {!res.loading && points.length === 0 && (
        <EmptyState>No telemetry yet for this session.</EmptyState>
      )}

      {points.length > 0 && <SessionCharts series={charts} />}

      <LatestCards agent={latestAgent} browser={latestBrowser} now={now} />

      {id && token && (
        <DiagnosticsDisclosure
          sessionId={id}
          token={token}
          session={session}
          effectiveMedia={effectiveMedia}
          points={points}
          onOpen={() => setDiagnosticsOpened(true)}
        />
      )}

      {confirmStop && (
        <Modal
          open
          onClose={() => setConfirmStop(false)}
          title="Terminate session"
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmStop(false)}>
                Cancel
              </Button>
              <Button variant="danger" disabled={stop.pending != null} onClick={() => void stop.run()}>
                {stop.pending ? "Terminating…" : "Terminate"}
              </Button>
            </>
          }
        >
          <p className="sec">
            Terminate this session? This immediately ends the session and frees its GPU slot.
          </p>
        </Modal>
      )}
    </section>
  );
}
