// Access tab (handoff §A.10, spec §6/§6.6). Who may see and launch this app.
// Authorization is server-enforced (CLAUDE.md invariant #6); this is the
// convenience control over the same rows.
//
// Model: one subject_type='all' row (Everyone) or per-user grant rows. Both can
// coexist (a stray grant survives switching back to Everyone), so the per-user
// list is always shown: what is stored, not just the toggle's intent.
//
// The grants themselves are loaded by the page, because the tab bar shows their
// count whether or not this tab was ever opened.

import { useState } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AdminUser, Entitlement } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { Modal } from "../../../../components/Modal";
import { ResourceStates } from "../../../../components/ResourceStates";
import { SegmentedControl } from "../../../../components/SegmentedControl";
import { SelectField } from "../../../../components/TextField";
import { useAdminAction } from "../../../../lib/resource/action";
import { useResource } from "../../../../lib/resource/react";
import { Section } from "./primitives";

const GRANTED_BY_LABEL: Record<Entitlement["granted_by"], string> = {
  admin: "Granted by an admin",
  provider: "Granted by a library sync",
  migration: "Carried over automatically",
};

interface AccessTabProps {
  appId: string;
  token: string;
  items: Entitlement[];
  loading: boolean;
  error: string | null;
  /** Re-read the page's entitlements after a write, so the tab count moves too. */
  reload: () => void;
  /** The provider app's Library tab, where a sync-written grant is actually
   *  stopped. Null when this app hangs off no provider. */
  libraryLink: string | null;
}

export function AccessTab({
  appId,
  token,
  items,
  loading,
  error,
  reload,
  libraryLink,
}: AccessTabProps) {
  const [pickedUserId, setPickedUserId] = useState("");
  const [revokeTarget, setRevokeTarget] = useState<Entitlement | null>(null);

  const users = useResource<AdminUser[]>(
    {
      label: "users",
      initialData: [],
      fetch: async (ctx) => (await adminApi.listUsers(ctx.token)).items,
    },
    [],
  );

  const everyone = items.find((e) => e.subject_type === "all") ?? null;
  const userGrants = items.filter((e) => e.subject_type === "user");
  const grantedUserIds = new Set(userGrants.map((e) => e.subject_id));
  const pickableUsers = (users.data ?? []).filter((u) => !grantedUserIds.has(u.id));

  const setEveryone = useAdminAction(
    async (on: boolean) => {
      if (on) await adminApi.grantEntitlement(token, appId, { subject_type: "all" });
      else if (everyone) await adminApi.revokeEntitlement(token, appId, everyone.id);
      reload();
    },
    {
      success: (_r, on) => (on ? "Now visible to everyone." : "Restricted to specific users."),
      failure: "that did not work",
    },
  );

  const grantUser = useAdminAction(
    async (userId: string) => {
      await adminApi.grantEntitlement(token, appId, { subject_type: "user", subject_id: userId });
      setPickedUserId("");
      reload();
    },
    {
      success: "Access granted.",
      // 409 = the store's ErrEntitlementExists: the subject already holds a
      // grant. Distinguished so an admin does not read it as a generic failure.
      failure: (e) =>
        e instanceof ApiError && e.status === 409
          ? "That user already has access to this app."
          : e instanceof ApiError
            ? e.message
            : "could not grant access",
    },
  );

  const revoke = useAdminAction(
    async (ent: Entitlement) => {
      await adminApi.revokeEntitlement(token, appId, ent.id);
      setRevokeTarget(null);
      reload();
    },
    {
      success: (_r, ent) => `Access revoked for ${ent.subject_username ?? "user"}.`,
      failure: "could not revoke access",
    },
  );

  const busy =
    setEveryone.pending != null || grantUser.pending != null || revoke.pending != null;

  return (
    <Section
      title="Access"
      desc="Who may see and launch this app. Enforced by the server on every request, so this control is convenience, not the boundary."
    >
      <ResourceStates loading={loading} error={error} />
      {!loading && !error && (
        <>
          <SegmentedControl
            aria-label="Visible to"
            activation="manual"
            value={everyone ? "everyone" : "specific"}
            onChange={(v) => void setEveryone.run(v === "everyone")}
            disabled={busy}
            options={[
              { value: "everyone", label: "Everyone" },
              { value: "specific", label: "Specific users" },
            ]}
          />

          {/* Always shown, even under Everyone: a stray per-user row must be
              visible and manageable rather than silently hidden. */}
          <div className="field" style={{ maxWidth: 560 }}>
            <span className="label">Specific grants</span>
            {everyone && (
              <span className="hint">
                Everyone can already see this app. These grants have no additional effect unless
                Everyone access is later removed.
              </span>
            )}
            <div className="ae-list">
              {userGrants.length === 0 ? (
                <span className="hint">No individual grants.</span>
              ) : (
                userGrants.map((g) => (
                  <div key={g.id} className="ae-item">
                    <div>
                      <div className="ae-item-t">{g.subject_username ?? "(deleted user)"}</div>
                      <div className="ae-item-m">{GRANTED_BY_LABEL[g.granted_by]}</div>
                    </div>
                    <Button
                      variant="ghost"
                      size="sm"
                      disabled={busy}
                      onClick={() => setRevokeTarget(g)}
                    >
                      Revoke
                    </Button>
                  </div>
                ))
              )}
            </div>
          </div>

          <div className="rowflex" style={{ alignItems: "flex-end", maxWidth: 560 }}>
            <div style={{ flex: 1 }}>
              <SelectField
                label="Add a user"
                aria-label="Add a user"
                value={pickedUserId}
                onChange={(e) => setPickedUserId(e.target.value)}
                disabled={pickableUsers.length === 0}
              >
                <option value="">
                  {pickableUsers.length === 0 ? "Every user already has a grant" : "Choose a user"}
                </option>
                {pickableUsers.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.username}
                  </option>
                ))}
              </SelectField>
            </div>
            <Button
              disabled={busy || !pickedUserId}
              onClick={() => void grantUser.run(pickedUserId)}
            >
              Grant access
            </Button>
          </div>

          {userGrants.some((g) => g.granted_by === "provider") && (
            <div className="note warn">
              <div>
                One grant here came from a library sync. Revoking it holds only until the next sync.{" "}
                {libraryLink ? (
                  <>
                    Ignore the appid under <Link to={libraryLink}>Steam › Library</Link> to stop it
                    coming back.
                  </>
                ) : (
                  <>Ignore the appid on the provider app&rsquo;s Library tab to stop it coming back.</>
                )}
              </div>
            </div>
          )}
        </>
      )}

      {revokeTarget && (
        <Modal
          open
          onClose={() => setRevokeTarget(null)}
          title="Revoke access"
          footer={
            <>
              <Button variant="ghost" onClick={() => setRevokeTarget(null)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={revoke.pending != null}
                onClick={() => void revoke.run(revokeTarget)}
              >
                {revoke.pending ? "Revoking…" : "Revoke access"}
              </Button>
            </>
          }
        >
          <p className="sec">
            Revoke <strong>{revokeTarget.subject_username ?? "this user"}</strong>&rsquo;s access to
            this app?
          </p>
          {revokeTarget.granted_by === "provider" && (
            <div className="note warn mt4">
              <div>
                <b>This grant came from a library sync.</b> The next sync may re-grant it
                automatically if the game is still installed. Revoking one user&rsquo;s entitlement
                is not how you get rid of a junk tile. For a permanent, fleet-wide removal, use
                Ignore on the discovered tile instead.
              </div>
            </div>
          )}
        </Modal>
      )}
    </Section>
  );
}
