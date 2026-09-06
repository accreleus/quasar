import { Link } from "react-router-dom";
import type { CatalogImage } from "../../../api/types";
import * as adminApi from "../../../api/admin";
import { Switch } from "../../../components/TextField";
import { ResourceStates } from "../../../components/ResourceStates";
import { useResource } from "../../../lib/resource/react";
import type { UseSettingsResult } from "../settings/useSettings";
import { SteamPreparationStatus } from "./SteamPreparationStatus";

export function SteamPreparation({ settings }: { settings: UseSettingsResult }) {
  const enabled = settings.settings?.steam_preparation_enabled;
  const images = useResource<CatalogImage[]>({
    label: "Steam preparation status",
    fetch: async ({ token, signal }) => (await adminApi.listImages(token, signal)).images.filter((image) => image.id === "steam"),
    pollMs: 5000,
  }, []);
  const image = images.data?.[0];
  return (
    <div className="col gap3" style={{ marginTop: "var(--s4)" }}>
      <div className="rowflex">
        <label htmlFor="steam-preparation-enabled" className="label">Prepare Steam for faster first launch</label>
        <Switch id="steam-preparation-enabled" aria-label="Prepare Steam for faster first launch"
          checked={enabled === true} disabled={typeof enabled !== "boolean" || settings.pending === "steam_preparation_enabled"}
          onChange={(next) => void settings.patch("steam_preparation_enabled", next)} />
      </div>
      <p className="hint" style={{ margin: 0 }}>
        Prepare Steam in the background after installation or updates, then reuse the prepared files for new users’ first launches.
        Existing homes and running sessions are preserved. Discovery is controlled separately.
      </p>
      {typeof enabled !== "boolean" && <p className="note warn">Upgrade the control plane to configure Steam preparation.</p>}
      <ResourceStates loading={images.loading} error={images.errorMessage} />
      {image?.hosts?.map((host) => (
        <div key={host.host_id} className="col gap2">
          <Link to={`/admin/fleet/hosts/${host.host_id}`}>{host.node_name || host.host_id}</Link>
          <SteamPreparationStatus status={host.steam_preparation} desiredEnabled={enabled} />
        </div>
      ))}
      {!images.loading && !images.errorMessage && !image?.hosts?.length && (
        <p className="hint">Per-host preparation status appears after the supported Steam image is installed.</p>
      )}
      <Link to="/admin/library/images/steam">View Steam image details</Link>
    </div>
  );
}
