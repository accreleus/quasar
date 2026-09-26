/**
 * Developer apply (#360): the Releases rail card and the drawer that applies an
 * arbitrary digest set to one Quasar-owned machine (control-api.md §"Developer
 * apply"). Laid out to design_handoff_v3/screens/rh06 `devapply*.png`
 * (`rhDevApply` / `rhDevCard` in assets/pages-rh06.js).
 *
 * Owned GPU hosts are offered, and the control plane's own machine when it is owned.
 * There a control-plane image makes the request the control-plane target (with its
 * recovery actor first); the node agent alone is that host's own request. A control-plane
 * digest that migrates follows a migrating release's database rule (#364): the Database
 * section is drawn for every control-plane request, worded conditionally, because the
 * console cannot know before submitting whether a digest migrates.
 */

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type {
  ApplyComponentDigest,
  PlatformReleaseView,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Card } from "../../../components/Card";
import { Drawer } from "../../../components/Drawer";
import { SelectField } from "../../../components/TextField";
import { useAdminAction } from "../../../lib/resource/action";
import { developerRefusalText } from "./releasesCopy";

/** handoff: `.drawer` is `min(760px, 94vw)`, as the runtime-preset drawer. */
const DRAWER_WIDTH = 760;

type ComponentName = "recovery-actor" | "node-agent" | "control-plane";

interface ImageSlot {
  name: ComponentName;
  label: string;
  placeholder: string;
}

const SLOTS: ImageSlot[] = [
  { name: "recovery-actor", label: "Recovery actor", placeholder: "namespace/quasar-recovery@sha256:…" },
  { name: "node-agent", label: "Node agent", placeholder: "namespace/quasar-node-agent@sha256:…" },
  { name: "control-plane", label: "Control plane", placeholder: "namespace/quasar-control-plane@sha256:…" },
];

/** One machine the drawer offers, and the services it can take (control-api.md
 *  §"Developer apply"). */
export interface DeveloperMachine {
  key: string;
  label: string;
  /** The host a node-agent request names; null on a control-only machine. */
  hostId: string | null;
  nodeName: string;
  /** The control plane's own machine: its control plane can be applied here. */
  controlPlane: boolean;
  /** Its database, on the control plane's machine; null when not reported. */
  databaseMode?: "owned" | "external" | null;
}

/** Owned GPU hosts, and the control plane's own machine when it reports an owned
 *  install. A combined host is that machine, never a GPU host of its own. */
export function developerApplyMachines(view: PlatformReleaseView): DeveloperMachine[] {
  const cp = view.installed.control_plane;
  const own = cp.install_mode === "owned" ? (cp.machine_node_name ?? null) : null;
  const combined = own != null && cp.machine_role === "combined";
  const out: DeveloperMachine[] = [];
  if (own != null && (combined || cp.machine_role === "control_only")) {
    const agent = combined ? view.installed.hosts.find((h) => h.node_name === own) : undefined;
    out.push({
      key: "control-plane",
      label: `${own} · ${combined ? "Combined host" : "Control-only host"}`,
      hostId: agent?.host_id ?? null,
      nodeName: own,
      controlPlane: true,
      databaseMode: cp.database_mode ?? null,
    });
  }
  for (const h of view.installed.hosts) {
    if (h.install_mode !== "owned" || (combined && h.node_name === own)) continue;
    out.push({ key: h.host_id, label: `${h.node_name} · GPU host`, hostId: h.host_id, nodeName: h.node_name, controlPlane: false });
  }
  return out;
}

/** The services a machine offers; any other slot reads as not on this machine. */
function offers(m: DeveloperMachine | undefined, name: ComponentName): boolean {
  if (!m) return false;
  switch (name) {
    case "control-plane":
      return m.controlPlane;
    case "node-agent":
      return m.hostId != null;
    case "recovery-actor":
      return true;
  }
}

