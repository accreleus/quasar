/**
 * BulkBar (UI-04).
 * Fixed pill at the bottom of the screen showing selected-row count
 * and bulk-action buttons.
 */
interface BulkBarAction {
  label: string;
  onClick: () => void;
  variant?: "default" | "danger";
}

interface BulkBarProps {
  selectedCount: number;
  actions: BulkBarAction[];
  /** Called when the user clears the selection */
  onClear: () => void;
  /** Noun for the selected items, e.g. "session" → "3 sessions selected" */
  noun?: string;
}

export function BulkBar({ selectedCount, actions, onClear, noun = "item" }: BulkBarProps) {
  if (selectedCount === 0) return null;

  const label = `${selectedCount} ${noun}${selectedCount === 1 ? "" : "s"} selected`;

  return (
    <div className="bulk-bar" role="toolbar" aria-label="Bulk actions">
      <span className="bulk-count">
        <span>{selectedCount}</span> {noun}{selectedCount === 1 ? "" : "s"} selected
      </span>
      <div className="bulk-sep" />
      {actions.map((action) => (
        <BulkAction key={action.label} action={action} />
      ))}
      <button
        className="bulk-clear muted t-xs"
        onClick={onClear}
        aria-label={`Clear ${label}`}
      >
        Clear
      </button>
    </div>
  );
}

function BulkAction({ action }: { action: BulkBarAction }) {
  const isDanger = action.variant === "danger";
  return (
    <button
      className={`bulk-action t-sm${isDanger ? " bulk-action-danger" : ""}`}
      onClick={action.onClick}
    >
      {action.label}
    </button>
  );
}
