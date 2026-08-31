// One published app row (handoff §A.9). Row click opens the editor; the
// preset/profile links and the enabled switch stop propagation so they act
// without also navigating.

import { Link } from "react-router-dom";
import type { AdminApp } from "../../../api/types";
import { ActionsMenu, type ActionsMenuItem } from "../../../components/ActionsMenu";
import { Chip } from "../../../components/Chip";
import { appGlyph } from "../../../lib/appGlyph";
import { isSteamSourced } from "./appsFilters";

const KIND_LABEL: Record<AdminApp["kind"], string> = {
  game: "Game",
  desktop: "Desktop",
  launcher: "Launcher",
};

function imageRef(app: AdminApp): string {
  const spec = app.runtime_spec as { image?: string } | undefined;
  return spec?.image || "—";
}

interface AppRowProps {
  app: AdminApp;
  coverClass: string;
  presetName: string | null;
  profileName: string;
  toggling: boolean;
  onToggleEnabled: () => void;
  onOpen: () => void;
  onIgnore?: () => void;
  onDelete: () => void;
}

export function AppRow({
  app,
  coverClass,
  presetName,
  profileName,
  toggling,
  onToggleEnabled,
  onOpen,
  onIgnore,
  onDelete,
}: AppRowProps) {
  const menuItems: ActionsMenuItem[] = [
    { key: "open", label: "Open", onClick: onOpen },
    ...(onIgnore ? [{ key: "ignore", label: "Ignore", onClick: onIgnore } as ActionsMenuItem] : []),
    { key: "delete", label: "Delete", variant: "danger", onClick: onDelete },
  ];

  return (
    <tr className="clickable" style={app.enabled ? undefined : { opacity: 0.6 }} onClick={onOpen}>
      <td>
        <div className="rowflex">
          <span
            className={`cover ${coverClass}`}
            aria-hidden="true"
            style={{ width: 26, height: 26, flex: "none", borderRadius: "var(--r-xs)" }}
          >
            {app.cover_url ? (
              <img src={app.cover_url} alt="" className="cover-img" />
            ) : (
              <span className="glyph" style={{ fontSize: 11 }}>
                {appGlyph(app.name)}
              </span>
            )}
          </span>
          <div className="stack">
            <span className="primary">{app.name}</span>
            <span className="sub mono">{imageRef(app)}</span>
          </div>
        </div>
      </td>
      <td>
        <Chip variant={app.kind === "game" ? "accent" : "neutral"}>{KIND_LABEL[app.kind]}</Chip>
      </td>
      <td>{isSteamSourced(app) ? "Steam" : "Manual"}</td>
      <td>
        {presetName && app.runtime_preset_id ? (
          <Link
            to={`/admin/library/apps?preset=${encodeURIComponent(app.runtime_preset_id)}`}
            onClick={(e) => e.stopPropagation()}
          >
            {presetName}
          </Link>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td>
        <Link to="/admin/streaming/launch" onClick={(e) => e.stopPropagation()}>
          {profileName}
        </Link>
      </td>
      <td className="right num">{app.sessions_30d}</td>
      <td className="right">
        <button
          type="button"
          className="switch"
          role="switch"
          aria-checked={app.enabled}
          aria-label={`${app.enabled ? "Disable" : "Enable"} ${app.name}`}
          disabled={toggling}
          onClick={(e) => {
            e.stopPropagation();
            onToggleEnabled();
          }}
        />
      </td>
      <td className="right" onClick={(e) => e.stopPropagation()}>
        <div className="cell-actions">
          <ActionsMenu items={menuItems} label={`Actions for ${app.name}`} />
        </div>
      </td>
    </tr>
  );
}
