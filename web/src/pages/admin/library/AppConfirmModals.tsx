// Delete and Ignore confirmation modals for the Apps tab (spec §8.2/§17):
// Ignore is the fix for a junk discovered tile, Delete is not — a discovered
// tile's Delete confirm steers toward Ignore instead of just executing.

import type { AdminApp } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";

interface DeleteAppModalProps {
  target: AdminApp;
  pending: boolean;
  onConfirm: () => void;
  onClose: () => void;
}

export function DeleteAppModal({ target, pending, onConfirm, onClose }: DeleteAppModalProps) {
  return (
    <Modal
      open
      onClose={onClose}
      title="Delete app"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="danger" disabled={pending} onClick={onConfirm}>
            {pending ? "Deleting…" : "Delete app"}
          </Button>
        </>
      }
    >
      <p className="sec">
        This permanently removes <strong>{target.name}</strong> from the catalog. Its session
        history is purged. Users with an active session on this app must stop it first. This
        cannot be undone.
      </p>
      {target.parent_app_id && (
        <div className="note warn mt4">
          <div>
            <b>This is a discovered tile. Delete is probably not what you want.</b> Deleting it
            destroys every user&rsquo;s favourite of it and its artwork, and cannot be undone. The
            game is still installed, so the next scan recreates a bare, un-favourited, art-less
            tile in its place. For a junk tile you never want back, use <strong>Ignore</strong>{" "}
            instead. It stays gone across every future scan.
          </div>
        </div>
      )}
    </Modal>
  );
}

interface IgnoreAppModalProps {
  target: AdminApp;
  pending: boolean;
  onConfirm: () => void;
  onClose: () => void;
}

// rule='ignore' (spec §8.2): the server disables the tile + revokes provider
// entitlements in one transaction. Never a delete — app row, artwork and
// favourites survive.
export function IgnoreAppModal({ target, pending, onConfirm, onClose }: IgnoreAppModalProps) {
  return (
    <Modal
      open
      onClose={onClose}
      title="Ignore this tile"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={pending} onClick={onConfirm}>
            {pending ? "Ignoring…" : "Ignore tile"}
          </Button>
        </>
      }
    >
      <p className="sec">
        Disable <strong>{target.name}</strong> for every user and revoke the library sync&rsquo;s
        access grants for it. This is fleet-wide and permanent across future scans — the game
        being reinstalled or re-observed will not bring it back.
      </p>
      <p className="muted mt3" style={{ fontSize: "var(--t-sm)" }}>
        The app row, its artwork and anyone&rsquo;s favourite of it are kept, not deleted —
        recoverable later from the provider app&rsquo;s Library panel (&ldquo;Seen, not
        published&rdquo;) with an un-ignore action.
      </p>
    </Modal>
  );
}
