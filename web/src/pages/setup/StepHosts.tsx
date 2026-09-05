// Wizard step 3 — host/GPU check (first-run-wizard spec "telling the truth").
// Reads real state (GET /v1/hosts + /gpus + /settings) and is deliberately
// non-blocking: a failing check warns and links to /admin/fleet/hosts, never blocks.
//
// Storage (§S4b/§S4c): `effective.home_root` is the agent's own reported value
// — the one path compose bind-mounts (`${QUASAR_HOME_ROOT}:${QUASAR_HOME_ROOT}`)
// — and `resolveHomeDriver` (lib/hostStorage.ts) mirrors control-plane's
// resolveDriver so this card cannot disagree with a launch. Only the reported
// root or a subpath is typeable (mirror of hostcfg.ValidateHomeRootUnder, the
// real gate). The free-text escape hatch is gone: it let an operator write a
// path the agent cannot see, which failed silently at every later layer. A
// different root means QUASAR_HOME_ROOT in deploy/.env plus a redeploy.
//
// Codecs (§S5): lists `HostSettingsResponse.codecs` — what the agent reported,
// not the catalog — off the same settings call, no extra request. Not an
// enablement control (migration 0046 already chains AV1→HEVC→H.264).
// `explainCodecGap` (lib/hostCodecs.ts) owns the wording, including the one
// operator knob (QUASAR_VULKAN_HEVC=0 on a Vulkan host).

import { useState } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/context";
import type { GPUAvailability, Host, HostSettingsResponse, StorageProvider } from "../../api/types";
import { Button } from "../../components/Button";
import { Chip } from "../../components/Chip";
import { useResource } from "../../lib/resource/react";
import { ReadinessCard } from "../../components/ReadinessCard";
import { StatusChip, type StatusChipConfig } from "../../components/StatusChip";
import { codecDisplayName } from "../../lib/codecDisplay";
import { explainCodecGap } from "../../lib/hostCodecs";
import { MISCONFIGURED_LOCAL_IMPACT, resolveHomeDriver } from "../../lib/hostStorage";

interface StepHostsProps {
  onNext: () => void;
}

interface HostRow {
  host: Host;
  gpus: GPUAvailability[] | null; // null = GPU fetch failed for this host
  settings: HostSettingsResponse | null; // null = settings fetch failed for this host
}

// Host.status is a closed union; anything but online/draining renders Offline.
const HOST_STATUS_CHIP_CONFIG: StatusChipConfig = {
  online: { label: "Online", variant: "success", dot: true },
  draining: { label: "Draining", variant: "warning", dot: true },
  offline: { label: "Offline", variant: "danger", dot: true },
};

function hostStatusKey(host: Host): string {
  return host.status === "online" || host.status === "draining" ? host.status : "offline";
}

/** The root a launch would use now: override wins, else the agent's reported
 *  value. Never `settings.resolved.home_root` — that display value ignores the
 *  agent report and reads "" on an un-overridden host. Mirrors
 *  hostcfg.Store.HomeRoot minus its process-local env fallback (invisible to
 *  any API). */
function currentHomeRoot(settings: HostSettingsResponse): { root: string; isOverride: boolean } {
  const override = settings.overrides["home_root"];
  if (typeof override === "string" && override.trim() !== "") {
    return { root: override.trim(), isOverride: true };
  }
  const effective = settings.effective?.["home_root"];
  return { root: (effective ?? "").trim(), isOverride: false };
}