/** Why the filled images cannot be one request on this machine, or null. */
function combinationProblem(m: DeveloperMachine | undefined, names: ComponentName[]): string | null {
  if (!m?.controlPlane) return null;
  const cp = names.includes("control-plane");
  if (cp && names.includes("node-agent")) {
    return "Apply the control plane first, then its node agent on its own.";
  }
  if (!cp && names.includes("recovery-actor")) {
    return "On the control plane’s machine the recovery actor moves only with the control plane.";
  }
  return null;
}

const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
// A registry host (optionally with a port), then at least one lowercase path
// component: the namespace allowlist needs a namespace to match against.
const REPOSITORY_RE = /^[A-Za-z0-9.-]+(?::[0-9]+)?(?:\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+$/;

type Parsed = { ok: true; image: string; digest: string } | { ok: false; error: string };

/** `repository@sha256:<64 hex>`, split the way ApplyComponentDigest carries it
 *  (ADR 0001: the repository with no tag, the digest on its own). */
export function parseImageReference(raw: string): Parsed {
  const value = raw.trim();
  const at = value.indexOf("@");
  const repository = at < 0 ? value : value.slice(0, at);
  const lastComponent = repository.slice(repository.lastIndexOf("/") + 1);
  if (lastComponent.includes(":")) {
    return { ok: false, error: "Use a digest (@sha256:…), not a tag." };
  }
  const digest = at < 0 ? "" : value.slice(at + 1);
  if (!REPOSITORY_RE.test(repository) || !DIGEST_RE.test(digest)) {
    return { ok: false, error: "Use repository@sha256: followed by 64 lowercase hex characters." };
  }
  return { ok: true, image: repository, digest };
}

const COUNT_WORDS = ["", "", "two", "three"];

function footerHint(filled: number, invalid: number): string {
  if (invalid === 1) return "Fix the image above to continue.";
  if (invalid > 1) return `Fix the ${COUNT_WORDS[invalid] ?? invalid} images above to continue.`;
  if (filled === 0) return "Enter at least one image by digest.";
  return "Checks each digest at the registry before anything stops.";
}

interface Refusal {
  code: string;
  reason?: string;
  message: string;
}

export function DeveloperApplyCard({
  view,
  onApplied,
}: {
  view: PlatformReleaseView;
  onApplied: () => void;
}) {
  const [open, setOpen] = useState(false);
  const machines = developerApplyMachines(view);
  if (machines.length === 0) return null;

  return (
    <>
      <Card className="card-pad">
        <div className="eyebrow">Developer apply</div>
        <div className="hint devapply-card-hint">
          Apply a build that is not a release, by digest, from an allowed image namespace. Admins
          only; offered only on Quasar-owned machines.
        </div>
        <Button size="sm" className="mt4" onClick={() => setOpen(true)}>
          Developer apply…
        </Button>
      </Card>
      {open && (
        <DeveloperApplyDrawer
          machines={machines}
          onClose={() => setOpen(false)}
          onApplied={() => {
            setOpen(false);
            onApplied();
          }}
        />
      )}
    </>
  );
}

export function DeveloperApplyDrawer({
  machines,
  onClose,
  onApplied,
}: {
  machines: DeveloperMachine[];
  onClose: () => void;
  onApplied: () => void;
}) {
  const { token } = useAuth();
  const [machineKey, setMachineKey] = useState(machines[0]?.key ?? "");
  const [values, setValues] = useState<Record<ComponentName, string>>({
    "recovery-actor": "",
    "node-agent": "",
    "control-plane": "",
  });
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [backupConfirmed, setBackupConfirmed] = useState(false);

  const machine = machines.find((m) => m.key === machineKey);
  const offered = SLOTS.filter((s) => offers(machine, s.name));
  const parsed = offered
    .filter((s) => values[s.name].trim() !== "")
    .map((s) => ({ slot: s, result: parseImageReference(values[s.name]) }));
  const invalid = parsed.filter((p) => !p.result.ok).length;
  const components: ApplyComponentDigest[] = parsed.flatMap(({ slot, result }) =>
    result.ok ? [{ name: slot.name, image: result.image, digest: result.digest }] : [],
  );
  const problem = combinationProblem(
    machine,
    parsed.map((p) => p.slot.name),
  );
  const controlPlaneTarget = components.some((c) => c.name === "control-plane");
  const namesControlPlane = parsed.some((p) => p.slot.name === "control-plane");
  const external = machine?.databaseMode === "external";

  const apply = useAdminAction(
    async () =>
      adminApi.developerApply(
        token ?? "",
        controlPlaneTarget
          ? {
              target: "control_plane",
              components,
              force: false,
              // Not a gate: a digest that migrates unconfirmed fails backup_unconfirmed
              // before anything stops, and the console cannot know which digests migrate.
              ...(external ? { external_backup_confirmed: backupConfirmed } : {}),
            }
          : { target: "host", host_id: machine?.hostId ?? "", components, force: false },
      ),
    {
      // Accepted is not started: the attempt may wait for the host's sessions to end.
      success: `Developer apply queued for ${machine?.nodeName ?? "the machine"}.`,
      // The drawer's refusal note carries the explanation; the toast only
      // reports the event, so the server's message is not shown twice.
      failure: () => "The developer apply was not started.",
      onSuccess: onApplied,
      onFailure: (e) =>
        setRefusal(
          e instanceof ApiError
            ? { code: e.code, reason: e.reason, message: e.message }
            : { code: "internal", message: e instanceof Error ? e.message : String(e) },
        ),
    },
  );

  const busy = apply.pending != null;
  const canApply = !busy && machine != null && parsed.length > 0 && invalid === 0 && problem == null;

  const edit = (name: ComponentName, value: string) => {
    setValues((v) => ({ ...v, [name]: value }));
    setRefusal(null);
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title="Developer apply"
      eyebrow="developer lane · not a release"
      width={DRAWER_WIDTH}
      footer={
        <>
          <span className="hint">
            {problem ??
              (external && namesControlPlane && !backupConfirmed && invalid === 0
                ? "Without your confirmation, a build that changes the database is refused before anything stops."
                : footerHint(parsed.length, invalid))}
          </span>
          <span className="grow" />
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!canApply} onClick={() => void apply.run()}>
            Apply digests
          </Button>
        </>
      }
    >
      <div className="note warn mb5">
        <b>For testing a build that is not a release.</b> Applies images by digest to one
        Quasar-owned machine. It is recorded as an attempt like any other, the recovery actor moves
        first, and a failed agent is put back automatically. Applying a node agent ends that
        host&rsquo;s sessions.
      </div>
      {refusal && <RefusalNote refusal={refusal} />}

      <div className="fsec">
        <div className="fs-label">
          <h4>Target</h4>
          <p>One machine at a time. Machines built from source are not offered.</p>
        </div>
        <div className="fs-fields">
          <SelectField
            label="Machine"
            name="developer_apply_machine"
            value={machineKey}
            onChange={(e) => {
              setMachineKey(e.target.value);
              setRefusal(null);
            }}
          >
            {machines.map((m) => (
              <option key={m.key} value={m.key}>
                {m.label}
              </option>
            ))}
          </SelectField>
        </div>
      </div>

      <div className="fsec">
        <div className="fs-label">
          <h4>Images</h4>
          <p>Leave a service empty to keep what it runs.</p>
        </div>
        <div className="fs-fields">
          {SLOTS.map((slot) =>
            offers(machine, slot.name) ? (
              <ImageField
                key={slot.name}
                slot={slot}
                value={values[slot.name]}
                onChange={(v) => edit(slot.name, v)}
              />
            ) : (
              <ImageField key={slot.name} slot={slot} value="" absent="not on this machine" />
            ),
          )}
        </div>
      </div>

      {machine?.controlPlane && namesControlPlane && (
        <DatabaseSection
          machine={machine}
          confirmed={backupConfirmed}
          onConfirm={setBackupConfirmed}
        />
      )}

      <div className="fsec">
        <div className="fs-label">
          <h4>Allowed namespaces</h4>
          <p>Set on each machine. Images from anywhere else are refused.</p>
        </div>
        <div className="fs-fields">
          <p className="hint devapply-ns-note">
            Each machine&rsquo;s list is its{" "}
            <span className="mono">QUASAR_UPDATER_ALLOWED_NAMESPACES</span>. The control plane
            checks every image against its own copy before anything is sent.
          </p>
        </div>
      </div>
    </Drawer>
  );
}

