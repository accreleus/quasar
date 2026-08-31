// Gate for the /admin area. Nests inside RequireAuth, so by here the user is
// authenticated; this only checks the role.
//
// IMPORTANT: this gate is UX, not access control. It hides the admin UI from
// non-admins so they don't see dead-ends — but the *enforcement* is server-side:
// every admin API endpoint rejects a non-admin bearer token (control-api.md).
// The role we read here came from GET /v1/me (server-sourced), and even if it
// were tampered with client-side, the admin API calls would still 403. Never
// treat this component as the security boundary.

import { Navigate, Outlet } from "react-router-dom";
import { useAuth } from "./context";

export function RequireAdmin() {
  const { isAdmin } = useAuth();
  if (!isAdmin) return <Navigate to="/app" replace />;
  return <Outlet />;
}
