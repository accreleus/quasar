/**
 * `/admin/fleet/releases` — what this instance is running, what has been
 * published, and why each target can or cannot take it (control-api.md
 * §"Platform releases").
 *
 * Laid out to design_handoff_v3/screens/releases-v3.html: the update banner and
 * the release feed on the left, a rail of fact cards (installed, targets,
 * channel, apply history) on the right. The release body is parsed into rows by
 * lib/platform/releaseNotes.ts rather than dumped as markdown; the per-target
 * table, its manual recipes and its revert controls all survive behind the
 * targets card's "Per-host detail" disclosure.
 *
 * The per-host apply half is #116 (ApplyControls.tsx); fleet apply and revert
 * are #117/#118.
 */

import { useState, type ReactNode } from "react";
import * as adminApi from "../../../api/admin";
import type {
  JobsResponse,
  PlatformHostIdentity,
  PlatformRelease,
  PlatformReleaseTarget,
  PlatformReleaseView,
  ReleaseChannel,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Bar } from "../../../components/Bar";
import { Button } from "../../../components/Button";
import { Card } from "../../../components/Card";
import { Chip } from "../../../components/Chip";
import { CopyableCommand } from "../../../components/CopyableCommand";
import { Markdown } from "../../../components/Markdown";
import { ResourceStates } from "../../../components/ResourceStates";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { TextField } from "../../../components/TextField";
import { IconChevronDown, IconDownload, IconRefresh } from "../../../components/icons";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { relativeTime } from "../../../lib/format/relativeTime";
import { manualUpdatePath } from "../../../lib/platform/manualUpdate";
import {
  parseReleaseNotes,
  releaseCountsLine,
  type NoteEntry,
} from "../../../lib/platform/releaseNotes";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import {
  ApplyConfirmModal,
  ApplyHistory,
  AttemptProgress,
  attemptForTarget,
  useHostSessionCounts,
} from "./ApplyControls";
import { ControlPlaneRestarting, FleetApplyButton, FleetRunPanel } from "./FleetApply";
import {
  FailedAttemptPanel,
  RevertConfirmModal,
  releaseForDigest,
  useRevertStates,
} from "./RevertControls";
import {
  commitsMatch,
  eligibilityText,
  faultText,
  hasUpdate,
  releaseLabel,
  shortCommit,
} from "./releasesCopy";
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

/** An instant as the console prints a release's publication: "5 Sep 2026,
 *  14:59". UTC, because every timestamp on this page is a UTC instant and the
 *  next-check line beside it is a UTC cron. */
function stamp(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const date = new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
    timeZone: "UTC",
  }).format(d);
  const time = new Intl.DateTimeFormat("en-GB", {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone: "UTC",
  }).format(d);
  return `${date}, ${time}`;
}

/** "Mon 02:00 UTC" — the detection job's next scheduled run. */
function nextCheckLabel(iso: string): string | null {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  const day = new Intl.DateTimeFormat("en-GB", { weekday: "short", timeZone: "UTC" }).format(d);
  const time = new Intl.DateTimeFormat("en-GB", {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone: "UTC",
  }).format(d);
  return `${day} ${time} UTC`;
}

/** The head's "next check" fragment, or null. Read from the jobs list because
 *  the release view carries no schedule; a failed or empty read simply omits
 *  the fragment rather than inventing a cadence. */
function useNextCheck(): string | null {
  const res = useResource<JobsResponse>({
    label: "jobs",
    fetch: ({ token, signal }) => adminApi.listJobs(token, signal),
  });
  const job = res.data?.items?.find((j) => j.id === DETECT_JOB_ID);
  if (!job?.enabled || !job.next_run_at) return null;
  return nextCheckLabel(job.next_run_at);
}

function gh(repo: string, path: string): string | null {
  return repo ? `https://github.com/${repo}/${path}` : null;
}