/** rh06 devapply-migrating(-external).png, worded for a digest that may or may not
 *  migrate. The confirmation is sent, never required: the server decides. */
function DatabaseSection({
  machine,
  confirmed,
  onConfirm,
}: {
  machine: DeveloperMachine;
  confirmed: boolean;
  onConfirm: (confirmed: boolean) => void;
}) {
  const mode = machine.databaseMode ?? null;
  return (
    <div className="fsec">
      <div className="fs-label">
        <h4>Database</h4>
        <p>If this control-plane image changes the database, it follows a migrating release&rsquo;s rule.</p>
      </div>
      <div className="fs-fields">
        <p className="hint devapply-ns-note">
          If this build changes the database, then like a migrating release this apply waits for
          every session on the instance to end before the control plane is replaced, and is never
          applied unattended.
        </p>
        {mode === "external" ? (
          <>
            <div className="note warn">
              <b>Quasar does not back up your database.</b> It uses your database as it is and
              never dumps, restores or upgrades it. If this build changes the database, take a
              backup with your own tools first: it is the only way back if this build fails.
            </div>
            <label className="rowflex">
              <input
                type="checkbox"
                checked={confirmed}
                onChange={(e) => onConfirm(e.target.checked)}
              />
              <span>
                I have a current backup of this database, taken after the last change I want to
                keep.
              </span>
            </label>
          </>
        ) : mode === "owned" ? (
          <div className="note">
            <b>Quasar dumps its database first.</b> If this build changes the database, the
            recovery actor on {machine.nodeName} dumps Quasar&rsquo;s database before the control
            plane is replaced. If the dump cannot be taken, the apply stops there: the control
            plane is not replaced and the database is not touched.
          </div>
        ) : (
          <div className="note warn">
            <b>The database has not been reported yet.</b> The recovery actor on{" "}
            {machine.nodeName} has not said whether this is Quasar&rsquo;s own database or yours.
            If this build changes the database, Quasar dumps its own first, or needs your
            confirmation of a backup of yours.
          </div>
        )}
      </div>
    </div>
  );
}

