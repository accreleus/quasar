// Wizard step 4 — library providers (#454); hands off to StepFinishing.
//
// Do not re-implement the server side: PATCH /v1/admin/settings
// library_discovery_enabled=true drives the whole idempotent provider chain
// (internal/images/provider.go EnsureProviders — eager install + preset +
// provider app). No lazy checkbox: EnsureProviders has no lazy path.
// Providers come from GET /v1/admin/images (any row with a library_provider),
// so a second provider needs no wizard change. In-flight state comes from
// admin/library/imageStatus.ts directly, not the Images tab, so this
// pre-auth step doesn't defeat the admin route's code-splitting.
//
// #465 entitlement mode: a second call after the settings PATCH
// (POST /v1/admin/library-providers/{provider}/entitlement-mode, by provider
// NAME — the app is created off-thread so no id exists yet), bounded-retried;
// a lasting 404 degrades to honest copy, never blocks Continue.
//
// #461 virgin instance: zero catalog rows until a sync has run, so an empty
// provider list with `fetched_at == null` triggers one auto-sync per mount
// (ref-guarded against StrictMode double-invoke); a sync failure degrades to
// the empty-state copy plus an error line, never a dead end.
//
// No design_handoff_v3 mockup covers this screen — matches the other steps'
// layout and components rather than inventing anything.

import { useEffect, useMemo, useRef, useState } from "react";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import { useAuth } from "../../auth/context";
import type { CatalogImage, ProviderEntitlementMode } from "../../api/types";
import { Button } from "../../components/Button";
import { SegmentedControl } from "../../components/SegmentedControl";
import { StatusChip, type StatusChipConfig } from "../../components/StatusChip";
import { Switch } from "../../components/TextField";
import { dominantInFlightState, HOST_STATE_COPY } from "../admin/library/imageStatus";

// Kept beside the mode list so the summary chip and the picker hint can never
// drift into describing different modes.
const ENTITLEMENT_MODE_OPTIONS: { value: ProviderEntitlementMode; label: string }[] = [
  { value: "all", label: "Everyone" },
  { value: "user", label: "Only me" },
  { value: "none", label: "Nobody yet" },
];

const ENTITLEMENT_MODE_HINT: Record<ProviderEntitlementMode, string> = {
  all: "Enabling this provider makes it available to all users.",
  user: "Enabling this provider makes it available to your account only. You can invite others later from Admin → Apps.",
  none: "Enabling this provider creates it but grants nobody access yet. You'll need to grant access from Admin → Apps before anyone (including you) can see it.",
};

// The entitlement-mode call can legitimately 404 for a few seconds after the
// settings PATCH (app creation is off-thread). 6 × 1.5s of patience, never an
// unbounded loop, never blocks Continue.
export async function applyEntitlementModeWithRetry(
  token: string,
  provider: string,
  mode: ProviderEntitlementMode,
  attempts = 6,
  delayMs = 1500,
): Promise<void> {
  for (let i = 0; i < attempts; i++) {
    try {
      await adminApi.setProviderEntitlementMode(token, provider, mode);
      return;
    } catch (err) {
      const isLastAttempt = i === attempts - 1;
      const notReadyYet = err instanceof ApiError && err.status === 404;
      if (isLastAttempt || !notReadyYet) throw err;
      await new Promise((resolve) => setTimeout(resolve, delayMs));
    }
  }
}

interface StepLibrariesProps {
  /** Advances to step 5 (StepFinishing) — this step no longer finishes the wizard. */
  onNext: () => void;
}

interface ProviderEntry {
  kind: string;
  displayName: string;
  description?: string;
}

type ProviderStatus = "pulling" | "building" | "ready" | "failed" | "settling";

/**
 * One chip's worth of provider state. "settling" covers isImageInFlight's
 * documented gap: adopted with no host report yet — neither in-flight nor
 * honestly "ready".
 */
