// The pre-auth glass card — one surface behind /login, /register and /setup.
//
// handoff-v3-spec.md §C: an accent bloom field, a 24.5rem stack, and a card
// whose heading is the brand lockup (mark + wordmark, no greeting line). The
// children are the form. Styling lives in styles/login.css, scoped under
// `.auth-scene`.
//
// Dark-locked: these screens have no theme toggle and the mock is dark-only,
// so the card holds a dark lock for as long as it is mounted. That also means
// every page rendering an AuthCard must sit inside <ThemeProvider>. The lock's
// streaming hint is off — nothing is playing behind a sign-in form.

import type { CSSProperties, ReactNode } from "react";
import { QuasarMark } from "../../components/QuasarMark";
import { useDarkLock } from "../../settings/ThemeContext";

interface AuthCardProps {
  /** Overrides the 24.5rem stack width — the setup wizard runs wider. */
  width?: string;
  children: ReactNode;
}

export function AuthCard({ width, children }: AuthCardProps) {
  useDarkLock({ streaming: false });
  const stackStyle: CSSProperties | undefined = width ? { width } : undefined;

  return (
    <div className="auth-scene">
      <div className="stack" style={stackStyle}>
        <section className="card">
          <header className="lockup">
            <QuasarMark size={52} className="mark" />
            <h1 className="wordmark">Quasar</h1>
          </header>
          {children}
        </section>
      </div>
    </div>
  );
}
