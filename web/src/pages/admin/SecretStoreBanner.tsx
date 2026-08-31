// #522: admin-wide warning when the secret store's master key isn't configured
// — Settings shows the same fact, but only to an admin who opens it. Same
// field off GET /v1/admin/secrets, surfaced from AdminLayout instead.
// Dismissal is sessionStorage, not localStorage: a fresh session sees it again
// until the operator actually sets QUASAR_SECRET_KEY.

import { useState } from "react";
import { useResource } from "../../lib/resource/react";
import * as adminApi from "../../api/admin";
import { Button } from "../../components/Button";
import type { SecretsResponse } from "../../api/types";

const DISMISS_KEY = "quasar-admin-secret-banner-dismissed";

export function SecretStoreBanner() {
  const [dismissed, setDismissed] = useState(
    () => sessionStorage.getItem(DISMISS_KEY) === "1",
  );

  const { data } = useResource<SecretsResponse>({
    label: "secret store status",
    fetch: ({ token, signal }) => adminApi.listSecrets(token, signal),
  });

  if (dismissed || !data || data.master_key_configured) return null;

  function dismiss() {
    sessionStorage.setItem(DISMISS_KEY, "1");
    setDismissed(true);
  }

  return (
    <div
      className="note warn mb6"
      role="status"
      style={{ alignItems: "center", justifyContent: "space-between" }}
    >
      <div>
        <b>No master key is configured on this control plane.</b> Admin-stored credentials
        (third-party API keys, etc.) cannot be saved until one is set. Set{" "}
        <code>QUASAR_SECRET_KEY</code> to a base64 32-byte value (
        <code>openssl rand -base64 32</code>) and back it up; losing it makes every stored
        secret unrecoverable. See <a href="/admin/settings">Settings</a> for details.
      </div>
      <Button type="button" variant="ghost" size="sm" onClick={dismiss}>
        Dismiss for this session
      </Button>
    </div>
  );
}
