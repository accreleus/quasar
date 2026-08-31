// Modal for editing a user's max concurrent session quota.

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import type { AdminUser } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { TextField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { friendlyError } from "./format";

interface QuotaModalProps {
  user: AdminUser;
  token: string;
  onSave: (user: AdminUser) => void;
  onClose: () => void;
}

export function QuotaModal({ user, token, onSave, onClose }: QuotaModalProps) {
  const { addToast } = useToast();
  const [limit, setLimit] = useState(String(user.max_concurrent_sessions));
  const [saving, setSaving] = useState(false);

  const handleSave = async () => {
    const n = parseInt(limit, 10);
    if (isNaN(n) || n < 1) {
      addToast({ variant: "danger", title: "Session limit must be a positive integer" });
      return;
    }
    setSaving(true);
    try {
      const { user: updated } = await adminApi.updateUser(token, user.id, {
        max_concurrent_sessions: n,
      });
      onSave(updated);
    } catch (e: unknown) {
      addToast({ variant: "danger", title: "Save failed", body: friendlyError(e) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      onClose={onClose}
      title={`Session quota for ${user.username}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={() => void handleSave()} disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <TextField
        label="Max concurrent sessions"
        type="number"
        value={limit}
        onChange={(e) => setLimit(e.target.value)}
      />
    </Modal>
  );
}
