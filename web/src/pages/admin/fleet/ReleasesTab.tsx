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
 * The per-host apply half is #116 (ApplyControls.tsx); fleet apply and revert
 * are #117/#118.
 */

import { useState, type ReactNode } from "react";
import * as adminApi from "../../../api/admin";
import type {
  PlatformHostIdentity,
  PlatformRelease,
  PlatformReleaseTarget,
  PlatformReleaseView,
  ReleaseChannel,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Card } from "../../../components/Card";
import { Chip } from "../../../components/Chip";
import { CopyableCommand } from "../../../components/CopyableCommand";
import { Markdown } from "../../../components/Markdown";
import { ResourceStates } from "../../../components/ResourceStates";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { TextField } from "../../../components/TextField";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { relativeTime } from "../../../lib/format/relativeTime";
import { manualUpdatePath } from "../../../lib/platform/manualUpdate";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import {
  ApplyConfirmModal,
  ApplyHistory,
  AttemptProgress,
  attemptForTarget,
  useHostSessionCounts,
} from "./ApplyControls";
import {
  FailedAttemptPanel,
  RevertConfirmModal,
  releaseForDigest,
  useRevertStates,
} from "./RevertControls";
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
    // Only while something is in flight: an idle Releases page has nothing to
    // animate, and the read is not free.
    pollMs: (view) => (view.active_apply ? 3000 : undefined),
  });
  const view = res.data;
  // Bumped on every apply, so the history reloads without polling it too.
  const [applied, setApplied] = useState(0);

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
          <TargetsSection
            view={view}
            refreshKey={applied}
            onApplied={() => {
              setApplied((n) => n + 1);
              void res.refresh();
            }}
          />
          <Section
            title="Apply history"
            hint="Every update this instance has made to itself, newest first."
          >
            <ApplyHistory refreshKey={applied} />
          </Section>
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
      <div className="rowflex" style={{ alignItems: "center" }}>
        <SegmentedControl<ReleaseChannel>
          options={CHANNEL_OPTIONS}
          value={view.channel}
          activation="manual"
          aria-label="Release channel"
          disabled={save.pending != null}
          onChange={(value) => void save.run({ release_channel: value })}
        />
        {view.channel === "edge" && (
          <span className="muted">
            following <span className="mono">{view.edge_branch}</span>
          </span>
        )}
      </div>
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
  // An edge build has no version and no notes: the title IS the commit, and the
  // compare link stands in for the notes (control-api.md §Platform releases).
  const edge = release.channel === "edge";
  return (
    <li className="release-entry">
      <div className="rowflex">
        <b>{releaseLabel(release)}</b>
        {release.prerelease && <Chip variant="warning">Prerelease</Chip>}
        <span className="muted">
          {when(release.built_at)}
          {!edge && (
            <>
              {" · "}
              <span className="mono">{shortCommit(release.source_commit)}</span>
            </>
          )}{" "}
          · schema {release.schema_version}
        </span>
      </div>
      {edge ? (
        <p className="muted">
          {release.compare_url ? (
            <a href={release.compare_url} target="_blank" rel="noreferrer noopener">
              Compare with the installed build
            </a>
          ) : (
            "A branch build publishes no notes."
          )}
        </p>
      ) : release.notes ? (
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

/** The manual path for one ineligible target, under the targets table: what
 *  this target is on, what it could be on, and the commands for ITS situation
 *  (lib/platform/manualUpdate.ts, which is where the wording lives). */
function ManualPath({
  target,
  identity,
  release,
}: {
  target: PlatformReleaseTarget;
  identity: PlatformHostIdentity | undefined;
  release: PlatformRelease | undefined;
}) {
  const path = manualUpdatePath({
    reason: target.reason ?? null,
    kind: target.kind,
    installMode: identity?.install_mode ?? null,
    // The Releases view carries no GPU vendor, so the redeploy profile stays
    // the doc's own placeholder rather than a guess.
    release: release ?? null,
  });
  if (!path) return null;

  const name = target.kind === "control_plane" ? "Control plane" : (target.node_name ?? "Host");
  const installed = identity?.source_commit;

  return (
    <div className="note" data-testid={`manual-${target.host_id ?? "control-plane"}`}>
      <div className="rowflex">
        <b>{name}</b>
        <span className="muted">
          {installed ? (
            <>
              on <span className="mono">{shortCommit(installed)}</span>
            </>
          ) : (
            "build unknown"
          )}
          {release && <> · release {releaseLabel(release)}</>}
        </span>
      </div>
      <p className="hint">{path.summary}</p>
      {path.commands.map((c) => (
        <CopyableCommand key={c.label} label={c.label} text={c.command} />
      ))}
    </div>
  );
}

function TargetsSection({
  view,
  refreshKey,
  onApplied,
}: {
  view: PlatformReleaseView;
  refreshKey: number;
  onApplied: () => void;
}) {
  const [confirming, setConfirming] = useState<PlatformReleaseTarget | null>(null);
  const [reverting, setReverting] = useState<PlatformReleaseTarget | null>(null);
  const counts = useHostSessionCounts();
  // Derived from the history: a revert restores the digests of the host's last
  // succeeded attempt, and `targets` carries no signal for that.
  const reverts = useRevertStates(refreshKey);
  // Targets are evaluated against the newest listed release and nothing else.
  const newest = view.available[0];
  const attempts = view.active_apply?.attempts;

  const columns: TableColumn<PlatformReleaseTarget>[] = [
    {
      key: "target",
      header: "Target",
      render: (t) => (t.kind === "control_plane" ? "Control plane" : t.node_name),
    },
    {
      key: "state",
      header: "State",
      width: "220px",
      render: (t) => {
        const open = attemptForTarget(attempts, t);
        if (open) return <AttemptProgress attempt={open} />;
        return t.eligible ? (
          <Chip variant="success">Ready</Chip>
        ) : (
          <Chip variant="neutral">Not ready</Chip>
        );
      },
    },
    {
      key: "why",
      header: "Why",
      render: (t) => (attemptForTarget(attempts, t) ? "" : eligibilityText(t.reason ?? null)),
    },
    {
      key: "action",
      header: "",
      width: "170px",
      render: (t) => {
        // The control plane is #117's, and it is never revertible (ADR 0002).
        // An apply that has been sent cannot be forced or cancelled, so an open
        // attempt offers no second action.
        if (t.kind !== "host" || !t.host_id || attemptForTarget(attempts, t)) return null;
        const back = reverts.get(t.host_id);
        return (
          <>
            {t.eligible && newest && (
              <Button variant="ghost" onClick={() => setConfirming(t)}>
                Apply
              </Button>
            )}
            {back?.digest && (
              <Button variant="ghost" onClick={() => setReverting(t)}>
                Revert
              </Button>
            )}
          </>
        );
      },
    },
  ];
  return (
    <Section
      title="Targets"
      hint="Evaluated against the newest listed release. A host is cordoned while it updates and returns to service afterwards."
    >
      <Table
        columns={columns}
        rows={view.targets}
        rowKey={(t) => t.host_id ?? "control-plane"}
        empty="No targets."
      />
      {view.targets.map((t) => (
        <ManualPath
          key={t.host_id ?? "control-plane"}
          target={t}
          identity={view.installed.hosts.find((h) => h.host_id === t.host_id)}
          release={view.available[0]}
        />
      ))}
      {view.targets.map((t) => {
        const back = t.host_id ? reverts.get(t.host_id) : undefined;
        if (!back?.failed) return null;
        return (
          <FailedAttemptPanel
            key={`failed-${t.host_id}`}
            attempt={back.failed}
            state={back}
            onRevert={back.digest ? () => setReverting(t) : undefined}
          />
        );
      })}
      {reverting?.host_id && reverts.get(reverting.host_id) && (
        <RevertConfirmModal
          hostId={reverting.host_id}
          nodeName={reverting.node_name ?? "this host"}
          state={reverts.get(reverting.host_id)!}
          release={releaseForDigest(view.available, reverts.get(reverting.host_id)?.digest ?? "")}
          liveSessions={counts ? (counts.get(reverting.host_id) ?? 0) : null}
          onClose={() => setReverting(null)}
          onReverted={onApplied}
        />
      )}
      {confirming && newest && (
        <ApplyConfirmModal
          release={newest}
          target={confirming}
          liveSessions={counts ? (counts.get(confirming.host_id ?? "") ?? 0) : null}
          onClose={() => setConfirming(null)}
          onApplied={onApplied}
        />
      )}
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
