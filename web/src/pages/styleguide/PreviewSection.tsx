// Command palette, HUD pill and KPI card — /styleguide. All three embed the
// real component: CommandPalette is the actual shell/CommandPalette.tsx over
// demo search sources, the HUD pill is OverlayPreview (the same `<Hud
// preview>` the account overlay page shows), and the KPI card is the actual
// KpiRow the Overview page renders, over fabricated numbers.
import { useState } from "react";
import { Button } from "../../components/Button";
import { CommandPalette } from "../../components/shell/CommandPalette";
import type {
  PaletteApp,
  PaletteHost,
  PaletteSession,
  PaletteUser,
} from "../../components/shell/paletteSearch";
import type { Kpis } from "../../lib/fleet/deriveKpis";
import { OverlayPreview } from "../app/account/OverlayPreview";
import { KPI_SERIES, KpiRow } from "../admin/overview/KpiRow";
import type { Series } from "../admin/overview/history";

const DEMO_HOSTS: PaletteHost[] = [
  { id: "h1", node_name: "quasar-node-1", status: "online" },
  { id: "h2", node_name: "quasar-node-2", status: "draining" },
];
const DEMO_SESSIONS: PaletteSession[] = [
  { id: "s1", app_name: "Cyberpunk 2077", username: "mara.k", state: "running" },
];
const DEMO_APPS: PaletteApp[] = [
  { id: "a1", name: "Half-Life 2", kind: "game" },
  { id: "a2", name: "Blender Studio", kind: "desktop" },
];
const DEMO_USERS: PaletteUser[] = [{ id: "u1", username: "mara.k", email: "mara@example.com" }];

const DEMO_KPIS: Kpis = {
  sessions: { live: 6, degraded: 1, mbpsOut: 42.3 },
  slots: { used: 14, total: 20, free: 6, onlineHosts: 5, capacityHosts: 5 },
  hosts: { online: 5, total: 6, attention: 1 },
  users: { active: 12, streaming: 6, pendingInvites: 2 },
};
const DEMO_SERIES: Series = {
  [KPI_SERIES.live]: [3, 4, 4, 5, 6, 5, 6, 6, 7, 6],
  [KPI_SERIES.slots]: [10, 11, 12, 13, 14, 13, 14, 14, 15, 14],
  [KPI_SERIES.hosts]: [5, 5, 5, 4, 5, 5, 5, 5, 5, 5],
  [KPI_SERIES.users]: [8, 9, 10, 10, 11, 11, 12, 12, 12, 12],
};

export function PreviewSection() {
  const [paletteOpen, setPaletteOpen] = useState(false);

  return (
    <>
      {/* ── Command palette ── */}
      <section className="sg-block" id="sg-command-palette">
        <h2>Command palette</h2>
        <p className="sg-desc">
          Opens on the trigger below, ⌘K/Ctrl+K or a bare /. Searches hosts, sessions, apps and
          users; admin actions only show for an admin bearer.
        </p>
        <div className="sg-specimen">
          <Button onClick={() => setPaletteOpen(true)}>Open command palette</Button>
        </div>
        <CommandPalette
          open={paletteOpen}
          onOpenChange={setPaletteOpen}
          hosts={DEMO_HOSTS}
          sessions={DEMO_SESSIONS}
          apps={DEMO_APPS}
          users={DEMO_USERS}
        />
      </section>

      {/* ── HUD pill ── */}
      <section className="sg-block" id="sg-hud-pill">
        <h2>HUD pill</h2>
        <p className="sg-desc">
          The real <code className="mono">Hud</code> in preview mode, the same component the
          account overlay page shows. No document listeners, no idle clock, one static telemetry
          snapshot.
        </p>
        <div className="sg-specimen">
          <OverlayPreview />
        </div>
      </section>

      {/* ── KPI card ── */}
      <section className="sg-block" id="sg-kpi-card">
        <h2>KPI card</h2>
        <p className="sg-desc">
          The four cards the Overview page opens with, over fabricated numbers. Each card links to
          the rows behind it and carries a sparkline of its own recent history.
        </p>
        <div className="sg-specimen">
          <KpiRow kpis={DEMO_KPIS} series={DEMO_SERIES} />
        </div>
      </section>
    </>
  );
}