/** A rail card: an eyebrow label over its facts. */
function RailCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card className="card-pad">
      <div className="eyebrow">{title}</div>
      <div className="mt3">{children}</div>
    </Card>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="rel-fact">
      <span>{label}</span>
      <span>{children}</span>
    </div>
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
  const nextCheck = useNextCheck();
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
    sub: view
      ? `${view.channel} channel · last checked ${when(view.checked_at)}` +
        (nextCheck ? ` · next check ${nextCheck}` : "")
      : undefined,
    actions: (
      <>
        <Button variant="ghost" onClick={() => void check.run()} disabled={check.pending != null}>
          <IconRefresh /> Check now
        </Button>
        {view && (
          <FleetApplyButton view={view} onStarted={() => void res.refresh()}>
            <IconDownload /> Update Quasar
          </FleetApplyButton>
        )}
      </>
    ),
  });

  const run = view?.active_apply?.run ?? null;
  const cpTarget = view?.targets.find((t) => t.kind === "control_plane");
  const cpAttempt = cpTarget ? attemptForTarget(view?.active_apply?.attempts, cpTarget) : undefined;
  // A run's first target restarts the process serving this page, so a load
  // error while the run is active is the update working, not a fault to report.
  const restarting =
    run != null &&
    (cpAttempt?.state === "recreating" ||
      cpAttempt?.state === "pending" ||
      res.errorMessage != null);

  return (
    <>
      <ResourceStates loading={res.loading} error={run ? null : res.errorMessage} />
      {restarting && <ControlPlaneRestarting />}
      {view && (
        <>
          {run ? (
            <Card className="card-pad mb4">
              <div className="eyebrow">Fleet update</div>
              <div className="mt3">
                <FleetRunPanel
                  run={run}
                  targets={view.targets}
                  onChanged={() => void res.refresh()}
                />
              </div>
            </Card>
          ) : (
            <UpdateBanner view={view} />
          )}
          <div className="split rel-split">
            <div>
              <ReleaseFeed view={view} />
            </div>
            <div className="rel-rail">
              <InstalledCard view={view} />
              <TargetsCard
                view={view}
                refreshKey={applied}
                onApplied={() => {
                  setApplied((n) => n + 1);
                  void res.refresh();
                }}
              />
              <ChannelCard view={view} onSaved={() => void res.refresh()} />
              <RailCard title="Apply history">
                <ApplyHistory refreshKey={applied} />
              </RailCard>
              <FaultsCard view={view} />
            </div>
          </div>
        </>
      )}
    </>
  );
}

/** The banner above the split: the version step this instance can take, or the
 *  note that there is none. */
function UpdateBanner({ view }: { view: PlatformReleaseView }) {
  const newest = view.available[0];
  // With nothing listed there is nothing to be up to date WITH; the feed's own
  // empty card is the whole answer.
  if (!newest) return null;
  if (!hasUpdate(view)) {
    return (
      <p className="note mb4">
        <strong>Up to date.</strong> {prefixed(releaseLabel(newest))} is the newest release on the{" "}
        {view.channel} channel.
      </p>
    );
  }

  const installed = view.installed.control_plane;
  const from = installed.version || shortCommit(installed.source_commit);
  const behind = view.available.filter((r) => !commitsMatch(installed.source_commit, r.source_commit))
    .length;
  const counts = releaseCountsLine(parseReleaseNotes(newest.notes, view.source_repo ?? "").counts);

  return (
    <Card className="card-pad mb4 rel-update">
      <div className="rel-update-main">
        <div className="eyebrow">Update available</div>
        <div className="rowflex mt3" style={{ alignItems: "baseline" }}>
          <span className="rel-version">
            {prefixed(from)} <span className="rel-arrow">→</span> {prefixed(releaseLabel(newest))}
          </span>
          {newest.prerelease && <Chip variant="warning">Pre-release</Chip>}
        </div>
        <div className="hint mt2">
          Published {stamp(newest.built_at)}
          {counts && ` · ${counts}`} · {behind} release{behind === 1 ? "" : "s"} behind
        </div>
      </div>
      <div className="hint rel-update-why">
        Updating moves the control plane first, then each eligible host in sequence, stopping at
        the first failure. Hosts are cordoned during their apply and wait for zero sessions.
      </div>
    </Card>
  );
}

/** A version reads as "v0.2.0"; a bare commit (edge) does not take the v. */
function prefixed(label: string): string {
  return /^\d/.test(label) ? `v${label}` : label;
}

function ReleaseFeed({ view }: { view: PlatformReleaseView }) {
  const repo = view.source_repo ?? "";
  const installedCommit = view.installed.control_plane.source_commit;
  const releasesUrl = gh(repo, "releases");

  return (
    <>
      {view.available.length === 0 ? (
        <Card className="card-pad mb4">
          <p className="muted">
            Nothing at or above this control plane's schema has been detected on the {view.channel}{" "}
            channel.
          </p>
        </Card>
      ) : (
        view.available.map((release, i) => (
          <ReleaseCard
            key={release.id}
            release={release}
            repo={repo}
            latest={i === 0}
            installed={commitsMatch(installedCommit, release.source_commit)}
          />
        ))
      )}
      <p className="hint" style={{ padding: "2px 4px" }}>
        Notes are sanitised from{" "}
        {releasesUrl ? (
          <a href={releasesUrl} target="_blank" rel="noreferrer noopener">
            GitHub Releases
          </a>
        ) : (
          "GitHub Releases"
        )}
        {repo && ` on ${repo}`}. The updater follows the version tag; images are applied by digest.
      </p>
    </>
  );
}

