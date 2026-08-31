// The restart-class `.note.warn` above the panels (handoff-v3-spec §A.7).
// Exactly one of four mutually-exclusive states renders: an in-flight confirm
// (Save-triggered or the standalone Restart-agent one) always wins over the
// passive notices, and the proactive dirty-but-unconfirmed note is the fallback.
import { Button } from "../../../../components/Button";

function liveSessionsWording(n: number | undefined): string {
  if (n === undefined) return "any live sessions";
  return `${n} live session${n === 1 ? "" : "s"}`;
}

function pluralChanges(n: number): string {
  return `${n} restart-class change${n === 1 ? " is" : "s are"}`;
}

function itOrThem(n: number): string {
  return n === 1 ? "it" : "them";
}

export function RestartNote({
  hasDirtyRestart,
  dirtyRestartCount,
  confirmRestart,
  restartConfirmPending,
  showRestartButton,
  pendingRestart,
  liveSessionsCount,
  saving,
  restarting,
  onCancelConfirm,
  onSaveConfirm,
  onCancelRestartPending,
  onConfirmRestartPending,
  onRestartNow,
}: {
  hasDirtyRestart: boolean;
  dirtyRestartCount: number;
  confirmRestart: boolean;
  restartConfirmPending: boolean;
  showRestartButton: boolean;
  pendingRestart: boolean;
  liveSessionsCount: number | undefined;
  saving: boolean;
  restarting: boolean;
  onCancelConfirm: () => void;
  onSaveConfirm: () => void;
  onCancelRestartPending: () => void;
  onConfirmRestartPending: () => void;
  onRestartNow: () => void;
}) {
  if (confirmRestart) {
    return (
      <div className="note warn row gap3 center" style={{ marginBottom: "var(--s4)" }}>
        <span style={{ flex: 1 }}>
          This will restart the agent and drop {liveSessionsWording(liveSessionsCount)} on this host.
        </span>
        <Button variant="ghost" onClick={onCancelConfirm}>Cancel</Button>
        <Button variant="primary" disabled={saving} onClick={onSaveConfirm}>
          {saving ? "Restarting…" : "Save & restart"}
        </Button>
      </div>
    );
  }

  if (restartConfirmPending) {
    return (
      <div className="note warn row gap3 center" style={{ marginBottom: "var(--s4)" }}>
        <span style={{ flex: 1 }}>
          Restarting will drop {liveSessionsWording(liveSessionsCount)} on this host.
        </span>
        <Button variant="ghost" onClick={onCancelRestartPending}>Cancel</Button>
        <Button variant="primary" disabled={restarting} onClick={onConfirmRestartPending}>
          {restarting ? "Restarting…" : "Restart agent"}
        </Button>
      </div>
    );
  }

  if (showRestartButton) {
    return (
      <div className="note warn row gap3 center" style={{ marginBottom: "var(--s4)" }}>
        <span style={{ flex: 1 }}>
          {pendingRestart
            ? "Encoder or GPU changes are pending an agent restart."
            : "A saved change hasn't reached the agent yet."}
        </span>
        <Button variant="ghost" disabled={restarting} onClick={onRestartNow}>
          {restarting ? "Restarting…" : "Restart agent"}
        </Button>
      </div>
    );
  }

  if (hasDirtyRestart) {
    return (
      <div className="note warn" style={{ marginBottom: "var(--s4)" }}>
        {pluralChanges(dirtyRestartCount)} pending. Saving {itOrThem(dirtyRestartCount)} restarts the node
        agent and ends {liveSessionsWording(liveSessionsCount)} on this host.
      </div>
    );
  }

  return null;
}
