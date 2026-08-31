// Route map for the unified client. Two role-separated areas behind auth:
//   /app/*   — user UI (RequireAuth)
//   /admin/* — operator UI (RequireAuth + RequireAdmin)
// plus the public /login. Both areas share the API client, auth, and components.
//
// Bundle split (#139): the entire /admin/* area is lazy-loaded as a separate
// chunk via React.lazy. Users (non-admin) never download admin code. This is a
// loading optimisation ONLY — authorization stays server-enforced at the API
// layer (CLAUDE.md invariant #6); the RequireAdmin guard remains unchanged.

import { lazy, Suspense, type ReactNode } from "react";
import { Navigate, Route, Routes, useLocation, useParams } from "react-router-dom";
import { RequireAdmin } from "./auth/RequireAdmin";
import { RequireAuth } from "./auth/RequireAuth";
import { LoginPage } from "./pages/LoginPage";
import { NotFound } from "./pages/NotFound";
import { RegisterPage } from "./pages/RegisterPage";
import { SetupWizard } from "./pages/setup/SetupWizard";
import { RequireClaimedInstance } from "./setup/RequireClaimedInstance";
import { AccountLayout } from "./pages/app/account/AccountLayout";
import {
  AccountSection,
  PreferencesSection,
  UsageSection,
} from "./pages/app/account/AccountSections";
import { AccountOverlay } from "./pages/app/account/AccountOverlay";
import { AccountProfile } from "./pages/app/account/AccountProfile";
import { AccountDevices } from "./pages/app/account/AccountDevices";
import { AccountSessions } from "./pages/app/account/AccountSessions";
import { AccountStreaming } from "./pages/app/account/AccountStreaming";
import { AppHomeNext } from "./pages/app/AppHomeNext";
import { AppLayout } from "./pages/app/AppLayout";
import { AccountStorage } from "./pages/app/account/AccountStorage";
import { SessionPage } from "./pages/app/SessionPage";
import { StyleguidePage } from "./pages/StyleguidePage";
import { RouteErrorBoundary, RouteSkeleton } from "./components/RouteBoundary";
import { OverlayPreferencesRoute } from "./settings/OverlayPreferencesContext";

// Admin area — lazy-loaded. Vite splits this into a separate chunk automatically
// because of the dynamic import boundary. The Suspense fallback is intentionally
// minimal: the shell nav is already visible (RequireAuth + RequireAdmin have
// rendered) so a full-page spinner is not needed.
// Helper: wrap a lazy-imported component in Suspense with null fallback.
const lazyPage = (
  loader: () => Promise<{ default: React.ComponentType<any> }>
) => lazy(loader);

const Suspended = ({ children }: { children: ReactNode }) => (
  <RouteErrorBoundary>
    <Suspense fallback={<RouteSkeleton />}>{children}</Suspense>
  </RouteErrorBoundary>
);