function ReleaseCard({
  release,
  repo,
  latest,
  installed,
}: {
  release: PlatformRelease;
  repo: string;
  latest: boolean;
  installed: boolean;
}) {
  // An edge build has no version and no notes: the title IS the commit, and the
  // compare link stands in for the notes (control-api.md §Platform releases).
  const edge = release.channel === "edge";
  const { entries, counts } = parseReleaseNotes(release.notes, repo);
  const countsLine = releaseCountsLine(counts);
  const commitUrl = gh(repo, `commit/${release.source_commit}`);
  const releaseUrl = release.version ? gh(repo, `releases/tag/v${release.version}`) : null;

  return (
    <Card className="mb4">
      <div className="panel-head" style={{ alignItems: "flex-start" }}>
        <div>
          <div className="rowflex" style={{ alignItems: "center" }}>
            <span className="rel-card-title">{prefixed(releaseLabel(release))}</span>
            {latest && <Chip variant="accent">Latest</Chip>}
            {installed && (
              <Chip variant="success" dot>
                Installed
              </Chip>
            )}
            {release.prerelease && <Chip variant="warning">Pre-release</Chip>}
          </div>
          <div className="hint mt2">
            {stamp(release.built_at)}
            {!edge && (
              <>
                {" · "}
                {commitUrl ? (
                  <a className="mono" href={commitUrl} target="_blank" rel="noreferrer noopener">
                    {shortCommit(release.source_commit)}
                  </a>
                ) : (
                  <span className="mono">{shortCommit(release.source_commit)}</span>
                )}
              </>
            )}
            {countsLine && ` · ${countsLine}`} · schema {release.schema_version}
          </div>
        </div>
        <div className="acts">
          {releaseUrl && (
            <a
              className="btn btn-sm btn-ghost"
              href={releaseUrl}
              target="_blank"
              rel="noreferrer noopener"
            >
              View on GitHub
            </a>
          )}
        </div>
      </div>
      {entries.length > 0 ? (
        entries.map((entry, i) => <NoteRow key={`${entry.tag}-${i}`} entry={entry} />)
      ) : (
        <div className="card-pad">
          <NoNotes release={release} edge={edge} />
        </div>
      )}
    </Card>
  );
}

/** One changelog bullet: tag, title, issue chips, and the detail behind a
 *  disclosure. */
function NoteRow({ entry }: { entry: NoteEntry }) {
  return (
    <details className="rel-e">
      <summary>
        <span className={`rel-c ${entry.tone}`}>{entry.tag}</span>
        <span className="rel-t">{entry.title}</span>
        {entry.issues.map((issue) =>
          issue.url ? (
            <a
              key={issue.number}
              className="rel-ref"
              href={issue.url}
              target="_blank"
              rel="noreferrer noopener"
              onClick={(e) => e.stopPropagation()}
            >
              #{issue.number}
            </a>
          ) : (
            <span key={issue.number} className="rel-ref muted">
              #{issue.number}
            </span>
          ),
        )}
        <span className="rel-x" aria-hidden="true">
          <IconChevronDown />
        </span>
      </summary>
      {entry.detail && (
        <div className="rel-b">
          <Markdown>{entry.detail}</Markdown>
        </div>
      )}
    </details>
  );
}

/** An edge build, or a stable release the workflow published with an empty
 *  body: the compare link stands in for the notes. */
function NoNotes({ release, edge }: { release: PlatformRelease; edge: boolean }) {
  if (release.compare_url) {
    return (
      <p className="muted">
        <a href={release.compare_url} target="_blank" rel="noreferrer noopener">
          {edge ? "Compare with the installed build" : "See the changes"}
        </a>
      </p>
    );
  }
  return (
    <p className="muted">
      {edge ? "A branch build publishes no notes." : "No notes were published with this release."}
    </p>
  );
}

