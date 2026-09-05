/**
 * `/admin/fleet/releases` — what this instance is running, what has been
 * published, and why each target can or cannot take it (control-api.md
 * §"Platform releases").
 *
 * A Fleet tab with no v3 mock, for the reason Jobs is one: it is fleet
 * operations, and it reuses the Fleet head rather than earning a rail row
 * (sectionTabs.ts). Everything composes existing card/table/chip primitives;
 * the only new rule is `.release-*` in styles/admin/fleet.css.
 *
 * There are no Apply buttons: the apply half is amendment 2 (#116/#117/#118).
 * This page reads, and says what could move.
 */

import { useState, type ReactNode } from "react";
import * as adminApi from "../../../api/admin";
import type {
  PlatformRelease,
  PlatformReleaseTarget,
  PlatformReleaseView,
  ReleaseChannel,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Card } from "../../../components/Card";
import { Chip } from "../../../components/Chip";
import { Markdown } from "../../../components/Markdown";
import { ResourceStates } from "../../../components/ResourceStates";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { TextField } from "../../../components/TextField";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { relativeTime } from "../../../lib/format/relativeTime";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { eligibilityText, faultText, hasUpdate, releaseLabel, shortCommit } from "./releasesCopy";
import "../../../styles/admin/fleet.css";

/** The detection job's id, and what "Check now" runs. Server twin:
 *  internal/platform.DetectJobID. */
const DETECT_JOB_ID = "platform.release_detect";

const CHANNEL_OPTIONS: { value: ReleaseChannel; label: string }[] = [
  { value: "stable", label: "Stable" },
  { value: "edge", label: "Edge" },
];

function when(iso: string | null | undefined): string {
  return iso ? relativeTime(iso) : "—";
}

function Section({ title, hint, children }: { title: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <Card className="mb6">
      <div className="panel-head">
        <div>
          <span className="panel-title">{title}</span>
          {hint && (
            <div className="hint" style={{ marginTop: 3 }}>
              {hint}
            </div>
          )}
        </div>
      </div>
      <div className="card-pad">{children}</div>
    </Card>
  );
}

export function ReleasesTab() {
  const { token } = useAuth();
  const res = useResource<PlatformReleaseView>({
    label: "platform releases",
    fetch: ({ token: t, signal }) => adminApi.getPlatformReleases(t, signal),
  });
  const view = res.data;

  // "Check now" is the jobs run-now action: this page's read never triggers
  // detection (control-api.md).
  const check = useAdminAction(async () => adminApi.runJobNow(token ?? "", DETECT_JOB_ID), {
    success: "Checking for new releases…",
    failure: "Could not start the release check.",
    onSuccess: () => void res.refresh(),
  });

  useSectionHead({
    sub: view ? `${view.channel} channel · last checked ${when(view.checked_at)}` : undefined,
    actions: (
      <>
        <Button variant="ghost" onClick={() => void res.refresh()}>
          Refresh
        </Button>
        <Button onClick={() => void check.run()} disabled={check.pending != null}>
          Check now
        </Button>
      </>
    ),
  });

  return (
    <>
      <ResourceStates loading={res.loading} error={res.errorMessage} />
      {view && (
        <>
          <InstalledSection view={view} />
          <ChannelSection view={view} onSaved={() => void res.refresh()} />
          <AvailableSection view={view} />
          <TargetsSection view={view} />
          <FaultsSection view={view} />
        </>
      )}
    </>
  );
}

function InstalledSection({ view }: { view: PlatformReleaseView }) {
  const cp = view.installed.control_plane;
  const hosts = view.installed.hosts;
  const unknown = hosts.filter((h) => !h.identity_known).length;
  const offline = hosts.filter((h) => h.status === "offline").length;

  return (
    <Section title="Installed">
      <p>
        <b>Control plane</b> {cp.version} · <span className="mono">{shortCommit(cp.source_commit)}</span>{" "}
        · schema {cp.schema_version} · built {when(cp.built_at)}
      </p>
      <p>
        <b>Node agents</b> {hosts.length} host{hosts.length === 1 ? "" : "s"}
        {unknown > 0 && ` · ${unknown} not reporting an identity`}
        {offline > 0 && ` · ${offline} offline`}
      </p>
      {view.available.length > 0 && !hasUpdate(view) && (
        <p className="muted">This instance is on the newest release for its channel.</p>
      )}
      {view.last_error && (
        <p className="form-error" role="alert">
          Last release check failed: {view.last_error}
        </p>
      )}
    </Section>
  );
}

