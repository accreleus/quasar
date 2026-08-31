// Components section — /styleguide. Every example below composes the real
// component from web/src/components (or the real page-level primitive, for
// tabs), never a redrawn lookalike.
import { useState } from "react";
import { ActionsMenu } from "../../components/ActionsMenu";
import { Bar, Gauge } from "../../components/Bar";
import { Button } from "../../components/Button";
import { Sparkline } from "../../components/Charts";
import { Chip, LiveDot } from "../../components/Chip";
import { Drawer } from "../../components/Drawer";
import { IconMore } from "../../components/icons";
import { SegmentedControl } from "../../components/SegmentedControl";
import { Table } from "../../components/Table";
import type { TableColumn } from "../../components/Table";
import { Checkbox, SearchInput, SelectField, Switch, TextField } from "../../components/TextField";
import { SectionHeadProvider } from "../../components/shell/sectionHead";
import type { SectionTab } from "../../components/shell/sectionTabs";

function CompLabel({ children }: { children: React.ReactNode }) {
  return <div className="sg-comp-label">{children}</div>;
}

function CompBlock({ children }: { children: React.ReactNode }) {
  return <div className="sg-comp-block">{children}</div>;
}

function SpecRow({ children }: { children: React.ReactNode }) {
  return <div className="sg-specimen-row">{children}</div>;
}

interface DemoSession {
  id: string;
  user: string;
  state: "Running" | "Connecting";
  telemetry: string;
}

const DEMO_SESSIONS: DemoSession[] = [
  { id: "743a921f", user: "7622e7d4", state: "Running", telemetry: "48 fps · 6153 kbps · RTT 4 ms" },
  { id: "9be1c40a", user: "deltest", state: "Connecting", telemetry: "negotiating" },
];

const SESSION_COLS: TableColumn<DemoSession>[] = [
  { key: "id", header: "Session", render: (r) => <span className="cell-id mono">{r.id}</span> },
  { key: "user", header: "User", render: (r) => <span className="primary">{r.user}</span> },
  {
    key: "state",
    header: "State",
    render: (r) =>
      r.state === "Running" ? (
        <Chip variant="success" dot>Running</Chip>
      ) : (
        <Chip variant="info">Connecting</Chip>
      ),
  },
  {
    key: "telemetry",
    header: "Telemetry",
    render: (r) => <span className="mono sub" style={{ fontSize: "var(--t-xs)" }}>{r.telemetry}</span>,
  },
  {
    key: "actions",
    header: "",
    render: () => (
      <div className="cell-actions">
        <Button size="sm">Details</Button>
        <Button size="sm" variant="danger">Stop</Button>
      </div>
    ),
  },
];

const SPARK_POINTS = [12, 40, 44, 46, 45, 47, 46, 48, 47, 46, 48, 47];

const DEMO_TABS: SectionTab[] = [
  { id: "overview", label: "Overview", to: "/styleguide" },
  { id: "detail", label: "Detail", to: "/styleguide" },
  { id: "history", label: "History", to: "/styleguide" },
];