// Admin pages table: each entry is a separate lazy() chunk boundary.
const adminPages = {
  Fleet: lazyPage(() => import("./pages/admin/Fleet").then((m) => ({ default: m.Fleet }))),
  Library: lazyPage(() => import("./pages/admin/Library").then((m) => ({ default: m.Library }))),
  Streaming: lazyPage(() =>
    import("./pages/admin/Streaming").then((m) => ({ default: m.Streaming }))
  ),
  People: lazyPage(() => import("./pages/admin/People").then((m) => ({ default: m.People }))),
  Layout: lazyPage(() =>
    import("./pages/admin/AdminLayout").then((m) => ({ default: m.AdminLayout }))
  ),
  Overview: lazyPage(() =>
    import("./pages/admin/Overview").then((m) => ({ default: m.Overview }))
  ),
  Apps: lazyPage(() =>
    import("./pages/admin/library/AppsTab").then((m) => ({ default: m.AppsTab }))
  ),
  StreamProfiles: lazyPage(() =>
    import("./pages/admin/streaming/ProfilesTab").then((m) => ({
      default: m.ProfilesTab,
    }))
  ),
  LaunchProfiles: lazyPage(() =>
    import("./pages/admin/streaming/LaunchTab").then((m) => ({
      default: m.LaunchTab,
    }))
  ),
  RuntimePresets: lazyPage(() =>
    import("./pages/admin/library/PresetsTab").then((m) => ({
      default: m.PresetsTab,
    }))
  ),
  Sources: lazyPage(() =>
    import("./pages/admin/library/SourcesTab").then((m) => ({
      default: m.SourcesTab,
    }))
  ),
  Images: lazyPage(() =>
    import("./pages/admin/library/ImagesTab").then((m) => ({ default: m.ImagesTab }))
  ),
  ImageDetail: lazyPage(() =>
    import("./pages/admin/ImageDetail").then((m) => ({ default: m.ImageDetail }))
  ),
  Jobs: lazyPage(() =>
    import("./pages/admin/fleet/JobsTab").then((m) => ({ default: m.JobsTab }))
  ),
  AppEditor: lazyPage(() =>
    import("./pages/admin/AdminApps/AppEditorPage").then((m) => ({
      default: m.AppEditorPage,
    }))
  ),
  Hosts: lazyPage(() =>
    import("./pages/admin/fleet/HostsTab").then((m) => ({ default: m.HostsTab }))
  ),
  HostDetail: lazyPage(() =>
    import("./pages/admin/HostDetail").then((m) => ({ default: m.HostDetail }))
  ),
  HostSettings: lazyPage(() =>
    import("./pages/admin/fleet/HostSettings").then((m) => ({
      default: m.HostSettings,
    }))
  ),
  Console: lazyPage(() =>
    import("./pages/admin/fleet/HostConsole").then((m) => ({
      default: m.HostConsole,
    }))
  ),
  Sessions: lazyPage(() =>
    import("./pages/admin/Sessions").then((m) => ({
      default: m.Sessions,
    }))
  ),
  SessionDetail: lazyPage(() =>
    import("./pages/admin/SessionDetail").then((m) => ({
      default: m.SessionDetail,
    }))
  ),
  Users: lazyPage(() =>
    import("./pages/admin/people/UsersTab").then((m) => ({ default: m.UsersTab }))
  ),
  Invites: lazyPage(() =>
    import("./pages/admin/people/InvitesTab").then((m) => ({ default: m.InvitesTab }))
  ),
  Storage: lazyPage(() =>
    import("./pages/admin/fleet/StorageTab").then((m) => ({
      default: m.StorageTab,
    }))
  ),
  Audit: lazyPage(() =>
    import("./pages/admin/Audit").then((m) => ({ default: m.Audit }))
  ),
  Settings: lazyPage(() =>
    import("./pages/admin/Settings").then((m) => ({ default: m.Settings }))
  ),
};

/**
 * A legacy path kept alive. Substitutes the matched route params into `to`
 * (`/admin/fleet/hosts/:id/console` -> `/admin/fleet/hosts/:id/console`) and carries
 * the query string across, so a bookmarked `?preset=` survives the move.
 * Exported for App.routes.test.tsx.
 */
export function RedirectWithParams({ to }: { to: string }) {
  const params = useParams();
  const { search, hash } = useLocation();
  // Router params arrive decoded; re-encode so an id with `/` or `%` cannot
  // produce a malformed target.
  const pathname = to.replace(/:([A-Za-z0-9_]+)/g, (whole, key: string) => {
    const value = params[key];
    return value === undefined ? whole : encodeURIComponent(value);
  });
  return <Navigate to={{ pathname, search, hash }} replace />;
}

// `/app`'s landing page. AppHomeNext (screens/home.html) is the only one —
// the classic page (screens/library.html) and its build-time rollback flag
// were retired 2026-08-12 by operator decision.
const AppIndexPage = AppHomeNext;

