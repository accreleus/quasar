/**
 * The Releases banner after a migrating control-plane update went wrong (#364): the
 * refusal when the pre-update dump could not be taken, and the restore card when the new
 * control plane failed after its migration. Laid out to design_handoff_v3/screens/rh06
 * `update-refused.png` and `restore-own|unknown|external.png` (`rhRefusedBanner`,
 * `rhRestore` in assets/pages-rh06.js). Wire identifiers appear only under the closed
 * Details disclosure.
 */

import type { PlatformApplyAttempt, PlatformRelease, PlatformReleaseView } from "../../../api/types";
import { Card } from "../../../components/Card";
import { dumpTakenAt, machineName, type FailedMigration } from "./migratingUpdate";
import { prefixed, releaseLabel, shortDigest, stamp } from "./releasesCopy";
import { Snippet } from "./Snippet";

/** The closed diagnostic disclosure (the handoff's `.diag`). */
function Diag({ label, lines }: { label: string; lines: string[] }) {
  return (
    <details className="enroll-more mt3">
      <summary>{label}</summary>
      <div className="enroll-snippet mt2">
        <pre className="mono">{lines.join("\n")}</pre>
      </div>
    </details>
  );
}

function releaseName(release: PlatformRelease | undefined, fallback: string): string {
  return release ? prefixed(releaseLabel(release)) : fallback;
}

function componentLines(a: PlatformApplyAttempt): string {
  return a.requested_digests
    .map((c) => {
      const from = a.previous_digests.find((p) => p.name === c.name)?.digest ?? null;
      return `${c.name} ${shortDigest(from)} → ${shortDigest(c.digest)}`;
    })
    .join(", ");
}

export function RefusedBanner({
  view,
  refused,
}: {
  view: PlatformReleaseView;
  refused: { attempt: PlatformApplyAttempt; release: PlatformRelease | undefined; actorMoved: boolean };
}) {
  const { attempt, release, actorMoved } = refused;
  const installed = view.installed.control_plane;
  const machine = machineName(view);
  const to = releaseName(release, "the new release");
  const details = [
    `attempt: ${attempt.id}`,
    `target: control_plane · outcome: failed`,
    `components: ${componentLines(attempt)}`,
    `reason: ${attempt.reason ?? ""}`,
    `schema: ${installed.schema_version} (unchanged)`,
  ];
  if (attempt.output.trim() !== "") details.push("output:", attempt.output.trimEnd());

  return (
    <Card className="card-pad mb4" data-testid="update-refused">
      <div className="eyebrow rel-alert-eyebrow warning">Update refused</div>
      <div className="rel-card-title mt2">
        {prefixed(installed.version)} → {to} stopped before the control plane moved
      </div>
      <p className="hint rel-refused-body">
        Quasar could not dump its database on {machine}. The control plane was not replaced, the
        database was not touched and no host was updated; sessions can start again.
        {actorMoved && (
          <>
            {" "}
            The recovery actor on {machine} is already on {to}: it always moves first, and running
            one release ahead of the control plane on its own machine is expected and harmless.
          </>
        )}{" "}
        If the disk there is full, free some space on that machine, then update again.
      </p>
      <Diag label="Details" lines={details} />
    </Card>
  );
}

export function RestoreCard({ view, failed }: { view: PlatformReleaseView; failed: FailedMigration }) {
  const { attempt, variant, command, release, previous } = failed;
  const machine = machineName(view);
  const to = releaseName(release, "This build");
  const back = releaseName(previous, "the build it replaced");
  const what = release ? "the release’s" : "the build’s";
  const schema =
    previous != null
      ? `schema: ${previous.schema_version} → ${view.installed.control_plane.schema_version}`
      : `schema: ${view.installed.control_plane.schema_version}`;

  let body;
  if (variant === "own") {
    const taken = dumpTakenAt(attempt.pre_update_dump);
    body = (
      <>
        <p className="rel-alert-body">
          Quasar does not undo a migrating update on its own, because the new control plane may
          already have written data. To go back to <b>{back}</b>, run this on {machine}. It stops
          the control plane, loads the dump{" "}
          {taken ? (
            <>
              taken at {stamp(taken)} &mdash; before the migration, under {back} &mdash;
            </>
          ) : (
            "taken before the migration"
          )}{" "}
          into Quasar&rsquo;s database, and starts {back} again.{" "}
          {taken
            ? `Anything written after ${stamp(taken)} is lost.`
            : "Anything written since that dump was taken is lost."}
        </p>
        <Snippet
          caption={`Run on ${machine} as root`}
          text={command ?? ""}
          testId="restore-command"
          label="Copy restore command"
        />
        <p className="hint rel-restore-hint">
          The recovery actor on that machine prints the same command in its output, so it can be
          run while this page is unreachable. The last three dumps are kept.
        </p>
        <Diag
          label="Attempt details"
          lines={[
            `attempt: ${attempt.id} · control plane · outcome: failed, not restored`,
            `reason: ${attempt.reason ?? ""}`,
            `dump: ${attempt.pre_update_dump ?? ""}`,
            schema,
          ]}
        />
      </>
    );
  } else if (variant === "unknown") {
    body = (
      <>
        <p className="rel-alert-body">
          Quasar does not undo a migrating update on its own. The recovery actor on {machine} has
          not reported which dump it took, so the restore command cannot be shown here yet. The
          same command is also printed in the recovery actor&rsquo;s log on that machine.
        </p>
        <p className="hint rel-restore-hint">This card fills in when the recovery actor answers.</p>
      </>
    );
  } else {
    body = (
      <>
        <p className="rel-alert-body">
          Quasar holds no dump of your database, and does not undo a migrating update on its own.
          To go back to <b>{back}</b>: make sure the control plane on {machine} is stopped (the
          recovery actor stops it when the update fails; if it runs, <code>docker stop
          quasar-control-plane</code>), restore the backup you confirmed into your database with
          your own tools, then run this on {machine}. It starts {back} only if the
          database&rsquo;s schema matches that release, and refuses otherwise.
        </p>
        {command ? (
          <Snippet
            caption={`Run on ${machine} as root, after stopping the control plane and restoring your backup`}
            text={command}
            testId="restore-command"
            label="Copy restore command"
          />
        ) : (
          <p className="hint rel-restore-hint">
            The recovery actor on {machine} has not reported the command yet; it also prints it in
            its log on that machine.
          </p>
        )}
        <Diag
          label="Attempt details"
          lines={[
            `attempt: ${attempt.id} · control plane · outcome: failed, not restored`,
            `reason: ${attempt.reason ?? ""}`,
            "database: external",
            schema,
          ]}
        />
      </>
    );
  }

  return (
    <Card className="card-pad mb4" data-testid={`restore-${variant}`}>
      <div className="eyebrow rel-alert-eyebrow danger">Update failed</div>
      <div className="rel-card-title mt2">{to} failed after changing the database</div>
      <div className="hint mt2">
        Control plane on {machine} · {stamp(attempt.finished_at ?? attempt.created_at)} ·{" "}
        {what} database migration had already run
      </div>
      {body}
    </Card>
  );
}