function ChannelSection({ view, onSaved }: { view: PlatformReleaseView; onSaved: () => void }) {
  const { token } = useAuth();
  // A draft, so a half-typed branch is never a save.
  const [draft, setDraft] = useState<string | null>(null);
  const branch = draft ?? view.edge_branch;

  const save = useAdminAction(
    async (patch: { release_channel?: ReleaseChannel; release_edge_branch?: string }) =>
      adminApi.updateSettings(token ?? "", patch),
    {
      success: "Release settings saved.",
      failure: "Could not save the release settings.",
      onSuccess: () => {
        setDraft(null);
        onSaved();
      },
    },
  );

  return (
    <Section
      title="Channel"
      hint="Stable follows tagged releases with notes; edge follows a branch. Switching changes what is listed, never what is installed, and never starts a check."
    >
      <SegmentedControl<ReleaseChannel>
        options={CHANNEL_OPTIONS}
        value={view.channel}
        activation="manual"
        aria-label="Release channel"
        disabled={save.pending != null}
        onChange={(value) => void save.run({ release_channel: value })}
      />
      <div className="rowflex mt4" style={{ alignItems: "flex-end" }}>
        <TextField
          label="Edge branch"
          name="release_edge_branch"
          value={branch}
          mono
          onChange={(e) => setDraft(e.target.value)}
          hint="Selects nothing while the channel is stable, and survives a switch."
        />
        <Button
          variant="ghost"
          disabled={save.pending != null || branch === view.edge_branch || branch === ""}
          onClick={() => void save.run({ release_edge_branch: branch })}
        >
          Save branch
        </Button>
      </div>
    </Section>
  );
}

function AvailableSection({ view }: { view: PlatformReleaseView }) {
  return (
    <Section title="Available">
      {view.available.length === 0 ? (
        <p className="muted">
          Nothing at or above this control plane's schema has been detected on the{" "}
          {view.channel} channel.
        </p>
      ) : (
        <ol className="release-list">
          {view.available.map((release) => (
            <ReleaseEntry key={release.id} release={release} />
          ))}
        </ol>
      )}
    </Section>
  );
}

function ReleaseEntry({ release }: { release: PlatformRelease }) {
  return (
    <li className="release-entry">
      <div className="rowflex">
        <b>{releaseLabel(release)}</b>
        {release.prerelease && <Chip variant="warning">Prerelease</Chip>}
        <span className="muted">
          {when(release.built_at)} ·{" "}
          <span className="mono">{shortCommit(release.source_commit)}</span> · schema{" "}
          {release.schema_version}
        </span>
      </div>
      {release.notes ? (
        <Markdown>{release.notes}</Markdown>
      ) : (
        <p className="muted">
          No notes were published with this release
          {release.compare_url && (
            <>
              {" — "}
              <a href={release.compare_url} target="_blank" rel="noreferrer noopener">
                see the changes
              </a>
            </>
          )}
          .
        </p>
      )}
    </li>
  );
}

function TargetsSection({ view }: { view: PlatformReleaseView }) {
  const columns: TableColumn<PlatformReleaseTarget>[] = [
    {
      key: "target",
      header: "Target",
      render: (t) => (t.kind === "control_plane" ? "Control plane" : t.node_name),
    },
    {
      key: "state",
      header: "State",
      width: "120px",
      render: (t) =>
        t.eligible ? <Chip variant="success">Ready</Chip> : <Chip variant="neutral">Not ready</Chip>,
    },
    { key: "why", header: "Why", render: (t) => eligibilityText(t.reason ?? null) },
  ];
  return (
    <Section
      title="Targets"
      hint="Evaluated against the newest listed release. Applying an update from here arrives with the apply half; this reports what could move."
    >
      <Table
        columns={columns}
        rows={view.targets}
        rowKey={(t) => t.host_id ?? "control-plane"}
        empty="No targets."
      />
    </Section>
  );
}

function FaultsSection({ view }: { view: PlatformReleaseView }) {
  if (view.faults.length === 0) return null;
  return (
    <Section title="Needs attention">
      <ul className="release-faults">
        {view.faults.map((fault, i) => (
          <li key={`${fault.kind}-${fault.host_id ?? "instance"}-${i}`}>
            <Chip variant="warning">{faultText(fault.kind)}</Chip>{" "}
            {fault.node_name && <b>{fault.node_name}: </b>}
            {fault.detail}
          </li>
        ))}
      </ul>
    </Section>
  );
}