function providerStatus(images: CatalogImage[], kind: string): ProviderStatus | null {
  const rows = images.filter((img) => img.library_provider === kind);
  if (rows.length === 0) return null; // catalog hasn't reported this provider's image yet
  // Aggregate across all rows before applying precedence — row-by-row let a
  // building row beat a pulling one on array order alone (flickering chip).
  if (rows.some((r) => dominantInFlightState(r) === "pulling")) return "pulling";
  if (rows.some((r) => dominantInFlightState(r) === "building")) return "building";
  if (rows.some((r) => (r.hosts ?? []).some((h) => h.state === "failed")) && !rows.some((r) => r.installed)) {
    return "failed";
  }
  if (rows.every((r) => r.installed)) return "ready";
  return "settling";
}

// null status is keyed as "queued". Labels come off HOST_STATE_COPY so this
// can never drift from library/ImagesTab's vocabulary.
const PROVIDER_STATUS_CHIP_CONFIG: StatusChipConfig = {
  queued: { label: "Queued…", variant: "neutral" },
  pulling: { label: HOST_STATE_COPY.pulling.label, variant: HOST_STATE_COPY.pulling.variant, dot: true },
  building: { label: HOST_STATE_COPY.building.label, variant: HOST_STATE_COPY.building.variant, dot: true },
  ready: { label: HOST_STATE_COPY.ready.label, variant: HOST_STATE_COPY.ready.variant, dot: true },
  failed: { label: HOST_STATE_COPY.failed.label, variant: HOST_STATE_COPY.failed.variant },
  settling: { label: "Installing…", variant: "neutral" }, // adopted, no host report yet
};

function providerStatusKey(status: ProviderStatus | null): string {
  return status ?? "queued";
}

