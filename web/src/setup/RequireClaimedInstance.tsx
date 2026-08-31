// Bootstrap gate wrapping every route (App.tsx). A virgin instance (no admin
// yet) must never show a login screen no one can satisfy — this sends every
// path except /setup itself there. Wrapping the whole <Routes> tree (rather
// than gating inside it) means the redirect fires before route matching, so
// it works for /, /login, /register, deep links, everything.

import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { useSetupStatus } from "./useSetupStatus";
import { shouldRouteToSetup } from "./decideRoute";

export function RequireClaimedInstance({ children }: { children: ReactNode }) {
  const { status, loading } = useSetupStatus();
  const location = useLocation();

  // While the status fetch is in flight (or if it fails), render children
  // optimistically rather than blanking every page load: `status` stays
  // null, and shouldRouteToSetup treats null as "unknown, don't redirect".
  // An already-claimed instance (by far the common case) never sees a
  // flash; a virgin instance briefly shows the real route before this
  // re-renders and redirects once the fetch resolves.
  if (loading) return <>{children}</>;

  if (shouldRouteToSetup(status, location.pathname)) {
    return <Navigate to="/setup" replace />;
  }

  return <>{children}</>;
}