export function StepHosts({ onNext }: StepHostsProps) {
  const { token } = useAuth();
  const hostResource = useResource<HostRow[]>({
    label: "setup hosts",
    pollMs: 5000,
    fetch: async ({ token }) => {
      const { items } = await adminApi.listHosts(token);
      return Promise.all(items.map(async (host) => {
        const [gpus, settings] = await Promise.all([
          adminApi.getHostGPUs(token, host.id).then(r => r.items, () => null),
          adminApi.getHostSettings(token, host.id).then(r => r, () => null),
        ]);
        return { host, gpus, settings };
      }));
    },
  });
  const settingsResource = useResource<Awaited<ReturnType<typeof adminApi.getSettings>>>({
    label: "setup storage settings",
    fetch: ({ token, signal }) => adminApi.getSettings(token, signal),
  });
  const rows = hostResource.data ?? null;
  const loadError = hostResource.errorMessage;
  const storageProvider = settingsResource.data?.settings.storage_provider ?? null;

  function applySettingsUpdate(hostId: string, next: HostSettingsResponse) {
    hostResource.setData(prev => prev.map(row => row.host.id === hostId ? { ...row, settings: next } : row));
  }

  const anyIssue =
    rows !== null &&
    (rows.length === 0 || rows.some((r) => r.host.status !== "online" || r.host.capacity_detection !== "ok"));

  return (
    <div
      className="card login-card"
      style={{ width: "100%", maxWidth: 640, display: "flex", flexDirection: "column", gap: "var(--s5)" }}
    >
      <div>
        <h2 style={{ margin: 0 }}>Host &amp; GPU check</h2>
        <p className="sub" style={{ marginTop: 6 }}>
          What the control plane has actually heard from your node agents —
          not what was declared.
        </p>
      </div>

      <p className="login-error" role="note" style={{ color: "var(--info-text)", background: "var(--info-bg)", borderColor: "var(--info-line)" }}>
        Media (WebRTC) is LAN/VPN-only in this release — there is no STUN/TURN
        yet. A player connecting from outside your network needs a VPN into
        it; this is a deliberate v1 posture, not a bug.
      </p>

      {loadError && (
        <p className="login-error" role="alert">
          {loadError}
        </p>
      )}

      {rows === null && <p className="muted">Checking registered hosts…</p>}

      {rows !== null && rows.length === 0 && !loadError && (
        <p className="login-error" role="alert" style={{ color: "var(--warning-text)", background: "var(--warning-bg)", borderColor: "var(--warning-line)" }}>
          No hosts have registered with this control plane yet. Bring up a
          node agent and point it at this instance, then check{" "}
          <Link to="/admin/fleet/hosts">Admin → Hosts</Link>.
        </p>
      )}

      {rows !== null && rows.length > 0 && (
        <div style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
          {rows.map(({ host, gpus, settings }) => (
            <div
              key={host.id}
              style={{
                border: "1px solid var(--line-2)",
                borderRadius: "var(--r-sm)",
                padding: "var(--s4)",
                display: "flex",
                flexDirection: "column",
                gap: "var(--s2)",
              }}
            >
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "var(--s3)" }}>
                <strong>{host.node_name}</strong>
                <StatusChip status={hostStatusKey(host)} config={HOST_STATUS_CHIP_CONFIG} />
              </div>
              <span className="field-hint">
                {host.last_registered_at
                  ? `Registered ${new Date(host.last_registered_at).toLocaleString()}`
                  : "Never registered"}
              </span>

              {host.capacity_detection !== "ok" && (
                <p className="login-error" role="alert" style={{ margin: 0 }}>
                  {host.capacity_reason ?? `Capacity detection ${host.capacity_detection}.`}
                </p>
              )}

              {gpus === null && (
                <p className="login-error" role="alert" style={{ margin: 0 }}>
                  Could not read GPU/encoder detail for this host.
                </p>
              )}
              {gpus !== null && gpus.length === 0 && (
                <p className="login-error" role="alert" style={{ margin: 0 }}>
                  No GPU/encoder detected on this host — sessions cannot launch here.
                </p>
              )}
              {gpus !== null && gpus.length > 0 && (
                <ul style={{ margin: 0, paddingLeft: "1.2em" }}>
                  {gpus.map((g) => (
                    <li key={g.gpu_id} className="field-hint">
                      {g.vendor} {g.model} — {g.slots_total} encode slot{g.slots_total === 1 ? "" : "s"},{" "}
                      {g.active_sessions} active session{g.active_sessions === 1 ? "" : "s"}
                    </li>
                  ))}
                </ul>
              )}

              {/* Readiness is refreshed by the agent and polled while this step is visible. */}
              <ReadinessCard
                checks={host.readiness}
                reportedAt={host.readiness_reported_at}
                footnote={
                  <>
                    Checks update automatically. Driver provisioning retries recoverable failures
                    and restarts the agent when its new libraries require it. If a check requests
                    container recreation, use <Link to="/admin/fleet/hosts">Admin → Hosts</Link>.
                  </>
                }
              />

              <CodecSection settings={settings} />

              {/* §S4b/§S4c — a rootless host is misconfigured, not blocking. */}
              {token && (
                <HomeStorageSection
                  token={token}
                  host={host}
                  settings={settings}
                  storageProvider={storageProvider}
                  onSettingsUpdated={(next) => applySettingsUpdate(host.id, next)}
                />
              )}
            </div>
          ))}
        </div>
      )}

      {anyIssue && (
        <p className="login-error" role="alert" style={{ color: "var(--warning-text)", background: "var(--warning-bg)", borderColor: "var(--warning-line)" }}>
          Something above needs attention, but that does not have to happen
          now — finish setup and fix it later from{" "}
          <Link to="/admin/fleet/hosts">Admin → Hosts</Link>.
        </p>
      )}

      <Button type="button" variant="primary" size="lg" onClick={onNext}>
        Continue
      </Button>
    </div>
  );
}