function ImageField({
  slot,
  value,
  onChange,
  absent,
}: {
  slot: ImageSlot;
  value: string;
  onChange?: (value: string) => void;
  /** Why this service cannot be applied here (the disabled field's placeholder). */
  absent?: string;
}) {
  const id = `developer-apply-${slot.name}`;
  const result = !absent && value.trim() !== "" ? parseImageReference(value) : null;
  const error = result && !result.ok ? result.error : null;
  return (
    <div className="field">
      <label className="label" htmlFor={id}>
        {slot.label}
      </label>
      <input
        id={id}
        className={["input", "mono", error ? "input-error" : ""].filter(Boolean).join(" ")}
        value={value}
        placeholder={absent ?? slot.placeholder}
        disabled={absent !== undefined}
        spellCheck={false}
        autoComplete="off"
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-error` : undefined}
        onChange={(e) => onChange?.(e.target.value)}
      />
      {error && (
        <span className="apps-field-err" id={`${id}-error`}>
          {error}
        </span>
      )}
    </div>
  );
}

function RefusalNote({ refusal }: { refusal: Refusal }) {
  const { text, showMessage } = developerRefusalText(refusal.code, refusal.reason, refusal.message);
  const details = [`code: ${refusal.code}`];
  if (refusal.reason) details.push(`reason: ${refusal.reason}`);
  if (refusal.message) details.push(`message: ${refusal.message}`);
  return (
    <div className="note warn mb5" role="alert">
      <b>Not applied.</b> {text}
      {showMessage && refusal.message && <> {refusal.message}</>}
      <details className="enroll-more mt2">
        <summary>Details</summary>
        <p className="mono hint devapply-detail">
          {details.join("\n")}
        </p>
      </details>
    </div>
  );
}
