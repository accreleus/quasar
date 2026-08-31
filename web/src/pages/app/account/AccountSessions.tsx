// /app/account/sessions — what is running under this account right now
// (handoff §A.22). Ending one here is the same DELETE the session view uses.

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import * as libraryApi from "../../../api/library";
import type { App, Session } from "../../../api/types";
import { ApiError } from "../../../api/client";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { ResourceStates } from "../../../components/ResourceStates";
import { Table } from "../../../components/Table";
import type { TableColumn } from "../../../components/Table";
import { useToast } from "../../../components/Toast";
import { useResource } from "../../../lib/resource/react";
import { durationBetween } from "../../../lib/format/duration";
import { useSectionHead } from "../../../components/shell/sectionHead";

/** The states a session can be in while it is still worth ending. */
const ACTIVE_STATES = new Set<Session["state"]>(["pending", "assigned", "starting", "running"]);

interface MySessions {
  sessions: Session[];
  appNames: Map<string, string>;
}

export function AccountSessions() {
  const { token } = useAuth();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const [ending, setEnding] = useState<Set<string>>(new Set());

  useSectionHead({ sub: "Sessions running under your account right now." });

  const res = useResource<MySessions>({
    label: "your sessions",
    fetch: async ({ token: t }) => {
      const [{ items }, { items: apps }] = await Promise.all([
        libraryApi.getMySessions(t),
        libraryApi.listApps(t),
      ]);
      return {
        sessions: items.filter((s) => ACTIVE_STATES.has(s.state)),
        appNames: new Map((apps as App[]).map((a) => [a.id, a.name])),
      };
    },
    initialData: { sessions: [], appNames: new Map() },
  });
  const sessions = res.data?.sessions ?? [];
  const appNames = res.data?.appNames ?? new Map<string, string>();

  async function handleEnd(id: string) {
    if (!token) return;
    setEnding((prev) => new Set(prev).add(id));
    try {
      await libraryApi.stopSession(token, id);
      addToast({ variant: "success", title: "Session ended" });
      void res.refresh();
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not end session",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setEnding((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }
  }

  const columns: TableColumn<Session>[] = [
    {
      key: "app",
      header: "App",
      render: (s) => (
        <span className="primary">{appNames.get(s.app_id) ?? s.app_id.slice(0, 8)}</span>
      ),
    },
    {
      key: "host",
      header: "Host",
      render: (s) => <span className="sub mono">{s.host_id ? s.host_id.slice(0, 8) : "—"}</span>,
      width: "140px",
    },
    {
      key: "duration",
      header: "Duration",
      width: "110px",
      render: (s) => (
        <span className="num ac-dur">
          {durationBetween(s.started_at, s.ended_at) || "—"}
        </span>
      ),
    },
    {
      key: "actions",
      header: "",
      width: "170px",
      render: (s) => (
        <div className="cell-actions">
          {s.state === "running" && (
            <Button variant="ghost" size="sm" onClick={() => navigate(`/app/session/${s.id}`)}>
              Resume
            </Button>
          )}
          <Button
            variant="danger"
            size="sm"
            disabled={ending.has(s.id)}
            onClick={() => void handleEnd(s.id)}
          >
            {ending.has(s.id) ? "Ending…" : "End"}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <div className="card sec-card">
      <ResourceStates loading={res.loading} error={res.errorMessage} />

      {!res.loading && !res.errorMessage && sessions.length === 0 && (
        <div className="empty">
          <h3>Nothing running</h3>
          <p>Sessions you start from the library appear here while they are live.</p>
        </div>
      )}

      {sessions.length > 0 && (
        <Table columns={columns} rows={sessions} rowKey={(s) => s.id} />
      )}
    </div>
  );
}