function InstalledCard({ view }: { view: PlatformReleaseView }) {
  const cp = view.installed.control_plane;
  const hosts = view.installed.hosts;
  const repo = view.source_repo ?? "";
  const commitUrl = gh(repo, `commit/${cp.source_commit}`);
  const versions = new Set(hosts.map((h) => h.agent_version).filter(Boolean));
  const agents =
    hosts.length === 0
      ? "none registered"
      : `${hosts.length} host${hosts.length === 1 ? "" : "s"} · ${
          versions.size === 1 ? prefixed([...versions][0] as string) : "mixed"
        }`;

  return (
    <RailCard title="Installed">
      <Fact label="Control plane">
        <span className="num">{prefixed(cp.version)}</span>
      </Fact>
      <Fact label="Commit">
        {commitUrl ? (
          <a className="mono" href={commitUrl} target="_blank" rel="noreferrer noopener">
            {shortCommit(cp.source_commit)}
          </a>
        ) : (
          <span className="mono">{shortCommit(cp.source_commit)}</span>
        )}
      </Fact>
      <Fact label="Schema">
        <span className="num">{cp.schema_version}</span>
      </Fact>
      <Fact label="Node agents">{agents}</Fact>
      {view.last_error && (
        <p className="form-error mt3" role="alert">
          Last release check failed: {view.last_error}
        </p>
      )}
    </RailCard>
  );
}

function ChannelCard({ view, onSaved }: { view: PlatformReleaseView; onSaved: () => void }) {
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
    <RailCard title="Channel">
      <SegmentedControl<ReleaseChannel>
        options={CHANNEL_OPTIONS}
        value={view.channel}
        activation="manual"
        aria-label="Release channel"
        disabled={save.pending != null}
        onChange={(value) => void save.run({ release_channel: value })}
      />
      <p className="hint mt2">
        Stable follows tagged releases with notes; edge follows a branch. Switching changes what is
        listed, never what is installed, and never starts a check.
      </p>
      <div className="mt3" style={{ opacity: view.channel === "edge" ? 1 : 0.6 }}>
        <TextField
          label="Edge branch"
          name="release_edge_branch"
          value={branch}
          mono
          onChange={(e) => setDraft(e.target.value)}
        />
        <Button
          variant="ghost"
          className="mt2"
          disabled={save.pending != null || branch === view.edge_branch || branch === ""}
          onClick={() => void save.run({ release_edge_branch: branch })}
        >
          Save branch
        </Button>
      </div>
    </RailCard>
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

function TargetsCard({
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

  const cp = view.targets.find((t) => t.kind === "control_plane");
  const hostTargets = view.targets.filter((t) => t.kind === "host");
  const ready = hostTargets.filter((t) => t.eligible).length;
  const holdouts = hostTargets.filter((t) => !t.eligible).slice(0, 3);
  const moreHoldouts = hostTargets.length - ready - holdouts.length;

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
    <RailCard title="Targets">
      <p className="hint">
        {newest
          ? `Evaluated against ${prefixed(releaseLabel(newest))}. `
          : "Nothing is listed to evaluate against. "}
        Control plane goes first; hosts follow in sequence, cordoned during their apply.
      </p>
      <div className="mt3">
        <Fact label="Control plane">
          {cp ? (
            cp.eligible ? (
              <Chip variant="success" dot>
                Ready
              </Chip>
            ) : (
              <Chip variant="neutral">Not ready</Chip>
            )
          ) : (
            <span className="muted">—</span>
          )}
        </Fact>
        <Fact label="Node agents">
          <span className="rowflex" style={{ alignItems: "center" }}>
            <span className="num">
              {ready}/{hostTargets.length}
            </span>
            <Bar
              percent={hostTargets.length ? (ready / hostTargets.length) * 100 : 0}
              variant={ready === hostTargets.length ? "success" : "warning"}
            />
          </span>
        </Fact>
      </div>
      {holdouts.length > 0 && (
        <div className="mt2">
          {holdouts.map((t) => (
            <div className="rel-holdout" key={t.host_id ?? "cp"}>
              <span>{t.node_name}</span>
              <span className="hint">{eligibilityText(t.reason ?? null)}</span>
            </div>
          ))}
          {moreHoldouts > 0 && <div className="hint">+{moreHoldouts} more not ready</div>}
        </div>
      )}

      <details className="rel-detail mt3">
        <summary>Per-host detail</summary>
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
      </details>

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
    </RailCard>
  );
}

function FaultsCard({ view }: { view: PlatformReleaseView }) {
  if (view.faults.length === 0) return null;
  return (
    <RailCard title="Needs attention">
      <ul className="release-faults">
        {view.faults.map((fault, i) => (
          <li key={`${fault.kind}-${fault.host_id ?? "instance"}-${i}`}>
            <Chip variant="warning">{faultText(fault.kind)}</Chip>{" "}
            {fault.node_name && <b>{fault.node_name}: </b>}
            {fault.detail}
          </li>
        ))}
      </ul>
    </RailCard>
  );
}
