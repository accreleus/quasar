// Gate that requires an authenticated session. Unauthenticated users are sent to
// /login, preserving where they were headed so login can bounce them back.

import { Navigate, Outlet, useLocation } from "react-router-dom";
import { useAuth } from "./context";

export function RequireAuth() {
  const { status } = useAuth();
  const location = useLocation();

  if (status === "loading") return <FullScreenMessage>Loading…</FullScreenMessage>;
  if (status === "unauthenticated") {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  return <Outlet />;
}

function FullScreenMessage({ children }: { children: React.ReactNode }) {
  return <div className="centered-screen muted">{children}</div>;
}