const warningBoxStyle = {
  color: "var(--warning-text)",
  background: "var(--warning-bg)",
  borderColor: "var(--warning-line)",
  margin: 0,
} as const;

const dangerBoxStyle = {
  color: "var(--danger-text)",
  background: "var(--danger-bg)",
  borderColor: "var(--danger-line)",
  margin: 0,
} as const;

/** §S5 codec truth-telling; no control ever ("tell the truth, do not add a
 *  toggle"). Three states kept distinct: settings unreadable → say so;
 *  `codecs` null (pre-multi-codec agent — the API deliberately does not
 *  normalise to ["h264"]) → "not reported" plus the consequence, never an
 *  assertion; present → list them and explain any gap (explainCodecGap). */
function CodecSection({ settings }: { settings: HostSettingsResponse | null }) {
  if (!settings) {
    return (
      <p className="field-hint" style={{ margin: 0 }}>
        Codecs: could not read this host's settings.
      </p>
    );
  }

  const codecs = settings.codecs ?? null;

  if (codecs === null || codecs.length === 0) {
    return (
      <p className="field-hint" style={{ margin: 0 }}>
        Codecs: this host's agent has not reported a codec set yet, so sessions placed here will be
        treated as H.264-only until it does. That normally means an older node-agent — restart it
        from <Link to="/admin/fleet/hosts">Admin → Hosts</Link> once setup is finished and this fills in.
      </p>
    );
  }

  // The agent's own reported encoder, never `resolved` — that view cannot see
  // agent env and reads "openh264" on an un-overridden Vulkan host, which
  // would suppress the one gap with a real fix.
  const encoder = settings.effective?.["encoder"] ?? null;
  const gap = explainCodecGap(codecs, encoder);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--s2)" }}>
      <div style={{ display: "flex", alignItems: "center", gap: "var(--s2)", flexWrap: "wrap" }}>
        <span className="field-hint">Codecs this host reports:</span>
        {codecs.map((c) => (
          <Chip key={c} variant="success">
            {codecDisplayName(c) ?? c}
          </Chip>
        ))}
      </div>
      {gap && (
        <p className="login-error" role="note" style={warningBoxStyle}>
          {gap.reason}
        </p>
      )}
    </div>
  );
}

/** §S4b storage summary: the agent-reported root + the driver computed as
 *  control-plane's resolveDriver does (lib/hostStorage.ts); "not yet reported"
 *  when settings failed, rather than pretending to know. */
function HomeStorageSection({
  token,
  host,
  settings,
  storageProvider,
  onSettingsUpdated,
}: {
  token: string;
  host: Host;
  settings: HostSettingsResponse | null;
  storageProvider: StorageProvider | null;
  onSettingsUpdated: (next: HostSettingsResponse) => void;
}) {
  if (!settings) {
    return (
      <p className="field-hint" style={{ margin: 0 }}>
        Storage: could not read this host's settings.
      </p>
    );
  }

  const { root: currentRoot, isOverride } = currentHomeRoot(settings);
  const effectiveRoot = (settings.effective?.["home_root"] ?? "").trim();

  if (!storageProvider) {
    return (
      <p className="field-hint" style={{ margin: 0 }}>
        Storage: effective root {effectiveRoot || "not set"} (driver unknown — could not read the
        instance storage provider).
      </p>
    );
  }

  const { driver } = resolveHomeDriver(storageProvider, currentRoot);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--s2)" }}>
      {/* Danger, not warning: every session placed on a rootless host fails.
          resolveHomeDriver can no longer return "volume" (#473). */}
      <div style={{ display: "flex", alignItems: "center", gap: "var(--s2)", flexWrap: "wrap" }}>
        <span className="field-hint">Managed-home storage:</span>
        {driver === "local" && <Chip variant="success">Local — {currentRoot}</Chip>}
        {driver === "misconfigured" && <Chip variant="danger">No storage root</Chip>}
      </div>

      {driver === "misconfigured" && (
        <p className="login-error" role="alert" style={dangerBoxStyle}>
          {MISCONFIGURED_LOCAL_IMPACT}
        </p>
      )}

      <HomeRootControl
        token={token}
        host={host}
        effectiveRoot={effectiveRoot}
        currentRoot={currentRoot}
        isOverride={isOverride}
        onSettingsUpdated={onSettingsUpdated}
      />
    </div>
  );
}

/** Portion of `current` below `root` (no leading slash); "" when equal, empty
 *  or not a subpath. Display-only mirror — hostcfg.ValidateHomeRootUnder is
 *  the real gate. */
