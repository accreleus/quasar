// Admin invites (handoff §A.19): the instance-wide registration_mode switch,
// and minting/listing/revoking invites — the plaintext code + magic link are
// shown exactly once, right after minting, never retrievable again.
// Authorization is server-enforced (control-api.md §Authorization); this page is UX only.

import { useEffect, useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { Invite, MintInviteResponse, RegistrationMode } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconMore, IconPlus } from "../../../components/icons";
import { LoadingState } from "../../../components/LoadingState";
import { Modal } from "../../../components/Modal";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { ResourceStates } from "../../../components/ResourceStates";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { SelectField, TextField, TextareaField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { fmtDate } from "../../../lib/formatLegacy";
import { useResource } from "../../../lib/resource/react";
import { inviteState, inviteStateChipVariant, inviteStateLabel } from "./invitesState";

const MODE_COPY: Record<RegistrationMode, { label: string }> = {
  closed: { label: "Closed" },
  invite_only: { label: "Invite only" },
  open: { label: "Open" },
};

function inviteUrlFor(code: string): string {
  return `${window.location.origin}/register?invite=${code}`;
}

/** Code cell sub-line: "Admin", a note, or "Admin · {note}" — role is folded
 *  in here rather than its own column (dropped per the mock's column list)
 *  so a pending admin-granting invite is still visible after minting. User
 *  role is the common case and adds nothing, so it is omitted. */
function inviteSubLabel(i: Pick<Invite, "role" | "note">): string | null {
  const parts: string[] = [];
  if (i.role === "admin") parts.push("Admin");
  if (i.note) parts.push(i.note);
  return parts.length ? parts.join(" · ") : null;
}

// navigator.clipboard is secure-context-only (quasar#376) — absent entirely on
// a plain-HTTP deployment, not just permission-denied. Feature-detect rather
// than relying on the catch below to interpret a TypeError.
async function copyToClipboard(
  text: string,
  label: string,
  addToast: ReturnType<typeof useToast>["addToast"],
) {
  if (!navigator.clipboard?.writeText) {
    addToast({
      variant: "danger",
      title: `Could not copy ${label.toLowerCase()}`,
      body: "Clipboard access needs a secure context. Use the HTTPS address.",
    });
    return;
  }
  try {
    await navigator.clipboard.writeText(text);
    addToast({ variant: "success", title: `${label} copied` });
  } catch {
    addToast({ variant: "danger", title: `Could not copy ${label.toLowerCase()}` });
  }
}

// ── Registration mode card (handoff §A.19: eyebrow + segmented + note.warn) ──

function RegistrationModeCard({ token }: { token: string }) {
  const { addToast } = useToast();
  const [mode, setMode] = useState<RegistrationMode | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    void adminApi.getSettings(token).then(({ settings }) => {
      if (!cancelled) setMode(settings.registration_mode);
    });
    return () => { cancelled = true; };
  }, [token]);

  async function handleChange(next: RegistrationMode) {
    if (mode === next) return;
    setSaving(true);
    try {
      const { settings } = await adminApi.updateSettings(token, { registration_mode: next });
      setMode(settings.registration_mode);
      addToast({ variant: "success", title: `Registration set to "${MODE_COPY[next].label}"` });
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not update registration mode",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <div
      className="card card-pad"
      style={{ display: "flex", gap: "var(--s7)", alignItems: "center", flexWrap: "wrap" }}
    >
      <div>
        <div className="eyebrow">Registration mode</div>
        {mode === null ? (
          <LoadingState />
        ) : (
          <div style={{ marginTop: 8 }}>
            <SegmentedControl
              options={(Object.keys(MODE_COPY) as RegistrationMode[]).map((m) => ({
                value: m,
                label: MODE_COPY[m].label,
              }))}
              value={mode}
              onChange={(next) => void handleChange(next)}
              disabled={saving}
              activation="manual"
              aria-label="Registration mode"
            />
          </div>
        )}
      </div>
      <div className="note warn" style={{ maxWidth: 460, marginLeft: "auto" }}>
        An invite code is shown <strong>once</strong>, when it is minted. It cannot be retrieved
        afterwards. Revoke and mint a new one instead.
      </div>
    </div>
  );
}

// ── Mint modal (handoff §A.19 action "Mint invite"; same fields as before,
//    now behind the head action instead of an always-visible card) ──────────

interface MintModalProps {
  token: string;
  onClose: () => void;
  onMinted: (invite: MintInviteResponse["invite"]) => void;
}

