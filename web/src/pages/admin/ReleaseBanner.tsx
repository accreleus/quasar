// Admin-wide advisory when a newer platform release exists (#104/#110). Same
// slot and same dismissal model as SecretStoreBanner: sessionStorage, not
// localStorage, so a fresh session is told again until the instance is updated.

import { useState } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../api/admin";
import { Button } from "../../components/Button";
import { useResource } from "../../lib/resource/react";
import type { PlatformReleaseView } from "../../api/types";
import { hasUpdate, releaseLabel } from "./fleet/releasesCopy";

const DISMISS_KEY = "quasar-admin-release-banner-dismissed";

export function ReleaseBanner() {
  const [dismissed, setDismissed] = useState(
    () => sessionStorage.getItem(DISMISS_KEY) === "1",
  );

  const { data } = useResource<PlatformReleaseView>({
    label: "platform releases",
    fetch: ({ token, signal }) => adminApi.getPlatformReleases(token, signal),
  });

  if (dismissed || !data || !hasUpdate(data)) return null;
  const newest = data.available[0];

  function dismiss() {
    sessionStorage.setItem(DISMISS_KEY, "1");
    setDismissed(true);
  }

  return (
    <div
      className="note mb6"
      role="status"
      style={{ alignItems: "center", justifyContent: "space-between" }}
    >
      <div>
        <b>Quasar {releaseLabel(newest)} is available.</b> This instance is on{" "}
        {data.installed.control_plane.version}.{" "}
        <Link to="/admin/fleet/releases">See what changed</Link>.
      </div>
      <Button type="button" variant="ghost" size="sm" onClick={dismiss}>
        Dismiss for this session
      </Button>
    </div>
  );
}
