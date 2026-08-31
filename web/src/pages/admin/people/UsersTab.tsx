// Admin user management (handoff §A.18): segmented All/Admins/Disabled
// toolbar, bulk-select, row checkboxes, kebab row menu, session history drawer.
// Authorization is server-enforced — this UI is UX only (CLAUDE.md invariant #6).

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import type { AdminUser } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { ActionsMenu, type ActionsMenuItem } from "../../../components/ActionsMenu";
import { BulkBar } from "../../../components/BulkBar";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconDownload, IconPlus, IconSearch } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { ResourceStates } from "../../../components/ResourceStates";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { TextField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { downloadBlob } from "../../../lib/download";
import { bytes } from "../../../lib/format/bytes";
import { relativeTime } from "../../../lib/format/relativeTime";
import { avatarGradient } from "./format";
import { QuotaModal } from "./QuotaModal";
import { UserDrawer } from "./UserDrawer";
import { usersToCsv, type UserCsvRow } from "./usersCsv";
import { useUsersPage, type UserSegment } from "./useUsersPage";

export function UsersTab() {
  const { token } = useAuth();
  const { addToast } = useToast();
  const navigate = useNavigate();

  const {
    users,
    loading,
    errorMessage,
    storageByUser,
    query,
    setQuery,
    segment,
    setSegment,
    disabledCount,
    selected,
    clearSelected,
    actingOn,
    bulkBusy,
    load,
    filtered,
    selectedInFiltered,
    allFilteredSelected,
    toggleSelect,
    toggleSelectAll,
    handleToggleRole,
    handleToggleDisable,
    handleDelete,
    handleBulkDisable,
    handleBulkDelete,
    handleBulkRole,
    replaceUser,
  } = useUsersPage();

  // Modal / drawer open state (UI concern only — kept in the page component)
  const [roleTarget, setRoleTarget] = useState<AdminUser | null>(null);
  const [disableTarget, setDisableTarget] = useState<AdminUser | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<AdminUser | null>(null);
  const [deleteConfirmText, setDeleteConfirmText] = useState("");
  const [quotaTarget, setQuotaTarget] = useState<AdminUser | null>(null);
  const [drawerUser, setDrawerUser] = useState<AdminUser | null>(null);
  const [bulkDeleteConfirm, setBulkDeleteConfirm] = useState(false);
  const [bulkRoleOpen, setBulkRoleOpen] = useState(false);

  // ── Row actions menu ─────────────────────────────────────────────────────

  function menuItemsFor(u: AdminUser): ActionsMenuItem[] {
    return [
      {
        key: "role",
        label: u.role === "admin" ? "Demote to user" : "Promote to admin",
        disabled: actingOn === u.id,
        onClick: () => setRoleTarget(u),
      },
      {
        key: "disable",
        label: u.disabled ? "Enable" : "Disable",
        disabled: actingOn === u.id,
        onClick: () => setDisableTarget(u),
      },
      { key: "quota", label: "Session quota", onClick: () => setQuotaTarget(u) },
      { key: "history", label: "Session history", onClick: () => setDrawerUser(u) },
      {
        key: "delete",
        label: "Delete",
        variant: "danger",
        disabled: actingOn === u.id,
        onClick: () => {
          setDeleteTarget(u);
          setDeleteConfirmText("");
        },
      },
    ];
  }

  // ── Table columns ──────────────────────────────────────────────────────────

  const columns: TableColumn<AdminUser>[] = [
    {
      key: "select",
      mobileLabel: "",
      width: "34px",
      header: (
        <label className="check" style={{ minHeight: 0 }}>
          <input
            type="checkbox"
            checked={allFilteredSelected}
            onChange={toggleSelectAll}
            aria-label="Select all"
          />
        </label>
      ),
      render: (u) => (
        <label className="check" style={{ minHeight: 0 }} onClick={(e) => e.stopPropagation()}>
          <input
            type="checkbox"
            checked={selected.has(u.id)}
            onChange={() => toggleSelect(u.id)}
            aria-label={`Select ${u.username}`}
          />
        </label>
      ),
    },
    {
      key: "user",
      header: "User",
      render: (u) => (
        <div className="rowflex">
          <span className="u-ava" style={{ background: avatarGradient(u.username) }}>
            {u.username[0].toUpperCase()}
          </span>
          <div className="stack">
            <span className="primary">{u.username}</span>
            <span className="sub">{u.email}</span>
          </div>
        </div>
      ),
    },
    {
      key: "role",
      header: "Role",
      width: "90px",
      render: (u) => (
        <Chip variant={u.role === "admin" ? "accent" : "neutral"}>
          {u.role === "admin" ? "Admin" : "User"}
        </Chip>
      ),
    },
    {
      key: "state",
      header: "State",
      width: "104px",
      render: (u) =>
        u.active_session_count > 0 ? (
          <Chip variant="success" dot>Streaming</Chip>
        ) : u.disabled ? (
          <Chip variant="neutral">Disabled</Chip>
        ) : (
          <Chip variant="success" dot>Active</Chip>
        ),
    },
    {
      key: "sessions",
      header: "Sessions",
      width: "90px",
      align: "right",
      render: (u) => <span className="num">{u.active_session_count}</span>,
    },
    {
      key: "home",
      header: "Home size",
      width: "110px",
      align: "right",
      render: (u) => (
        <span className="num">
          {storageByUser.has(u.id) ? bytes(storageByUser.get(u.id)!) : "—"}
        </span>
      ),
    },
    {
      key: "lastSeen",
      header: "Last seen",
      width: "110px",
      align: "right",
      render: (u) => <span className="sub">{u.last_seen_at ? relativeTime(u.last_seen_at) : "Never"}</span>,
    },
    {
      key: "menu",
      header: "",
      mobileLabel: "",
      width: "44px",
      render: (u) => (
        <div onClick={(e) => e.stopPropagation()}>
          <ActionsMenu label={`Actions for ${u.username}`} items={menuItemsFor(u)} />
        </div>
      ),
    },
  ];

  // ── Render ──────────────────────────────────────────────────────────────────

  const selectedCount = selectedInFiltered.length;
  const activeCount = users.filter((u) => !u.disabled).length;
  const adminCount = users.filter((u) => u.role === "admin").length;

  function handleExport() {
    // Export the segment/search-filtered view, not the full loaded set — an
    // admin who filtered to "Disabled" to review it is exporting that review.
    const rows: UserCsvRow[] = filtered.map((u) => ({
      username: u.username,
      email: u.email,
      role: u.role === "admin" ? "Admin" : "User",
      state: u.disabled ? "Disabled" : "Active",
      activeSessionCount: u.active_session_count,
      homeBytes: storageByUser.get(u.id) ?? null,
      lastSeenAt: u.last_seen_at,
    }));
    downloadBlob("users.csv", new Blob([usersToCsv(rows)], { type: "text/csv;charset=utf-8" }));
  }

  // The head is the People section's (../People.tsx); this tab fills it in.
  useSectionHead({
    sub: (
      <>
        {activeCount} active · {adminCount} admins
      </>
    ),
    actions: (
      <>
        {/* Neither tab polls, so a manual escape hatch is the only way back
            to a fresh read after a write elsewhere changes the picture. */}
        <Button variant="ghost" onClick={load}>
          Refresh
        </Button>
        <Button variant="ghost" onClick={handleExport}>
          <IconDownload />
          Export
        </Button>
        <Button variant="primary" onClick={() => navigate("/admin/people/invites")}>
          <IconPlus />
          Invite user
        </Button>
      </>
    ),
    // Invites' pending count is that tab's own resource — publishing it here
    // too would mean fetching invites on every Users-tab visit just for a
    // badge. Same limitation as Library's per-tab counts.
    counts: { users: users.length },
  });

  return (
    <section className="page admin-users-page">
      <ResourceStates loading={loading} error={errorMessage} />

      {!loading && (
        <>
          <div className="toolbar">
            <SegmentedControl<UserSegment>
              options={[
                { value: "all", label: "All" },
                { value: "admins", label: "Admins" },
                {
                  value: "disabled",
                  label: (
                    <>
                      Disabled <span className="num" style={{ opacity: 0.7 }}>{disabledCount}</span>
                    </>
                  ),
                },
              ]}
              value={segment}
              onChange={setSegment}
              aria-label="Filter users"
            />
            <div className="search">
              <IconSearch />
              <input
                placeholder="Filter by name or email"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                aria-label="Filter users by name or email"
              />
            </div>
          </div>

          <Table
            columns={columns}
            rows={filtered}
            rowKey={(u) => u.id}
            empty={query ? `No users matching "${query}"` : "No users found."}
            onRowClick={(u) => setDrawerUser(u)}
            rowStyle={(u) => (u.disabled ? { opacity: 0.6 } : undefined)}
          />
        </>
      )}

      {/* Bulk bar — counts selected rows within the filtered set */}
      <BulkBar
        selectedCount={selectedCount}
        noun="user"
        onClear={clearSelected}
        actions={[
          { label: "Change role", onClick: () => setBulkRoleOpen(true) },
          { label: "Disable", onClick: () => void handleBulkDisable() },
          { label: "Delete", onClick: () => setBulkDeleteConfirm(true), variant: "danger" },
        ]}
      />
      {bulkBusy && <p className="muted" style={{ marginTop: 8 }}>Processing…</p>}

      {/* Bulk role-change modal */}
      {bulkRoleOpen && (
        <Modal
          open
          onClose={() => setBulkRoleOpen(false)}
          title="Change role"
          footer={
            <>
              <Button variant="ghost" onClick={() => setBulkRoleOpen(false)}>Cancel</Button>
              <Button
                variant="secondary"
                disabled={bulkBusy}
                onClick={() => void handleBulkRole("user").then(() => setBulkRoleOpen(false))}
              >
                Set to user
              </Button>
              <Button
                variant="primary"
                disabled={bulkBusy}
                onClick={() => void handleBulkRole("admin").then(() => setBulkRoleOpen(false))}
              >
                Set to admin
              </Button>
            </>
          }
        >
          <p className="sec">
            Change <strong>{selectedCount}</strong> user{selectedCount === 1 ? "" : "s"} to:
          </p>
        </Modal>
      )}

      {/* Bulk-delete confirmation modal */}
      {bulkDeleteConfirm && (
        <Modal
          open
          onClose={() => setBulkDeleteConfirm(false)}
          title="Delete users"
          footer={
            <>
              <Button variant="ghost" onClick={() => setBulkDeleteConfirm(false)}>Cancel</Button>
              <Button
                variant="danger"
                disabled={bulkBusy}
                onClick={() =>
                  void handleBulkDelete().then(() => setBulkDeleteConfirm(false))
                }
              >
                {bulkBusy ? "Deleting…" : `Delete ${selectedCount}`}
              </Button>
            </>
          }
        >
          <p className="sec">
            This permanently removes <strong>{selectedCount}</strong> user
            {selectedCount === 1 ? "" : "s"}, ends any active sessions, and frees their GPU
            slots. This cannot be undone.
          </p>
        </Modal>
      )}

      {/* Role change confirmation modal */}
      {roleTarget && (
        <Modal
          open
          onClose={() => setRoleTarget(null)}
          title={roleTarget.role === "admin" ? "Demote to user" : "Promote to admin"}
          footer={
            <>
              <Button variant="ghost" onClick={() => setRoleTarget(null)}>Cancel</Button>
              <Button
                variant="primary"
                disabled={actingOn === roleTarget.id}
                onClick={() => void handleToggleRole(roleTarget).then(() => setRoleTarget(null))}
              >
                {actingOn === roleTarget.id ? "Saving…" : "Confirm"}
              </Button>
            </>
          }
        >
          <p className="sec">
            {roleTarget.role === "admin" ? (
              <>Demote <strong>{roleTarget.username}</strong> from admin to regular user?</>
            ) : (
              <>Promote <strong>{roleTarget.username}</strong> to admin? They will gain full operator access.</>
            )}
          </p>
        </Modal>
      )}

      {/* Disable / Enable confirmation modal */}
      {disableTarget && (
        <Modal
          open
          onClose={() => setDisableTarget(null)}
          title={disableTarget.disabled ? "Enable account" : "Disable account"}
          footer={
            <>
              <Button variant="ghost" onClick={() => setDisableTarget(null)}>Cancel</Button>
              <Button
                variant={disableTarget.disabled ? "primary" : "danger"}
                disabled={actingOn === disableTarget.id}
                onClick={() =>
                  void handleToggleDisable(disableTarget).then(() => setDisableTarget(null))
                }
              >
                {actingOn === disableTarget.id
                  ? "Saving…"
                  : disableTarget.disabled
                    ? "Enable"
                    : "Disable"}
              </Button>
            </>
          }
        >
          <p className="sec">
            {disableTarget.disabled ? (
              <>
                Re-enable <strong>{disableTarget.username}</strong>? They will be able to log in
                and launch sessions.
              </>
            ) : (
              <>
                Disable <strong>{disableTarget.username}</strong>? They will be locked out
                immediately.
              </>
            )}
          </p>
        </Modal>
      )}

      {/* Delete confirmation modal */}
      {deleteTarget && (
        <Modal
          open
          onClose={() => {
            setDeleteTarget(null);
            setDeleteConfirmText("");
          }}
          title="Delete user"
          footer={
            <>
              <Button
                variant="ghost"
                onClick={() => {
                  setDeleteTarget(null);
                  setDeleteConfirmText("");
                }}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={
                  deleteConfirmText !== deleteTarget.username || actingOn === deleteTarget.id
                }
                onClick={() =>
                  void handleDelete(deleteTarget).then(() => {
                    setDeleteTarget(null);
                    setDeleteConfirmText("");
                  })
                }
              >
                {actingOn === deleteTarget.id ? "Deleting…" : "Delete user"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This permanently removes <strong>{deleteTarget.username}</strong>, ends any active
            sessions, and frees their GPU slots. This cannot be undone.
          </p>
          <div style={{ marginTop: "var(--s5)" }}>
            <TextField
              label={`Type "${deleteTarget.username}" to confirm`}
              value={deleteConfirmText}
              onChange={(e) => setDeleteConfirmText(e.target.value)}
              placeholder={deleteTarget.username}
            />
          </div>
        </Modal>
      )}

      {/* Quota modal */}
      {quotaTarget && (
        <QuotaModal
          user={quotaTarget}
          token={token!}
          onSave={(updated) => {
            replaceUser(updated);
            setQuotaTarget(null);
            addToast({ variant: "success", title: `Quota updated for ${updated.username}` });
          }}
          onClose={() => setQuotaTarget(null)}
        />
      )}

      {/* Session history drawer */}
      {drawerUser && (
        <UserDrawer
          user={drawerUser}
          token={token!}
          storageBytes={storageByUser.get(drawerUser.id)}
          onClose={() => setDrawerUser(null)}
          onRoleClick={(u) => {
            setDrawerUser(null);
            setRoleTarget(u);
          }}
          onDisableClick={(u) => {
            setDrawerUser(null);
            setDisableTarget(u);
          }}
        />
      )}
    </section>
  );
}