function MintModal({ token, onClose, onMinted }: MintModalProps) {
  const { addToast } = useToast();
  const [role, setRole] = useState<"user" | "admin">("user");
  const [maxUses, setMaxUses] = useState(1);
  const [expiresAt, setExpiresAt] = useState("");
  const [note, setNote] = useState("");
  const [minting, setMinting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleMint() {
    setMinting(true);
    setError(null);
    try {
      const { invite } = await adminApi.mintInvite(token, {
        role,
        max_uses: maxUses,
        expires_at: expiresAt ? new Date(expiresAt).toISOString() : null,
        note: note.trim() || undefined,
      });
      onMinted(invite);
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : "Could not mint invite.";
      setError(msg);
      addToast({ variant: "danger", title: "Could not mint invite", body: msg });
    } finally {
      setMinting(false);
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      title="Mint an invite"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={minting} onClick={() => void handleMint()}>
            {minting ? "Minting…" : "Mint invite"}
          </Button>
        </>
      }
    >
      <div className="pw-grid">
        <SelectField
          label="Role"
          value={role}
          onChange={(e) => setRole(e.target.value as "user" | "admin")}
        >
          <option value="user">User</option>
          <option value="admin">Admin</option>
        </SelectField>
        <TextField
          label="Max uses"
          type="number"
          min={1}
          value={maxUses}
          onChange={(e) => setMaxUses(Math.max(1, Number(e.target.value) || 1))}
        />
        <TextField
          label="Expires (optional)"
          type="datetime-local"
          value={expiresAt}
          onChange={(e) => setExpiresAt(e.target.value)}
        />
        <div style={{ gridColumn: "1 / -1" }}>
          <TextareaField
            label="Note (optional)"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="e.g. for the family Discord"
            rows={2}
          />
        </div>
      </div>

      {error && <p className="form-error mt4">{error}</p>}
    </Modal>
  );
}

// ── One-time reveal modal (unchanged behaviour) ─────────────────────────────

function InviteRevealModal({
  invite,
  onClose,
}: {
  invite: MintInviteResponse["invite"];
  onClose: () => void;
}) {
  const { addToast } = useToast();
  const url = invite.invite_url ?? inviteUrlFor(invite.code);

  return (
    <Modal open onClose={onClose} title="Invite created" maxWidth={520}>
      <p className="sec">
        This code and link are shown <strong>only once</strong>. Copy them now. They cannot be
        retrieved again after you close this dialog.
      </p>

      <div className="mt5" style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
        <div>
          <span className="label">Invite code</span>
          <div className="row gap2 mt2">
            <code
              className="mono"
              style={{
                flex: 1,
                background: "var(--ink-2)",
                border: "1px solid var(--line-2)",
                borderRadius: "var(--r-sm)",
                padding: "8px 12px",
                fontSize: "var(--t-sm)",
                wordBreak: "break-all",
              }}
            >
              {invite.code}
            </code>
            <Button variant="ghost" size="sm" onClick={() => void copyToClipboard(invite.code, "Code", addToast)}>
              Copy
            </Button>
          </div>
        </div>

        <div>
          <span className="label">Magic link</span>
          <div className="row gap2 mt2">
            <code
              className="mono"
              style={{
                flex: 1,
                background: "var(--ink-2)",
                border: "1px solid var(--line-2)",
                borderRadius: "var(--r-sm)",
                padding: "8px 12px",
                fontSize: "var(--t-xs)",
                wordBreak: "break-all",
              }}
            >
              {url}
            </code>
            <Button variant="ghost" size="sm" onClick={() => void copyToClipboard(url, "Link", addToast)}>
              Copy
            </Button>
          </div>
        </div>
      </div>

      <p className="muted mt4" style={{ fontSize: "var(--t-xs)" }}>
        Anyone with this link can register {invite.role === "admin" ? "an admin " : ""}account,
        up to {invite.max_uses} time{invite.max_uses === 1 ? "" : "s"}
        {invite.expires_at ? ` · expires ${fmtDate(invite.expires_at)}` : ""}.
      </p>
    </Modal>
  );
}

// ── Page ───────────────────────────────────────────────────────────────────

export function InvitesTab() {
  const { token } = useAuth();
  const { addToast } = useToast();
  const [minting, setMinting] = useState(false);
  const [reveal, setReveal] = useState<MintInviteResponse["invite"] | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<Invite | null>(null);
  const [revoking, setRevoking] = useState<string | null>(null);
  // A magic link the page has seen this session (freshly minted, or freshly
  // revealed) — the Invite list row itself never carries the plaintext code
  // or link (only code_prefix), so "Copy link" is only offered for these.
  const [knownLinks, setKnownLinks] = useState<Record<string, string>>({});

  const invitesRes = useResource<Invite[]>({
    label: "invites",
    initialData: [],
    fetch: async (ctx) => (await adminApi.listInvites(ctx.token)).invites,
  });
  const invites = invitesRes.data ?? [];
  const now = Date.now();

  const pendingCount = invites.filter((i) => inviteState(i, now) === "pending").length;

  // The head is the People section's (../People.tsx); this tab fills it in.
  useSectionHead({
    sub: "Invite-gated registration",
    actions: (
      <>
        {/* This tab doesn't poll, so a manual escape hatch is the only way
            back to a fresh read after a write elsewhere changes the picture. */}
        <Button variant="ghost" onClick={() => void invitesRes.refresh()}>
          Refresh
        </Button>
        <Button variant="primary" onClick={() => setMinting(true)}>
          <IconPlus />
          Mint invite
        </Button>
      </>
    ),
    counts: { invites: pendingCount },
  });

  async function handleRevoke(inv: Invite) {
    setRevoking(inv.id);
    try {
      // revokeInvite returns void — no updated row to write back locally, so
      // re-fetch the canonical list once the write lands (the documented
      // "mutate() never auto-revalidates" escape hatch in lib/resource/core.ts).
      await invitesRes.mutate((ctx) => adminApi.revokeInvite(ctx.token, inv.id));
      await invitesRes.refresh({ silent: true });
      addToast({ variant: "success", title: "Invite revoked" });
      setRevokeTarget(null);
    } catch (e) {
      addToast({
        variant: "danger",
        title: "Could not revoke invite",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setRevoking(null);
    }
  }

  const columns: TableColumn<Invite>[] = [
    {
      key: "code",
      header: "Code",
      width: "170px",
      render: (i) => {
        const sub = inviteSubLabel(i);
        return (
          <div className="stack">
            <span className="cell-id" style={{ fontSize: "var(--t-sm)" }}>{i.code_prefix}</span>
            {sub && (
              <span
                className="sub"
                title={sub}
                style={{
                  display: "block",
                  maxWidth: 150,
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                  whiteSpace: "nowrap",
                }}
              >
                {sub}
              </span>
            )}
          </div>
        );
      },
    },
    {
      key: "state",
      header: "State",
      width: "100px",
      render: (i) => {
        const s = inviteState(i, now);
        return <Chip variant={inviteStateChipVariant(s)}>{inviteStateLabel(s)}</Chip>;
      },
    },
    {
      key: "createdBy",
      header: "Created by",
      render: (i) => <span className="sub">{i.created_by_username ?? "—"}</span>,
    },
    {
      key: "created",
      header: "Created",
      width: "110px",
      align: "right",
      render: (i) => <span className="sub mono">{fmtDate(i.created_at)}</span>,
    },
    {
      key: "expires",
      header: "Expires",
      width: "110px",
      align: "right",
      render: (i) => (
        <span
          className="mono"
          style={{ color: inviteState(i, now) === "expired" ? "var(--danger-text)" : "var(--text-3)" }}
        >
          {i.expires_at ? fmtDate(i.expires_at) : "Never"}
        </span>
      ),
    },
    {
      key: "uses",
      header: "Uses",
      width: "80px",
      align: "right",
      render: (i) => <span className="num">{i.used_count} / {i.max_uses}</span>,
    },
    {
      key: "actions",
      header: "",
      mobileLabel: "",
      width: "170px",
      render: (i) => {
        const s = inviteState(i, now);
        const link = knownLinks[i.id];
        if (s !== "pending") {
          return (
            <div className="cell-actions">
              <button className="icon-btn" disabled aria-label="No actions available for this invite">
                <IconMore className="" />
              </button>
            </div>
          );
        }
        return (
          <div className="cell-actions">
            {link && (
              <Button variant="ghost" size="sm" onClick={() => void copyToClipboard(link, "Link", addToast)}>
                Copy link
              </Button>
            )}
            <Button
              variant="danger"
              size="sm"
              disabled={revoking === i.id}
              onClick={() => setRevokeTarget(i)}
            >
              Revoke
            </Button>
          </div>
        );
      },
    },
  ];

  return (
    <section className="page">
      {token && <RegistrationModeCard token={token} />}

      <ResourceStates loading={invitesRes.loading} error={invitesRes.errorMessage} />
      {!invitesRes.loading && (
        <Table
          columns={columns}
          rows={invites}
          rowKey={(i) => i.id}
          empty="No invites minted yet."
          rowStyle={(i) => (inviteState(i, now) !== "pending" ? { opacity: 0.65 } : undefined)}
        />
      )}

      {minting && token && (
        <MintModal
          token={token}
          onClose={() => setMinting(false)}
          onMinted={(invite) => {
            setMinting(false);
            setKnownLinks((prev) => ({ ...prev, [invite.id]: invite.invite_url ?? inviteUrlFor(invite.code) }));
            setReveal(invite);
            void invitesRes.refresh({ silent: true });
          }}
        />
      )}

      {reveal && (
        <InviteRevealModal
          invite={reveal}
          onClose={() => setReveal(null)}
        />
      )}

      {revokeTarget && (
        <Modal
          open
          onClose={() => setRevokeTarget(null)}
          title="Revoke invite"
          footer={
            <>
              <Button variant="ghost" onClick={() => setRevokeTarget(null)}>Cancel</Button>
              <Button
                variant="danger"
                disabled={revoking === revokeTarget.id}
                onClick={() => void handleRevoke(revokeTarget)}
              >
                {revoking === revokeTarget.id ? "Revoking…" : "Revoke invite"}
              </Button>
            </>
          }
        >
          <p className="sec">
            Revoke invite <strong>{revokeTarget.code_prefix}</strong>? Nobody will be able to use
            it to register after this.
          </p>
        </Modal>
      )}
    </section>
  );
}
