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
      <div style={{ width: 1, height: 20, background: "var(--line-2)", flexShrink: 0 }} />
      {actions.map((action) => (
        <BulkAction key={action.label} action={action} />
      ))}
      <button
        style={{
          marginLeft: "var(--s2)",
          background: "transparent",
          border: "none",
          cursor: "pointer",
          color: "var(--text-3)",
          fontSize: "var(--t-xs)",
          fontFamily: "var(--font-ui)",
          padding: "2px 6px",
          borderRadius: "var(--r-sm)",
        }}
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
      style={{
        padding: "6px 14px",
        borderRadius: "var(--r-pill)",
        border: isDanger ? "1px solid var(--danger-line)" : "1px solid var(--line-2)",
        background: isDanger ? "var(--danger-bg)" : "var(--ink-5)",
        color: isDanger ? "var(--danger-text)" : "var(--text)",
        cursor: "pointer",
        fontSize: "var(--t-sm)",
        fontFamily: "var(--font-ui)",
        fontWeight: 600,
        transition: "background 0.15s",
      }}
      onClick={action.onClick}
    >
      {action.label}
    </button>
  );
}
