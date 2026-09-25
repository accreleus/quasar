/**
 * Developer apply (#360): the Releases rail card and the drawer that applies an
 * arbitrary digest set to one Quasar-owned machine (control-api.md §"Developer
 * apply"). Laid out to design_handoff_v3/screens/rh06 `devapply*.png`
 * (`rhDevApply` / `rhDevCard` in assets/pages-rh06.js).
 *
 * Only owned GPU hosts are offered. The control plane's own machine is not: its
 * target can take a migrating control-plane digest, and the drawer does not yet
 * carry the mock's Database section (drain, dump or external-backup
 * confirmation) that such an apply needs.
 */

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type {
  ApplyComponentDigest,
  PlatformHostIdentity,
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

/** What a GPU host's recovery actor can replace in this build; `control-plane` is never
 *  sent to a host. */
const HOST_COMPONENTS: ReadonlySet<ComponentName> = new Set(["node-agent"]);

/** Offered to a host by the contract, but the actor refuses it `invalid` until it can
 *  replace itself; withheld rather than sent to a drained host for a certain refusal. */
const WITHHELD: Partial<Record<ComponentName, string>> = {
  "recovery-actor": "arrives with #362",
};

/** Owned GPU hosts, the only machines this build offers. */
export function developerApplyMachines(view: PlatformReleaseView): PlatformHostIdentity[] {
  return view.installed.hosts.filter((h) => h.install_mode === "owned");
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
  machines: PlatformHostIdentity[];
  onClose: () => void;
  onApplied: () => void;
}) {
  const { token } = useAuth();
  const [hostId, setHostId] = useState(machines[0]?.host_id ?? "");
  const [values, setValues] = useState<Record<ComponentName, string>>({
    "recovery-actor": "",
    "node-agent": "",
    "control-plane": "",
  });
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const offered = SLOTS.filter((s) => HOST_COMPONENTS.has(s.name));
  const parsed = offered
    .filter((s) => values[s.name].trim() !== "")
    .map((s) => ({ slot: s, result: parseImageReference(values[s.name]) }));
  const invalid = parsed.filter((p) => !p.result.ok).length;
  const components: ApplyComponentDigest[] = parsed.flatMap(({ slot, result }) =>
    result.ok ? [{ name: slot.name, image: result.image, digest: result.digest }] : [],
  );
  const machine = machines.find((m) => m.host_id === hostId);

  const apply = useAdminAction(
    async () =>
      adminApi.developerApply(token ?? "", {
        target: "host",
        host_id: hostId,
        components,
        force: false,
      }),
    {
      // Accepted is not started: the attempt may wait for the host's sessions to end.
      success: `Developer apply queued for ${machine?.node_name ?? "the host"}.`,
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
  const canApply = !busy && hostId !== "" && parsed.length > 0 && invalid === 0;

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
          <span className="hint">{footerHint(parsed.length, invalid)}</span>
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
            value={hostId}
            onChange={(e) => {
              setHostId(e.target.value);
              setRefusal(null);
            }}
          >
            {machines.map((m) => (
              <option key={m.host_id} value={m.host_id}>
                {m.node_name} · GPU host
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
            HOST_COMPONENTS.has(slot.name) ? (
              <ImageField
                key={slot.name}
                slot={slot}
                value={values[slot.name]}
                onChange={(v) => edit(slot.name, v)}
              />
            ) : (
              <ImageField
                key={slot.name}
                slot={slot}
                value=""
                absent={WITHHELD[slot.name] ?? "not on this machine"}
              />
            ),
          )}
        </div>
      </div>

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
