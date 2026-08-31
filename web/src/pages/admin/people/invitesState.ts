// Pure invite lifecycle derivation (handoff §A.19). No React/DOM — the
// InvitesTab table, its state chip and its "Copy link"/"Revoke" action
// gating all key off this one function so they can never disagree.

export type InviteLifecycleState = "pending" | "expired" | "redeemed" | "revoked";

export interface InviteStateFields {
  revoked_at: string | null;
  used_count: number;
  max_uses: number;
  expires_at: string | null;
}

/** Precedence is fixed: a revoked invite reads revoked even if it was also
 *  used up or past its expiry; a used-up invite reads redeemed even past its
 *  expiry (it did its job before it lapsed). `now` is a timestamp (ms) so
 *  callers — and tests — control the clock instead of racing Date.now(). */
export function inviteState(invite: InviteStateFields, now: number): InviteLifecycleState {
  if (invite.revoked_at) return "revoked";
  if (invite.used_count >= invite.max_uses) return "redeemed";
  if (invite.expires_at && new Date(invite.expires_at).getTime() < now) return "expired";
  return "pending";
}

const LABEL: Record<InviteLifecycleState, string> = {
  pending: "Pending",
  expired: "Expired",
  redeemed: "Redeemed",
  revoked: "Revoked",
};

export function inviteStateLabel(state: InviteLifecycleState): string {
  return LABEL[state];
}

/** Chip colour per state (mock `STATE_CHIP`: pending→warning, redeemed→info,
 *  expired/revoked→neutral). Only "success" states get the chip's dot, and
 *  no invite state is a success state. */
export function inviteStateChipVariant(
  state: InviteLifecycleState,
): "warning" | "info" | "neutral" {
  if (state === "pending") return "warning";
  if (state === "redeemed") return "info";
  return "neutral";
}
