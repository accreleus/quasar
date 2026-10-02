/**
 * "Turn console mode on/off" — the confirmation before the PATCH that starts
 * a recovery-actor replacement (amendment 18). Only a host reporting `access`
 * asks first; a host with none saves the field with the rest, as before.
 */

import { Button } from "../../../../components/Button";
import { Modal } from "../../../../components/Modal";
import { liveSessionsNoun } from "./access";

export function ConsoleAccessConfirmModal({
  hostName,
  turningOn,
  liveSessions,
  pending,
  onCancel,
  onConfirm,
}: {
  hostName: string;
  /** True: turning it on. False: turning it off. */
  turningOn: boolean;
  liveSessions: number | null;
  pending: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const verb = turningOn ? "on" : "off";
  return (
    <Modal
      open
      onClose={onCancel}
      title={`Turn ${verb} console mode on ${hostName}?`}
      footer={
        <>
          <Button variant="ghost" onClick={onCancel} disabled={pending}>
            Cancel
          </Button>
          <Button variant="primary" onClick={onConfirm} disabled={pending}>
            {pending ? "Working…" : `Turn ${verb} console mode`}
          </Button>
        </>
      }
    >
      <p>
        Its recovery actor replaces the node agent with {turningOn ? "one" : "the previous one"}{" "}
        that can use this machine's display, sound and monitor control.
      </p>
      <p>
        The <b>{liveSessionsNoun(liveSessions)}</b> on this host ends. If the new node agent does
        not become healthy, the previous one is put back and console mode stays{" "}
        {turningOn ? "off" : "on"}.
      </p>
    </Modal>
  );
}
