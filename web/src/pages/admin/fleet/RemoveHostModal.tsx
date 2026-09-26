/**
 * "Remove host" for an owned GPU host: the confirmation, and the explanation for a
 * host that is not connected (design_handoff_v3 fleet-rh06-v3.html `rhRemove`;
 * screenshots rh06/remove-confirm, remove-offline). The flow itself is removeHost.ts.
 */

import type { Host } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { IconTrash } from "../../../components/icons";
import { elapsedWords } from "../../../lib/format/relativeTime";
import { notConnected } from "./removeHost";

export interface RemoveHostModalProps {
  host: Host;
  liveSessions: number;
  onClose: () => void;
  onConfirm: () => void;
  pending?: boolean;
  now: number;
}

export function RemoveHostModal({
  host,
  liveSessions,
  onClose,
  onConfirm,
  pending,
  now,
}: RemoveHostModalProps) {
  const node = host.node_name;
  const offline = notConnected(host, now);
  return (
    <Modal
      open
      onClose={onClose}
      title={`Remove ${node}?`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {offline ? "Close" : "Cancel"}
          </Button>
          <Button variant="danger" disabled={offline || pending} onClick={onConfirm}>
            <IconTrash />
            Remove host
          </Button>
        </>
      }
    >
      {offline ? (
        <>
          <div className="note warn rm-host-note">
            <strong>{node} is not connected</strong>
            {host.last_heartbeat_at ? ` (last seen ${elapsedWords(host.last_heartbeat_at, now)} ago)` : ""}.
            Removing a host needs it connected, so its recovery actor can stop and remove the
            containers.
          </div>
          <p className="rm-host-copy">
            Reconnect it and try again. If the machine is gone, you can leave it: an offline host
            takes no sessions, and a new machine can be added under the same node name.
          </p>
        </>
      ) : (
        <>
          <p className="rm-host-copy">
            Quasar drains <b>{node}</b> — it takes no new sessions
            {liveSessions > 0 ? (
              <>
                {" "}
                and waits for its{" "}
                <b>
                  {liveSessions} live session{liveSessions === 1 ? "" : "s"}
                </b>{" "}
                to end
              </>
            ) : null}{" "}
            — then stops and removes its node agent and recovery actor.
          </p>
          <p className="rm-host-copy">
            Its homes and data stay on the machine. The seed stays too, idle; remove it the way you
            started it, when you like. To bring the machine back, add a host with the same node
            name: its history is kept.
          </p>
        </>
      )}
    </Modal>
  );
}
