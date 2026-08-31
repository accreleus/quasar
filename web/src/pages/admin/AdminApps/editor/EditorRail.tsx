// The editor's right rail (handoff §A.10 `aeRail`): what this app is, beside
// the tabs that edit what it does. Enabled writes straight through instead of
// joining the draft, so hiding a misbehaving app never waits on a save.

import { Link } from "react-router-dom";
import type { AdminApp } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { Switch } from "../../../../components/TextField";
import { AppFrame, Fact } from "./primitives";
import { appSourceLabel } from "./editorTabs";

interface EditorRailProps {
  app: AdminApp;
  presetName: string | null;
  launchProfileName: string;
  /** null when no catalog image is this app's: the fact is omitted rather
   *  than reporting somebody else's host count. */
  imageHosts: { ready: number; total: number } | null;
  onToggleEnabled: (next: boolean) => void;
  togglePending: boolean;
  onDelete: () => void;
}

export function EditorRail({
  app,
  presetName,
  launchProfileName,
  imageHosts,
  onToggleEnabled,
  togglePending,
  onDelete,
}: EditorRailProps) {
  return (
    <div className="ae-rail">
      <div className="card" style={{ overflow: "hidden" }}>
        <AppFrame name={app.name} url={app.hero_url ?? app.cover_url} variant="hero" flush />
        <div className="card-pad" style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
          <div className="rowflex" style={{ justifyContent: "space-between" }}>
            <label className="label" htmlFor="app-enabled">
              Enabled for users
            </label>
            <Switch
              checked={app.enabled}
              onChange={onToggleEnabled}
              disabled={togglePending}
              id="app-enabled"
            />
          </div>
          <div className="ae-facts">
            <Fact label="Sessions · 30d">
              <span className="num">{app.sessions_30d}</span>
            </Fact>
            {imageHosts && (
              <Fact label="Image present on">
                <Link to="/admin/library/images" className="num">
                  {imageHosts.ready} / {imageHosts.total} hosts
                </Link>
              </Fact>
            )}
            <Fact label="Runtime preset">
              {app.runtime_preset_id && presetName ? (
                <Link to={`/admin/library/presets?preset=${app.runtime_preset_id}`}>{presetName}</Link>
              ) : (
                <span className="muted">None</span>
              )}
            </Fact>
            <Fact label="Launch profile">{launchProfileName}</Fact>
            <Fact label="Source">{appSourceLabel(app)}</Fact>
          </div>
        </div>
      </div>
      <Button variant="danger" onClick={onDelete} style={{ width: "100%", justifyContent: "center" }}>
        Delete app
      </Button>
      <p className="hint">
        Deleting removes every user&rsquo;s favourite of this tile and its artwork with it. Disable
        it above to hide it from the library instead.
      </p>
    </div>
  );
}