export function App() {
  return (
    <RouteErrorBoundary>
    <RequireClaimedInstance>
    <Routes>
      <Route path="/" element={<Navigate to="/app" replace />} />
      <Route path="/login" element={<LoginPage />} />
      <Route path="/register" element={<RegisterPage />} />
      {/* First-run wizard (Spec B W1). Unauthenticated shell — must render
          with no session, since a virgin instance has no admin to sign in
          as yet. RequireClaimedInstance (above) is what routes a virgin
          instance here from any other path; this Route makes /setup itself
          resolve once it's there, and lets an authenticated admin reach it
          directly (resume) too. */}
      <Route path="/setup" element={<SetupWizard />} />

      {/* User area — any authenticated user. */}
      <Route element={<RequireAuth />}>
       <Route element={<OverlayPreferencesRoute />}>
        <Route path="/app" element={<AppLayout />}>
          <Route index element={<AppIndexPage />} />
          {/* The Library view is a REAL route, not component state: the topbar
              nav entries are plain links, the grouped view is bookmarkable, and
              back/forward works. Same component — AppHomeNext reads the route
              to choose its view. */}
          <Route path="library" element={<AppIndexPage />} />
          {/* Storage moved into the account area (2026-08-05). Kept as a
              redirect, not deleted — existing bookmarks and any link still in
              the wild must land on the page, not a 404. */}
          <Route
            path="storage"
            element={<Navigate to="/app/account/storage" replace />}
          />
        </Route>
        {/* /app/account moves OUT of AppLayout: AccountLayout supplies its own
            rail shell (mirroring the admin console), which would conflict with
            AppLayout's topbar pill nav. */}
        <Route path="/app/account" element={<AccountLayout />}>
          <Route index element={<Navigate to="/app/account/profile" replace />} />
          {/* Three section containers, each owning one head and its tab row
              (handoff §A.22). The pathless layout routes keep the URLs flat. */}
          <Route element={<AccountSection />}>
            <Route path="profile" element={<AccountProfile />} />
            <Route path="devices" element={<AccountDevices />} />
          </Route>
          <Route element={<PreferencesSection />}>
            <Route path="overlay" element={<AccountOverlay />} />
            <Route path="streaming" element={<AccountStreaming />} />
          </Route>
          <Route element={<UsageSection />}>
            <Route path="storage" element={<AccountStorage />} />
            <Route path="sessions" element={<AccountSessions />} />
          </Route>
          {/* The password card merges into Profile in v3 (spec §3.3), so
              /security is a redirect, not a page. */}
          <Route path="security" element={<Navigate to="/app/account/profile" replace />} />
        </Route>
        {/* Full-screen session view — outside AppLayout (no shell nav). */}
        <Route path="/app/session/:id" element={<SessionPage />} />

        {/* Admin area — authenticated AND role=admin (UX gate; API enforces).
            Suspense wraps the lazy subtree; RequireAdmin is eagerly loaded.

            Four of the eight rail destinations are SECTIONS: the container
            route renders the head and the tab row, its children render only
            their content (spec §3.3). Drill-downs (host settings, the app
            editor) sit OUTSIDE the container — they carry their own head and
            breadcrumbs, not a tab row.

            Every pre-v3 path stays as a redirect: bookmarks and anything
            already in the wild must land on the page, never on a 404. Our own
            links point at the new paths. */}
        <Route element={<RequireAdmin />}>
          <Route
            path="/admin"
            element={
              <Suspense fallback={<RouteSkeleton label="Loading administration" />}>
                <adminPages.Layout />
              </Suspense>
            }
          >
            <Route
              index
              element={
                <Suspended>
                  <adminPages.Overview />
                </Suspended>
              }
            />
            <Route
              path="settings"
              element={
                <Suspended>
                  <adminPages.Settings />
                </Suspended>
              }
            />
            <Route
              path="sessions"
              element={
                <Suspended>
                  <adminPages.Sessions />
                </Suspended>
              }
            />
            <Route
              path="sessions/:id"
              element={
                <Suspended>
                  <adminPages.SessionDetail />
                </Suspended>
              }
            />
            <Route
              path="audit"
              element={
                <Suspended>
                  <adminPages.Audit />
                </Suspended>
              }
            />

            <Route
              path="fleet"
              element={
                <Suspended>
                  <adminPages.Fleet />
                </Suspended>
              }
            >
              <Route index element={<Navigate to="/admin/fleet/hosts" replace />} />
              <Route
                path="hosts"
                element={
                  <Suspended>
                    <adminPages.Hosts />
                  </Suspended>
                }
              />
              <Route
                path="storage"
                element={
                  <Suspended>
                    <adminPages.Storage />
                  </Suspended>
                }
              />
              <Route
                path="jobs"
                element={
                  <Suspended>
                    <adminPages.Jobs />
                  </Suspended>
                }
              />
            </Route>
            <Route
              path="fleet/hosts/:id"
              element={
                <Suspended>
                  <adminPages.HostDetail />
                </Suspended>
              }
            />
            <Route
              path="fleet/hosts/:id/settings"
              element={
                <Suspended>
                  <adminPages.HostSettings />
                </Suspended>
              }
            />
            <Route
              path="fleet/hosts/:id/console"
              element={
                <Suspended>
                  <adminPages.Console />
                </Suspended>
              }
            />

            <Route
              path="library"
              element={
                <Suspended>
                  <adminPages.Library />
                </Suspended>
              }
            >
              <Route index element={<Navigate to="/admin/library/apps" replace />} />
              <Route
                path="apps"
                element={
                  <Suspended>
                    <adminPages.Apps />
                  </Suspended>
                }
              />
              <Route
                path="presets"
                element={
                  <Suspended>
                    <adminPages.RuntimePresets />
                  </Suspended>
                }
              />
              <Route
                path="images"
                element={
                  <Suspended>
                    <adminPages.Images />
                  </Suspended>
                }
              />
              {/* admin-libraries-ia spec §3: registered whether or not Steam
                  discovery is enabled — a direct URL must render the row
                  regardless (Task 25: disabled discovery still renders the
                  row, never an empty card). */}
              <Route
                path="sources"
                element={
                  <Suspended>
                    <adminPages.Sources />
                  </Suspended>
                }
              />
            </Route>
            {/* The editor's tab is a route segment (handoff §A.10), so a tab is
                linkable and the back button walks tabs. Both shapes hit the
                same page; the bare one is Identity. */}
            <Route
              path="library/apps/:id"
              element={
                <Suspended>
                  <adminPages.AppEditor />
                </Suspended>
              }
            />
            <Route
              path="library/apps/:id/:tab"
              element={
                <Suspended>
                  <adminPages.AppEditor />
                </Suspended>
              }
            />
            <Route
              path="library/images/:id"
              element={
                <Suspended>
                  <adminPages.ImageDetail />
                </Suspended>
              }
            />

            <Route
              path="streaming"
              element={
                <Suspended>
                  <adminPages.Streaming />
                </Suspended>
              }
            >
              <Route index element={<Navigate to="/admin/streaming/launch" replace />} />
              <Route
                path="launch"
                element={
                  <Suspended>
                    <adminPages.LaunchProfiles />
                  </Suspended>
                }
              />
              <Route
                path="profiles"
                element={
                  <Suspended>
                    <adminPages.StreamProfiles />
                  </Suspended>
                }
              />
            </Route>

            <Route
              path="people"
              element={
                <Suspended>
                  <adminPages.People />
                </Suspended>
              }
            >
              <Route index element={<Navigate to="/admin/people/users" replace />} />
              <Route
                path="users"
                element={
                  <Suspended>
                    <adminPages.Users />
                  </Suspended>
                }
              />
              <Route
                path="invites"
                element={
                  <Suspended>
                    <adminPages.Invites />
                  </Suspended>
                }
              />
            </Route>

            {/* Pre-v3 paths. */}
            <Route path="hosts" element={<RedirectWithParams to="/admin/fleet/hosts" />} />
            <Route
              path="hosts/:id/settings"
              element={<RedirectWithParams to="/admin/fleet/hosts/:id/settings" />}
            />
            <Route
              path="hosts/:id/console"
              element={<RedirectWithParams to="/admin/fleet/hosts/:id/console" />}
            />
            <Route path="storage" element={<RedirectWithParams to="/admin/fleet/storage" />} />
            <Route path="jobs" element={<RedirectWithParams to="/admin/fleet/jobs" />} />
            <Route path="apps" element={<RedirectWithParams to="/admin/library/apps" />} />
            <Route
              path="apps/:id"
              element={<RedirectWithParams to="/admin/library/apps/:id" />}
            />
            <Route
              path="runtime-presets"
              element={<RedirectWithParams to="/admin/library/presets" />}
            />
            <Route path="images" element={<RedirectWithParams to="/admin/library/images" />} />
            <Route
              path="libraries/steam"
              element={<RedirectWithParams to="/admin/library/sources" />}
            />
            <Route
              path="launch-profiles"
              element={<RedirectWithParams to="/admin/streaming/launch" />}
            />
            <Route
              path="stream-profiles"
              element={<RedirectWithParams to="/admin/streaming/profiles" />}
            />
            <Route path="users" element={<RedirectWithParams to="/admin/people/users" />} />
            <Route path="invites" element={<RedirectWithParams to="/admin/people/invites" />} />
            <Route path="activity" element={<RedirectWithParams to="/admin/audit" />} />
          </Route>
        </Route>
       </Route>
      </Route>

      {/* Dev-only: token/component showcase */}
      <Route path="/styleguide" element={<StyleguidePage />} />

      <Route path="*" element={<NotFound />} />
    </Routes>
    </RequireClaimedInstance>
    </RouteErrorBoundary>
  );
}