export function ComponentsSection() {
  const [segment, setSegment] = useState<"active" | "all">("active");
  const [switchOn, setSwitchOn] = useState(true);
  const [checked, setChecked] = useState(true);
  const [drawerOpen, setDrawerOpen] = useState(false);

  return (
    <>
      {/* ── Buttons ── */}
      <section className="sg-block" id="sg-buttons">
        <h2>Buttons</h2>
        <p className="sg-desc">Four variants, three sizes, an icon-only form and the disabled state.</p>
        <div className="sg-specimen">
          <SpecRow>
            <Button variant="primary">Launch session</Button>
            <Button variant="secondary">Secondary</Button>
            <Button variant="ghost">Ghost</Button>
            <Button variant="danger">Force stop</Button>
            <Button variant="primary" size="lg">Play</Button>
            <Button size="sm">Small</Button>
            <Button disabled>Disabled</Button>
            <Button iconOnly aria-label="More options">
              <IconMore />
            </Button>
          </SpecRow>
        </div>
      </section>

      {/* ── Chips and dots ── */}
      <section className="sg-block" id="sg-chips-and-dots">
        <h2>Chips and dots</h2>
        <p className="sg-desc">
          Fixed-vocabulary status badges. A dot marks a live/running state; the live dot pulses on
          its own.
        </p>
        <div className="sg-specimen">
          <SpecRow>
            <Chip variant="success" dot>Online</Chip>
            <Chip variant="success">Running</Chip>
            <Chip variant="warning">Draining</Chip>
            <Chip variant="danger">Offline</Chip>
            <Chip variant="info">Connecting</Chip>
            <Chip variant="accent">Admin</Chip>
            <Chip variant="neutral">User</Chip>
            <LiveDot />
          </SpecRow>
        </div>
      </section>

      {/* ── Tabs and segmented ── */}
      <section className="sg-block" id="sg-tabs-and-segmented">
        <h2>Tabs and segmented</h2>
        <p className="sg-desc">
          Tabs own a route, published by <code className="mono">SectionHeadProvider</code>, the
          real head every admin section renders. A segmented control is for one page choosing
          between mutually exclusive views, with no route of its own.
        </p>

        <CompLabel>Tabs (SectionHeadProvider)</CompLabel>
        <CompBlock>
          <div className="sg-specimen" style={{ padding: 0, overflow: "hidden" }}>
            <SectionHeadProvider title="Preview section" tabs={DEMO_TABS}>
              <div style={{ padding: "var(--s5)", color: "var(--text-3)", fontSize: "var(--t-sm)" }}>
                Tab content renders here.
              </div>
            </SectionHeadProvider>
          </div>
        </CompBlock>

        <CompLabel>Segmented control</CompLabel>
        <CompBlock>
          <div className="sg-specimen">
            <SegmentedControl
              aria-label="Session filter"
              options={[
                { value: "active", label: "Active" },
                { value: "all", label: "All" },
              ]}
              value={segment}
              onChange={setSegment}
            />
          </div>
        </CompBlock>
      </section>

      {/* ── Inputs and switches ── */}
      <section className="sg-block" id="sg-inputs-and-switches">
        <h2>Inputs and switches</h2>
        <p className="sg-desc">Text, select, search, switch and checkbox, all on the input surface token.</p>
        <div className="sg-specimen">
          <div className="sg-grid" style={{ gridTemplateColumns: "repeat(3,1fr)", maxWidth: 760, marginBottom: "var(--s4)" }}>
            <TextField label="Display name" defaultValue="quasar-node-1" />
            <SelectField label="GPU tier">
              <option>1080p · 60 fps</option>
              <option>720p · 30 fps</option>
            </SelectField>
            <TextField label="VRAM (MB)" defaultValue="512" mono />
          </div>
          <SpecRow>
            <SearchInput placeholder="Search games…" />
            <Switch checked={switchOn} onChange={setSwitchOn} label="GPU passthrough" />
            <Checkbox checked={checked} onChange={setChecked} label="Capture input on launch" />
          </SpecRow>
        </div>
      </section>

      {/* ── Table ── */}
      <section className="sg-block" id="sg-table">
        <h2>Table</h2>
        <p className="sg-desc">Row height and header follow the density token; the last column is action buttons.</p>
        <div className="sg-specimen" style={{ padding: 0 }}>
          <Table columns={SESSION_COLS} rows={DEMO_SESSIONS} rowKey={(r) => r.id} />
        </div>
      </section>

      {/* ── Note ── */}
      <section className="sg-block" id="sg-note">
        <h2>Note</h2>
        <p className="sg-desc">An inset callout for context that is not an error, and its warning variant.</p>
        <div className="sg-specimen">
          <div className="note" style={{ marginBottom: "var(--s3)" }}>
            Publishing happens on the next scan, not immediately.
          </div>
          <div className="note warn">Scheduling is paused for this host while it drains.</div>
        </div>
      </section>

      {/* ── Bars, gauges and sparklines ── */}
      <section className="sg-block" id="sg-bars-gauges-and-sparklines">
        <h2>Bars, gauges and sparklines</h2>
        <p className="sg-desc">Capacity reads as a bar or a gauge; a trend reads as a sparkline.</p>
        <div className="sg-specimen">
          <SpecRow>
            <div style={{ width: 320, display: "flex", flexDirection: "column", gap: "var(--s3)" }}>
              <Bar percent={62} label="VRAM" value="318/512" variant="grad" />
              <Bar percent={50} label="Slots" value="1/2" variant="success" />
              <Bar percent={83} label="Encode" value="83%" variant="warning" />
            </div>
            <Gauge percent={72} label="GPU" color="var(--accent)" />
            <Gauge percent={41} label="VRAM" color="var(--success)" />
            <div style={{ width: 200 }}>
              <div className="eyebrow" style={{ marginBottom: "var(--s2)" }}>Fps, last 60s</div>
              <Sparkline points={SPARK_POINTS} color="var(--info)" height={34} />
            </div>
          </SpecRow>
        </div>
      </section>

      {/* ── Menu and drawer ── */}
      <section className="sg-block" id="sg-menu-and-drawer">
        <h2>Menu and drawer</h2>
        <p className="sg-desc">A kebab popover for row actions, and a slide-over for contextual detail.</p>
        <div className="sg-specimen">
          <SpecRow>
            <ActionsMenu
              label="Row actions"
              items={[
                { key: "open", label: "Open host", onClick: () => {} },
                { key: "console", label: "Local console", onClick: () => {} },
                { key: "sep", separator: true },
                { key: "drain", label: "Drain", onClick: () => {} },
                { key: "remove", label: "Remove host", onClick: () => {}, variant: "danger" },
              ]}
            />
            <Button onClick={() => setDrawerOpen(true)}>Open drawer</Button>
          </SpecRow>
        </div>
        <Drawer open={drawerOpen} onClose={() => setDrawerOpen(false)} title="Session detail">
          <p style={{ color: "var(--text-3)", fontSize: "var(--t-sm)" }}>
            Drawer body content goes here. Use it for contextual detail panels, filters or a
            secondary workflow without leaving the current page.
          </p>
        </Drawer>
      </section>
    </>
  );
}