export function StepLibraries({ onNext }: StepLibrariesProps) {
  const { token } = useAuth();

  const [images, setImages] = useState<CatalogImage[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [syncing, setSyncing] = useState(false);
  const [syncError, setSyncError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [phase, setPhase] = useState<"select" | "submitting" | "submitted">("select");
  const [submitErrors, setSubmitErrors] = useState<Record<string, string>>({});
  // #465 per-provider mode; missing = "all", the server's own create default.
  const [entitlementMode, setEntitlementMode] = useState<Record<string, ProviderEntitlementMode>>({});
  // Second-call degradation only (mode call retried out); separate from
  // submitErrors, which is the settings PATCH itself failing.
  const [modeErrors, setModeErrors] = useState<Record<string, string>>({});

  function providersOf(rows: CatalogImage[]): ProviderEntry[] {
    const byKind = new Map<string, ProviderEntry>();
    for (const img of rows) {
      if (!img.library_provider) continue;
      if (byKind.has(img.library_provider)) continue;
      byKind.set(img.library_provider, {
        kind: img.library_provider,
        displayName: img.display_name,
        description: img.description,
      });
    }
    return [...byKind.values()];
  }

  // #461 auto-sync guard — a ref, not state: must be readable synchronously
  // in the same effect run that sets it (StrictMode double-invoke).
  const autoSyncFired = useRef(false);

  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    setLoadError(null);
    adminApi
      .listImages(token)
      .then((envelope) => {
        if (cancelled) return;
        const hasProviders = providersOf(envelope.images).length > 0;
        // Virgin instance: an unsynced catalog proves nothing — auto-sync once
        // rather than showing the "no providers" dead end.
        if (!hasProviders && envelope.fetched_at == null && !autoSyncFired.current) {
          autoSyncFired.current = true;
          setSyncing(true);
          setSyncError(null);
          adminApi
            .syncImages(token)
            .then((synced) => {
              if (cancelled) return;
              setImages(synced.images);
              setSyncError(null);
            })
            .catch((err) => {
              if (cancelled) return;
              setSyncError(err instanceof ApiError ? err.message : "Could not fetch the image catalog.");
              setImages(envelope.images);
            })
            .finally(() => {
              if (!cancelled) setSyncing(false);
            });
          return;
        }
        setImages(envelope.images);
      })
      .catch((err) => {
        if (cancelled) return;
        setLoadError(err instanceof ApiError ? err.message : "Could not load the image catalog.");
        setImages([]);
      });
    return () => {
      cancelled = true;
    };
  }, [token]);

  const providers = useMemo<ProviderEntry[]>(() => (images ? providersOf(images) : []), [images]);

  const anySelected = providers.some((p) => selected[p.kind]);

  // Poll while any submitted provider is still moving (#455), scoped to the
  // providers this step enabled.
  const submittedKinds = useMemo(
    () => providers.map((p) => p.kind).filter((k) => selected[k]),
    [providers, selected],
  );
  const anyStillMoving =
    phase === "submitted" &&
    images !== null &&
    submittedKinds.some((k) => {
      const s = providerStatus(images, k);
      return s === null || s === "pulling" || s === "building" || s === "settling";
    });

  useEffect(() => {
    if (!anyStillMoving || !token) return;
    const id = window.setInterval(() => {
      adminApi
        .listImages(token)
        .then((envelope) => setImages(envelope.images))
        .catch(() => {
          /* best-effort poll; a transient failure just tries again next tick */
        });
    }, 4000);
    return () => window.clearInterval(id);
  }, [anyStillMoving, token]);

  function toggle(kind: string, next: boolean) {
    setSelected((prev) => ({ ...prev, [kind]: next }));
  }

  async function handleContinue() {
    if (!token) return;
    if (!anySelected) {
      onNext();
      return;
    }
    setPhase("submitting");
    const errors: Record<string, string> = {};
    const modeFailures: Record<string, string> = {};
    for (const kind of submittedKinds) {
      try {
        // Only "steam" maps to a settings field today — mirrors the
        // Settings page's Libraries card.
        if (kind === "steam") {
          await adminApi.updateSettings(token, { library_discovery_enabled: true });
        }
      } catch (err) {
        errors[kind] = err instanceof ApiError ? err.message : `Could not enable ${kind}.`;
        continue; // nothing was enabled — no app to set a mode on
      }
      // #465: second call, only when the mode differs from the create default.
      const mode = entitlementMode[kind] ?? "all";
      if (mode !== "all") {
        try {
          await applyEntitlementModeWithRetry(token, kind, mode);
        } catch (err) {
          modeFailures[kind] = err instanceof ApiError ? err.message : `Could not set who can see ${kind} yet.`;
        }
      }
    }
    setSubmitErrors(errors);
    setModeErrors(modeFailures);
    setPhase("submitted");
    // Refresh immediately so the just-enabled provider's install state shows
    // up without waiting a full poll tick.
    try {
      const envelope = await adminApi.listImages(token);
      setImages(envelope.images);
    } catch {
      /* the poll effect above will retry */
    }
  }

  return (
    <div
      className="card login-card"
      style={{ width: "100%", maxWidth: 640, display: "flex", flexDirection: "column", gap: "var(--s5)" }}
    >
      <div>
        <h2 style={{ margin: 0 }}>Libraries</h2>
        <p className="sub" style={{ marginTop: 6 }}>
          Optional: automatically discover games your users already own from a
          library provider and publish them as launchable tiles. Everything
          here stays fully configurable later from{" "}
          <strong>Admin → Settings</strong> — leaving every provider off just
          means nobody has turned it on yet.
        </p>
      </div>

      {loadError && (
        <p className="login-error" role="alert">
          {loadError}
        </p>
      )}

      {images === null && !loadError && !syncing && <p className="muted">Loading library providers…</p>}

      {syncing && <p className="muted">Fetching the image catalog…</p>}

      {!syncing && images !== null && providers.length === 0 && !loadError && (
        <>
          <p className="muted">
            No library providers are in the image catalog yet. This is not a
            problem — enable one anytime later from{" "}
            <strong>Admin → Settings</strong> once the catalog has synced.
          </p>
          {syncError && (
            <p className="login-error" role="alert">
              {syncError}
            </p>
          )}
        </>
      )}

      {providers.length > 0 && phase === "select" && (
        <div style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
          {providers.map((p) => (
            <div
              key={p.kind}
              style={{
                border: "1px solid var(--line-2)",
                borderRadius: "var(--r-sm)",
                padding: "var(--s4)",
                display: "flex",
                flexDirection: "column",
                gap: "var(--s3)",
              }}
            >
              <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", gap: "var(--s4)" }}>
                <div>
                  <strong>{p.displayName}</strong>
                  {p.description && (
                    <p className="field-hint" style={{ margin: "4px 0 0" }}>
                      {p.description}
                    </p>
                  )}
                  <p className="field-hint" style={{ margin: "4px 0 0" }}>
                    The provider's image is a real download (roughly 2+ GB) —
                    enabling it starts a background install on every host.
                  </p>
                </div>
                <Switch
                  checked={selected[p.kind] ?? false}
                  onChange={(v) => toggle(p.kind, v)}
                  label={selected[p.kind] ? "Enabled" : "Enable"}
                  id={`library-${p.kind}`}
                />
              </div>

              {/* #465 "who can see it" (first-run §S3). activation="manual":
                  the choice is submitted on Continue, never on segment focus —
                  SegmentedControl's rule that activation must not drive a fetch. */}
              {selected[p.kind] && (
                <div style={{ display: "flex", flexDirection: "column", gap: "var(--s2)" }}>
                  <SegmentedControl
                    aria-label={`Who can see ${p.displayName}`}
                    value={entitlementMode[p.kind] ?? "all"}
                    onChange={(v) => setEntitlementMode((prev) => ({ ...prev, [p.kind]: v }))}
                    options={ENTITLEMENT_MODE_OPTIONS}
                    activation="manual"
                  />
                  <p className="field-hint" style={{ margin: 0 }}>
                    {ENTITLEMENT_MODE_HINT[entitlementMode[p.kind] ?? "all"]}
                  </p>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {phase !== "select" && submittedKinds.length > 0 && (
        <div style={{ display: "flex", flexDirection: "column", gap: "var(--s4)" }}>
          <p className="field-hint" style={{ margin: 0 }}>
            The download can take a while depending on your connection — it's
            safe to leave this page. Installs continue in the background, and
            you can always check progress later from{" "}
            <strong>Admin → Images</strong>.
          </p>
          {submittedKinds.map((kind) => {
            const provider = providers.find((p) => p.kind === kind);
            const status = images ? providerStatus(images, kind) : null;
            return (
              <div
                key={kind}
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: "var(--s3)",
                  border: "1px solid var(--line-2)",
                  borderRadius: "var(--r-sm)",
                  padding: "var(--s3) var(--s4)",
                }}
              >
                <span>{provider?.displayName ?? kind}</span>
                <StatusChip status={providerStatusKey(status)} config={PROVIDER_STATUS_CHIP_CONFIG} />
              </div>
            );
          })}
          {Object.entries(submitErrors).map(([kind, msg]) => (
            <p key={kind} className="form-error" style={{ margin: 0 }}>
              Could not enable {providers.find((p) => p.kind === kind)?.displayName ?? kind}: {msg}
            </p>
          ))}
          {/* An enabled provider stuck on "all" must say so honestly. */}
          {Object.entries(modeErrors).map(([kind, msg]) => (
            <p key={kind} className="form-error" style={{ margin: 0 }}>
              {providers.find((p) => p.kind === kind)?.displayName ?? kind} is enabled and
              visible to all users for now — could not switch it to{" "}
              {ENTITLEMENT_MODE_OPTIONS.find((o) => o.value === entitlementMode[kind])?.label.toLowerCase()}{" "}
              yet ({msg}). Set it from <strong>Admin → Apps</strong> once you're in.
            </p>
          ))}
          {submittedKinds.some((kind) => providerStatus(images ?? [], kind) === "failed") && (
            <p className="login-error" role="alert">
              An install didn't complete automatically. That's fine for now —
              continue setup and retry it from <strong>Admin → Images</strong>{" "}
              once you're in.
            </p>
          )}
        </div>
      )}

      <Button
        type="button"
        variant="primary"
        size="lg"
        disabled={phase === "submitting" || syncing}
        onClick={() => (phase === "submitted" ? onNext() : void handleContinue())}
      >
        {phase === "submitting"
          ? "Enabling…"
          : syncing
            ? "Fetching catalog…"
            : phase === "submitted"
              ? "Continue"
              : anySelected
                ? "Continue"
                : "Skip and continue"}
      </Button>
    </div>
  );
}
