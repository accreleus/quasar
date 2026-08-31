// Sources tab (Task 25): where catalog content and cover art come from.
// Scan-cadence and app-details-lookup controls stay on Settings (§A.21) and
// are only linked from here, so there is exactly one place to edit them.
//
// RomM is omitted (ui-v3-design spec §9): no RomM provider exists yet.

import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import { ARTWORK_API_KEY_SECRET } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconChevronRight, IconRefresh } from "../../../components/icons";
import { ResourceStates } from "../../../components/ResourceStates";
import { SecretField } from "../../../components/SecretField";
import { useToast } from "../../../components/Toast";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { ScanHealth } from "./ScanHealth";
import { SourceRow } from "./SourceRow";
import { inertReasonCopy, lastScanText, scanResultToast } from "./sourcesDerived";
import { STEAM_PROVIDER_LABEL, useSourcesData } from "./useSourcesData";

// One extra refetch a short while after "Scan now": the agent walks scans
// asynchronously, so an immediate refetch essentially never shows the new
// row. One shot, not a poll.
const POST_SCAN_REFETCH_MS = 5000;

export function SourcesTab() {
  const { token } = useAuth();
  const { addToast } = useToast();
  const navigate = useNavigate();
  const data = useSourcesData(ARTWORK_API_KEY_SECRET);

  const [scanning, setScanning] = useState(false);
  const [showHealth, setShowHealth] = useState(false);
  const postScanTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    return () => {
      if (postScanTimerRef.current) clearTimeout(postScanTimerRef.current);
    };
  }, []);

  useSectionHead({
    sub: "Where catalog content and cover art come from. Everything a source discovers lands in Apps.",
    counts: { sources: 2 },
  });

  async function handleScanNow() {
    if (!token) return;
    setScanning(true);
    try {
      const res = await adminApi.forceLibraryScan(token);
      const copy = scanResultToast(res);
      addToast({ variant: copy.variant, title: copy.title });
      data.refreshStatus();
      if (postScanTimerRef.current) clearTimeout(postScanTimerRef.current);
      postScanTimerRef.current = setTimeout(() => {
        postScanTimerRef.current = null;
        data.refreshStatus();
      }, POST_SCAN_REFETCH_MS);
    } catch (e: unknown) {
      addToast({
        variant: "danger",
        title: "Could not start a scan",
        body: e instanceof ApiError ? e.message : undefined,
      });
    } finally {
      setScanning(false);
    }
  }

  const s = data.settings.settings;
  const ls = data.status;
  const artworkConfigured = data.artworkSecret?.configured ?? false;

  return (
    <section className="page">
      <div className="eyebrow" style={{ marginBottom: 10 }}>
        Content sources
      </div>
      <div className="card mb6">
        <ResourceStates loading={data.loading} error={data.error} />
        {!data.loading && !data.error && s && ls && (
          <>
            <SourceRow
              name={STEAM_PROVIDER_LABEL}
              description="Discovers titles from the Steam library installed on your hosts. Importing one creates an app that inherits the Proton GPU preset."
              meta={
                data.unpublishedError ? (
                  <p className="form-error" role="alert" style={{ margin: 0 }}>
                    Could not read pending discovery items: {data.unpublishedError}
                  </p>
                ) : data.unpublishedLoading ? (
                  "Loading counts…"
                ) : (
                  <>
                    {data.counts.discovered} title{data.counts.discovered === 1 ? "" : "s"} discovered ·{" "}
                    <strong>{data.counts.imported} imported</strong> · last scan{" "}
                    {lastScanText(ls.last_scan_completed_at)}
                  </>
                )
              }
              actions={
                <>
                  <Button variant="ghost" size="sm" onClick={() => void handleScanNow()} disabled={scanning}>
                    <IconRefresh /> {scanning ? "Scanning…" : "Scan now"}
                  </Button>
                  {!data.unpublishedError && !data.unpublishedLoading && data.pendingCount > 0 && (
                    <Button size="sm" onClick={() => navigate("/admin/library/apps?segment=pending")}>
                      Review {data.pendingCount} pending <IconChevronRight />
                    </Button>
                  )}
                </>
              }
              switchChecked={s.library_discovery_enabled}
              onSwitchChange={(v) => void data.settings.patch("library_discovery_enabled", v)}
              switchDisabled={data.settings.pending === "library_discovery_enabled"}
              switchLabel="Steam discovery"
            >
              {ls.inert_reason && (
                <div className="note warn" style={{ marginTop: "var(--s3)" }}>
                  <div>{inertReasonCopy(ls.inert_reason)}</div>
                </div>
              )}
              <div style={{ marginTop: "var(--s3)" }}>
                <Button variant="ghost" size="sm" onClick={() => setShowHealth((v) => !v)}>
                  {showHealth ? "Hide scan health" : "Show scan health"}
                </Button>
                {showHealth && (
                  <ScanHealth
                    status={ls}
                    steamApp={data.steamApp}
                    preset={data.preset}
                    presetLoading={data.presetLoading}
                  />
                )}
              </div>
            </SourceRow>

            <SourceRow
              last
              name="Manual apps"
              description="Apps you define by hand against a runtime preset. Always on, because this is how anything outside a source gets into the catalog."
              meta={`${data.manualCount} app${data.manualCount === 1 ? "" : "s"} defined by hand`}
              actions={
                <Button size="sm" onClick={() => navigate("/admin/library/apps?source=manual")}>
                  Open Apps <IconChevronRight />
                </Button>
              }
              switchChecked
              switchDisabled
              switchLabel="Manual apps"
              switchTitle="Manual apps are always available. There is no switch to flip."
            />
          </>
        )}
      </div>

      <div className="eyebrow" style={{ margin: "var(--s7) 0 10px" }}>
        Artwork providers
      </div>
      <div className="card mb6">
        <ResourceStates loading={data.secretsLoading} error={data.secretsError} />
        {!data.secretsLoading && !data.secretsError && data.secretsData && (
          data.artworkSecret && token ? (
            <SourceRow
              last
              name="SteamGridDB"
              badge={
                <Chip variant={artworkConfigured ? "success" : "neutral"}>
                  {artworkConfigured ? "configured" : "not configured"}
                </Chip>
              }
              description="Artwork provider for the catalogue. Needs an API key from steamgriddb.com."
              switchChecked={artworkConfigured}
              switchDisabled
              switchLabel="SteamGridDB"
              switchTitle="Artwork lookup is automatic once a key is configured. This switch is informational."
            >
              <div style={{ marginTop: "var(--s4)", maxWidth: 560 }}>
                <SecretField
                  secret={data.artworkSecret}
                  masterKeyConfigured={data.secretsData.master_key_configured}
                  token={token}
                  onChange={() => data.refreshSecrets()}
                  label="API key"
                  hideStatus
                />
              </div>
            </SourceRow>
          ) : (
            <div className="card-pad">
              <p className="muted">This deployment declares no artwork provider credential yet.</p>
            </div>
          )
        )}
      </div>

      <div className="note" style={{ marginTop: "var(--s4)" }}>
        Artwork providers are tried in order. Apps with no match keep their gradient tile.
      </div>
    </section>
  );
}
