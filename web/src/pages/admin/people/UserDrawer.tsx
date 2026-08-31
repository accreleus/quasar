// Session-history drawer for a single user, opened from the UsersTab table.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import type { AdminSession, AdminUser, Entitlement } from "../../../api/types";
import { Chip } from "../../../components/Chip";
import { Button } from "../../../components/Button";
import { Drawer } from "../../../components/Drawer";
import { bytes } from "../../../lib/format/bytes";
import { fmtDate } from "../../../lib/formatLegacy";
import { avatarGradient, fmtDuration, sessionStateDot } from "./format";

/** How a grant's `granted_by` reads to an operator (mirrors the app editor's
 *  Access tab). */
const GRANTED_BY_LABEL: Record<Entitlement["granted_by"], string> = {
  admin: "Granted by an admin",
  provider: "From a library sync",
  migration: "Carried over automatically",
};

interface UserDrawerProps {
  user: AdminUser;
  token: string;
  storageBytes?: number;
  onClose: () => void;
  onRoleClick: (u: AdminUser) => void;
  onDisableClick: (u: AdminUser) => void;
}

export function UserDrawer({
  user,
  token,
  storageBytes,
  onClose,
  onRoleClick,
  onDisableClick,
}: UserDrawerProps) {
  const [sessions, setSessions] = useState<AdminSession[]>([]);
  const [loading, setLoading] = useState(true);

  // steam-library-discovery spec §6.6: the per-user entitlement view.
  // GET /v1/admin/users/{id}/entitlements answers "what was granted to this
  // person personally" — it deliberately excludes 'all' rows (§6.3/§6.6), so
  // an empty list here does NOT mean this user sees nothing. That distinction
  // is made explicit in the copy below rather than left for an admin to infer.
  const [entitlements, setEntitlements] = useState<Entitlement[]>([]);
  const [entitlementsLoading, setEntitlementsLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    adminApi
      .listAllSessions(token)
      .then(({ items }) => {
        setSessions(items.filter((s) => s.user_id === user.id));
      })
      .catch(() => { /* show empty */ })
      .finally(() => setLoading(false));
  }, [token, user.id]);

  useEffect(() => {
    setEntitlementsLoading(true);
    adminApi
      .listUserEntitlements(token, user.id)
      .then(({ items }) => setEntitlements(items))
      .catch(() => setEntitlements([]))
      .finally(() => setEntitlementsLoading(false));
  }, [token, user.id]);

  const totalSessions = sessions.length;
  const totalMins = sessions.reduce((acc, s) => {
    if (!s.started_at) return acc;
    const end = s.ended_at ? new Date(s.ended_at).getTime() : Date.now();
    return acc + (end - new Date(s.started_at).getTime()) / 60000;
  }, 0);

  return (
    <Drawer open onClose={onClose} title={user.username} width={440}>
      {/* User identity */}
      <div className="drawer-user-head">
        <span className="u-avatar u-avatar-lg" style={{ background: avatarGradient(user.username) }}>
          {user.username[0].toUpperCase()}
        </span>
        <div>
          <div className="u-name" style={{ fontSize: "1.1rem" }}>{user.username}</div>
          <div className="u-email">{user.email}</div>
          <div style={{ display: "flex", gap: "var(--s2)", marginTop: "var(--s2)" }}>
            <Chip variant={user.role === "admin" ? "accent" : "neutral"}>
              {user.role === "admin" ? "Admin" : "User"}
            </Chip>
            <Chip variant={user.disabled ? "warning" : "success"} dot={!user.disabled}>
              {user.disabled ? "Disabled" : "Active"}
            </Chip>
          </div>
        </div>
      </div>

      {/* Fact rows */}
      <div className="ufact-list">
        <div className="ufact">
          <span className="l">Total sessions</span>
          <span className="v mono">{totalSessions}</span>
        </div>
        <div className="ufact">
          <span className="l">Hours streamed</span>
          <span className="v mono">{(totalMins / 60).toFixed(1)} h</span>
        </div>
        <div className="ufact">
          <span className="l">Session limit</span>
          <span className="v mono">{user.max_concurrent_sessions}</span>
        </div>
        {storageBytes !== undefined && (
          <div className="ufact">
            <span className="l">Storage used</span>
            <span className="v mono">{bytes(storageBytes)}</span>
          </div>
        )}
        <div className="ufact">
          <span className="l">Joined</span>
          <span className="v mono">{fmtDate(user.created_at)}</span>
        </div>
      </div>

      {/* Personal library grants (spec §6.6) — PERSONAL grants only, never the
          'all' rows that also cover this user. Said explicitly, twice: once as
          a standing caption, once in the empty state, because the failure mode
          is an admin reading "no rows" as "this user can see nothing". */}
      <div className="eyebrow" style={{ marginTop: "var(--s6)", marginBottom: "var(--s3)" }}>
        Personal library grants
      </div>
      <p className="muted" style={{ fontSize: "var(--t-xs)", marginTop: "-6px", marginBottom: "var(--s3)" }}>
        Individual grants only — not apps this user can see because they are marked
        “Everyone”. Manage those from each app’s Access section.
      </p>

      {entitlementsLoading && (
        <p className="muted" style={{ fontSize: "var(--t-sm)" }}>Loading…</p>
      )}
      {!entitlementsLoading && entitlements.length === 0 && (
        <p className="muted" style={{ fontSize: "var(--t-sm)" }}>
          No personal grants. This user may still see apps marked “Everyone”.
        </p>
      )}
      {!entitlementsLoading &&
        entitlements.map((e) => (
          <div key={e.id} className="hist-item">
            <div style={{ flex: 1 }}>
              <div className="hi-app">
                <Link to={`/admin/library/apps/${e.app_id}`}>{e.app_name}</Link>
              </div>
              <div className="hi-meta">{GRANTED_BY_LABEL[e.granted_by]}</div>
            </div>
          </div>
        ))}

      {/* Session history */}
      <div className="eyebrow" style={{ marginTop: "var(--s6)", marginBottom: "var(--s3)" }}>
        Session history
      </div>

      {loading && <p className="muted" style={{ fontSize: "var(--t-sm)" }}>Loading…</p>}
      {!loading && sessions.length === 0 && (
        <p className="muted" style={{ fontSize: "var(--t-sm)" }}>No sessions yet.</p>
      )}
      {!loading &&
        sessions.map((s) => (
          <div key={s.id} className="hist-item">
            <span className="hist-dot" style={{ background: sessionStateDot(s.state) }} />
            <div style={{ flex: 1 }}>
              <div className="hi-app">{s.app_id.slice(0, 12)}</div>
              <div className="hi-meta">
                {s.id.slice(0, 8)} · {s.state}
                {s.state_detail ? ` · ${s.state_detail}` : ""}
                {s.started_at ? ` · ${fmtDuration(s.started_at, s.ended_at)}` : ""}
              </div>
            </div>
          </div>
        ))}

      {/* Actions */}
      <div style={{ display: "flex", flexDirection: "column", gap: "var(--s2)", marginTop: "var(--s6)" }}>
        <div style={{ display: "flex", gap: "var(--s2)" }}>
          <Button variant="secondary" style={{ flex: 1 }} onClick={() => onRoleClick(user)}>
            {user.role === "admin" ? "Demote" : "Promote"}
          </Button>
          <Button variant="secondary" style={{ flex: 1 }} onClick={() => onDisableClick(user)}>
            {user.disabled ? "Enable" : "Disable"}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}
