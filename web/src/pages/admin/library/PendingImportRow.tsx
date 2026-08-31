// One discovered-but-unpublished title (handoff §A.9). No size/Proton fields
// exist on LibraryUnpublished, so the sub-line uses what it does carry:
// `users`/`last_seen_at`. `suppressed_by: "rule_ignore"` is a prior admin
// Ignore, not just "not scanned yet" — the menu offers Un-ignore (the same
// allow-rule write as Import) instead of a redundant Ignore.

import { ActionsMenu, type ActionsMenuItem } from "../../../components/ActionsMenu";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { relativeTime } from "../../../lib/format/relativeTime";
import { appGlyph } from "../../../lib/appGlyph";
import type { PendingImportItem } from "./appsFilters";

interface PendingImportRowProps {
  row: PendingImportItem;
  importing: boolean;
  onImport: () => void;
  onIgnore: () => void;
}

export function PendingImportRow({ row, importing, onImport, onIgnore }: PendingImportRowProps) {
  const { item } = row;
  const label = item.name || `Appid ${item.external_id}`;
  const ignored = item.suppressed_by === "rule_ignore";
  const sub =
    `seen on ${item.users} account${item.users === 1 ? "" : "s"} · ` +
    `last seen ${relativeTime(item.last_seen_at)}`;
  const menuItems: ActionsMenuItem[] = ignored
    ? [{ key: "unignore", label: "Un-ignore", onClick: onImport }]
    : [{ key: "ignore", label: "Ignore", onClick: onIgnore }];

  return (
    <tr style={{ background: "var(--accent-soft)" }}>
      <td>
        <div className="rowflex">
          <span
            aria-hidden="true"
            style={{
              width: 26,
              height: 26,
              flex: "none",
              borderRadius: "var(--r-xs)",
              border: "1px dashed var(--line-3)",
              display: "grid",
              placeContent: "center",
              fontFamily: "var(--font-display)",
              fontSize: 11,
              fontWeight: 700,
              color: "var(--text-3)",
            }}
          >
            {appGlyph(label)}
          </span>
          <div className="stack">
            <span className="primary">{label}</span>
            <span className="sub">{ignored ? `Ignored · ${sub}` : sub}</span>
          </div>
        </div>
      </td>
      <td>
        <Chip variant="accent">Game</Chip>
      </td>
      <td>Steam</td>
      <td colSpan={3} style={{ color: "var(--text-3)" }}>
        Not imported. Importing applies the provider preset.
      </td>
      <td className="right">
        <Chip variant="warning">pending</Chip>
      </td>
      <td className="right">
        <div className="cell-actions">
          <Button variant="secondary" size="sm" disabled={importing} onClick={onImport}>
            {importing ? "Importing…" : "Import"}
          </Button>
          <ActionsMenu items={menuItems} label={`Actions for ${label}`} />
        </div>
      </td>
    </tr>
  );
}
