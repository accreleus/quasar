// Layout for the /admin/* operator area (IA in ./adminNav.ts). The RequireAdmin
// route gate is UX only — the admin API enforces the role server-side.

import { Outlet } from "react-router-dom";
import * as adminApi from "../../api/admin";
import type { AdminApp, AdminUser, InstanceSettings } from "../../api/types";
import { ConsoleShell } from "../../components/shell/ConsoleShell";
import { useResource } from "../../lib/resource/react";
import { ADMIN_NAV } from "./adminNav";
import { FleetProvider, useFleetBadges, useFleetContext } from "../../lib/fleet/FleetContext";
import { SetupResumeBanner } from "../setup/SetupResumeBanner";
import { SecretStoreBanner } from "./SecretStoreBanner";

// #386: admin.css imports here, not main.tsx, so it ships only in this lazy
// chunk — non-admin users never download it.
import "../../styles/admin.css";

export function AdminLayout() {
  // The provider sits above the shell so the rail badges, the palette and every
  // page under the Outlet read one poll of each (lib/fleet/FleetContext).
  return (
    <FleetProvider>
      <AdminConsoleShell />
    </FleetProvider>
  );
}

function AdminConsoleShell() {
  const badges = useFleetBadges();
  const { hosts, sessions } = useFleetContext();

  // The palette's other two sources (spec §3.2). Neither polls: a catalogue and
  // a user list change when an operator changes them, not on a timer, and
  // nothing else on the console reads either through the shell.
  const apps = useResource({
    label: "app catalogue",
    initialData: [] as AdminApp[],
    fetch: async (ctx) => (await adminApi.listAdminApps(ctx.token)).items,
  });
  const users = useResource({
    label: "users",
    initialData: [] as AdminUser[],
    fetch: async (ctx) => (await adminApi.listUsers(ctx.token)).items,
  });

  return (
    <ConsoleShell
      sections={[{ items: ADMIN_NAV }]}
      badges={badges}
      railLabel="Admin sections"
      palette={{ hosts, sessions, apps: apps.data ?? [], users: users.data ?? [] }}
    >
      <SetupResumeBanner />
      <SecretStoreBanner />
      <Outlet />
    </ConsoleShell>
  );
}

/**
 * Outlet context for /admin/* pages: a settings PATCH can report itself here.
 *
 * Nothing subscribes or calls this today. The v1 rail rendered a nav row per
 * enabled library provider and re-rendered on this callback; the v3 rail is a
 * flat, static eight-row list and gives providers the Library -> Sources page
 * instead (spec §3.3). The type stays because Sources is its next consumer.
 */
export interface AdminOutletContext {
  onSettingsChanged: (settings: InstanceSettings) => void;
}