function relativeSubpath(root: string, current: string): string {
  if (!root || !current || current === root) return "";
  const prefix = root.replace(/\/+$/, "") + "/";
  return current.startsWith(prefix) ? current.slice(prefix.length) : "";
}

/** §S4c: the settable set is exactly the agent-reported root or a
 *  subdirectory (typed as a name against a fixed prefix — an unacceptable
 *  value is untypeable; an unrelated root is a deploy/.env + redeploy change).
 *  A save confirms only that the value was accepted, not that the path is
 *  bind-mounted — that arrives with S4a's managed_home_root readiness check. */
function HomeRootControl({
  token,
  host,
  effectiveRoot,
  currentRoot,
  isOverride,
  onSettingsUpdated,
}: {
  token: string;
  host: Host;
  effectiveRoot: string;
  currentRoot: string;
  isOverride: boolean;
  onSettingsUpdated: (next: HostSettingsResponse) => void;
}) {
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [subpath, setSubpath] = useState(() => relativeSubpath(effectiveRoot, currentRoot));
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [savedRoot, setSavedRoot] = useState<string | null>(null);

  // "Pin the agent-reported root as an explicit override" — offered even when
  // no override exists yet (env-baseline fallthrough works today but is not
  // durable against that env changing).
  const recommendedAvailable = effectiveRoot !== "" && (!isOverride || effectiveRoot !== currentRoot);

  const rootPrefix = effectiveRoot.replace(/\/+$/, "");
  const trimmedSubpath = subpath.trim().replace(/^\/+/, "").replace(/\/+$/, "");
  const joinedPath = trimmedSubpath === "" ? effectiveRoot : `${rootPrefix}/${trimmedSubpath}`;

  async function save(path: string) {
    setPending(true);
    setError(null);
    setSavedRoot(null);
    try {
      await adminApi.updateHostSettings(token, host.id, { home_root: path });
      // Re-read immediately (S4c) — a bad value surfaces here, in the wizard,
      // rather than silently at the first save-game write.
      const fresh = await adminApi.getHostSettings(token, host.id);
      onSettingsUpdated(fresh);
      setSavedRoot(path);
      setAdvancedOpen(false);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not save the storage root.");
    } finally {
      setPending(false);
    }
  }

  if (effectiveRoot === "") {
    return (
      <p className="field-hint" style={{ margin: 0 }}>
        This host's agent has not reported a storage root yet (its QUASAR_HOME_ROOT env is unset),
        so there is nothing to set a subdirectory of. Set QUASAR_HOME_ROOT in deploy/.env for this
        host and redeploy, then a root will be offered here.
      </p>
    );
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--s2)" }}>
      {recommendedAvailable && (
        <Button type="button" variant="secondary" size="sm" disabled={pending} onClick={() => save(effectiveRoot)}>
          Use this host's reported path: <code>{effectiveRoot}</code>
        </Button>
      )}

      <button
        type="button"
        onClick={() => setAdvancedOpen((o) => !o)}
        style={{
          alignSelf: "flex-start",
          background: "none",
          border: "none",
          color: "var(--accent-text)",
          textDecoration: "underline",
          cursor: "pointer",
          padding: 0,
          font: "inherit",
        }}
      >
        {advancedOpen ? "Hide advanced" : "Advanced: set a different path"}
      </button>

      {advancedOpen && (
        <div style={{ display: "flex", flexDirection: "column", gap: "var(--s2)" }}>
          <p className="field-hint" style={{ margin: 0 }}>
            A storage root can only be this host's reported root or a subdirectory of it — that is
            the only path guaranteed to be inside the agent's bind mount. Want a genuinely different
            root? Set QUASAR_HOME_ROOT in deploy/.env for this host and redeploy — the bind mount has
            to move with it, which is not something this page can do.
          </p>
          <div style={{ display: "flex", alignItems: "center", gap: "var(--s1, 4px)" }}>
            <span className="field-hint mono">{rootPrefix}/</span>
            <input
              className="input mono"
              type="text"
              value={subpath}
              onChange={(e) => setSubpath(e.target.value)}
              placeholder="a subdirectory, e.g. instance-a"
              style={{ flex: 1 }}
            />
          </div>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={pending || joinedPath === currentRoot}
            onClick={() => save(joinedPath)}
          >
            Save
          </Button>
        </div>
      )}

      {error && (
        <p className="login-error" role="alert" style={{ margin: 0 }}>
          {error}
        </p>
      )}
      {savedRoot && !error && (
        <p className="field-hint" style={{ margin: 0 }}>
          Saved {savedRoot}. It takes effect for the next home this host provisions — no restart
          needed.
        </p>
      )}
    </div>
  );
}
