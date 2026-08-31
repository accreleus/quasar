// Two tiers of in-session notice: toast (informational, self-dismissing,
// top-right — "bitrate reduced", "now playing X") vs. banner (full-width at
// the top, used only when there's a button: Stop, Retry, Cancel). A notice
// with an action must never time out; one without must never demand
// dismissal — kept as separate types so that can't erode.
//
// v3 (handoff §E "Other overlay chrome"): the toast is `.ev-toast` and the
// banner is `.banner`, a full-width bar with a danger bottom border that
// pushes a top-docked HUD down (`.session-root.banner-on`).

import { useCallback, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";

/** How long an informational toast stays up. */
const TOAST_MS = 4200;

export interface SessionToast {
  id: number;
  text: ReactNode;
}

export function useSessionToast(): {
  toast: SessionToast | null;
  push: (text: ReactNode) => void;
} {
  const [toast, setToast] = useState<SessionToast | null>(null);
  const seq = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const push = useCallback((text: ReactNode) => {
    seq.current += 1;
    setToast({ id: seq.current, text });
  }, []);

  useEffect(() => {
    if (!toast) return;
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setToast(null), TOAST_MS);
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, [toast]);

  return { toast, push };
}

export function SessionToastHost({ toast }: { toast: SessionToast | null }) {
  if (!toast) return null;
  return (
    <div className="ev-toast" role="status" key={toast.id}>
      {toast.text}
    </div>
  );
}

export interface SessionBannerProps {
  /** Headline. Danger-coloured unless `variant` says otherwise. */
  title: ReactNode;
  message: ReactNode;
  /** `critical` announces as an alert; `warning` is a status line. */
  variant?: "critical" | "warning";
  /** The buttons. A banner without one should have been a toast. */
  actions?: ReactNode;
}

/**
 * One full-width banner. Rendered inside `SessionBannerHost` so several live
 * notices stack instead of covering each other — the defect that produced the
 * v1 bottom stack, kept fixed structurally rather than by tuned offsets.
 */
export function SessionBanner({ title, message, variant = "critical", actions }: SessionBannerProps) {
  return (
    <div
      className={`banner${variant === "warning" ? " warning" : ""}`}
      role={variant === "critical" ? "alert" : "status"}
    >
      <div className="bt">
        <strong>{title}</strong>
        <span>{message}</span>
      </div>
      {actions && <div className="ba">{actions}</div>}
    </div>
  );
}

/** The stack. Renders nothing (and takes no space) when there is no banner. */
export function SessionBannerHost({ children }: { children: ReactNode }) {
  return <div className="session-banners">{children}</div>;
}
