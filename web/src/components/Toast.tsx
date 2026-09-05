/**
 * Toast + ToastHost (UI-04).
 * ToastContext provides `addToast()` / `removeToast()`.
 * ToastHost renders the fixed overlay stack.
 */
import {
  createContext,
  useCallback,
  useContext,
  useId,
  useRef,
  useState,
  type ReactNode,
} from "react";

import { IconStatusDanger, IconStatusSuccess } from "./icons";

export type ToastVariant = "success" | "danger" | "info";

export interface ToastItem {
  id: string;
  variant: ToastVariant;
  title: string;
  body?: string;
  /** Auto-dismiss after ms (default 4000). `null` never dismisses — a huge
   *  number cannot stand in for it: setTimeout above 2^31-1 ms fires at once. */
  duration?: number | null;
  /**
   * One optional call-to-action button (e.g. "Go to session" — steam-library-
   * discovery spec §2.2's `home_in_use` copy needs a real link, not just a
   * toast that names the problem). Rendering a link/button here rather than
   * inlining an anchor into `body` keeps the toast a plain string pair and
   * keeps the click handler (routing) out of this generic component.
   */
  action?: {
    label: string;
    onClick: () => void;
  };
}

interface ToastContextValue {
  /** Returns the toast's id (#494) so a caller can removeToast it on a later
   * outcome (e.g. a "waiting for a slot…" toast when the retry resolves). */
  addToast: (item: Omit<ToastItem, "id">) => string;
  removeToast: (id: string) => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used inside <ToastProvider>");
  return ctx;
}

// ── Icons ──────────────────────────────────────────────────

function InfoIcon() {
  return (
    <svg className="ic" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <circle cx="8" cy="8" r="8" fill="var(--info)" />
      <path d="M8 7v4M8 5v.5" stroke="#08080c" strokeWidth="1.8" strokeLinecap="round" />
    </svg>
  );
}

// ── ToastItem component ────────────────────────────────────

interface ToastItemProps {
  item: ToastItem;
  onRemove: (id: string) => void;
}

function ToastItemComponent({ item, onRemove }: ToastItemProps) {
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Start auto-dismiss on mount
  const startTimer = useCallback(() => {
    if (item.duration === null) return;
    const ms = item.duration ?? 4000;
    timerRef.current = setTimeout(() => onRemove(item.id), ms);
  }, [item, onRemove]);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useState(() => { startTimer(); });

  return (
    <div
      className={`toast ${item.variant}`}
      // Severity-appropriate live semantics: danger interrupts (assertive
      // alert); success/info announce politely without interrupting.
      role={item.variant === "danger" ? "alert" : "status"}
      aria-live={item.variant === "danger" ? "assertive" : "polite"}
    >
      {item.variant === "success" && <IconStatusSuccess />}
      {item.variant === "danger"  && <IconStatusDanger />}
      {item.variant === "info"    && <InfoIcon />}
      <div>
        <div className="t-title">{item.title}</div>
        {item.body && <div className="t-body">{item.body}</div>}
        {item.action && (
          <button type="button" className="toast-action" onClick={() => { item.action?.onClick(); onRemove(item.id); }}>
            {item.action.label}
          </button>
        )}
      </div>
    </div>
  );
}

// ── Provider ───────────────────────────────────────────────

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);
  const baseId = useId();
  const counter = useRef(0);

  const removeToast = useCallback((id: string) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const addToast = useCallback(
    (item: Omit<ToastItem, "id">) => {
      const id = `${baseId}-${++counter.current}`;
      setToasts((prev) => [...prev, { ...item, id }]);
      return id;
    },
    [baseId],
  );

  return (
    <ToastContext.Provider value={{ addToast, removeToast }}>
      {children}
      <ToastHost toasts={toasts} onRemove={removeToast} />
    </ToastContext.Provider>
  );
}

// ── Host ───────────────────────────────────────────────────

interface ToastHostProps {
  toasts: ToastItem[];
  onRemove: (id: string) => void;
}

export function ToastHost({ toasts, onRemove }: ToastHostProps) {
  if (toasts.length === 0) return null;
  return (
    <div className="toast-host" aria-label="Notifications">
      {toasts.map((t) => (
        <ToastItemComponent key={t.id} item={t} onRemove={onRemove} />
      ))}
    </div>
  );
}
