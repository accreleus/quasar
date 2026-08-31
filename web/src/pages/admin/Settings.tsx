// Settings — instance-wide configuration (handoff-v3-spec.md §A.21;
// ui-v3 design spec §5.10/§9 substitutes Registration/Allowed origins/
// Libraries/Voice/Images/Secrets/Appearance for the mock's Instance
// name/Public URL/Scheduling/Rotate token rows, which have no backing
// setting). Not inside a section container — this page renders its own
// PageHeader, unlike the rest of /admin/*.
//
// One settings envelope (useSettings) backs every row except: Library
// discovery's env-override flags and inert_reason (its own status read),
// Secrets (its own registry read — a new Descriptor needs no change here),
// and Appearance (ThemeContext, per-account, no server round trip). Each
// card gates its rows on its own resource's `loading`/`error` via
// ResourceStates, never on `!data` — a failed GET must render an error line,
// not a spinner that never resolves.

import { useState } from "react";
import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import {
  ARTWORK_API_KEY_SECRET,
  type ImageUpdatePolicy,
  type LibraryStatus,
  type RegistrationMode,
  type SecretsResponse,
} from "../../api/types";
import { useAuth } from "../../auth/context";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import { PageHeader } from "../../components/PageHeader";
import { ResourceStates } from "../../components/ResourceStates";
import { SecretField } from "../../components/SecretField";
import { SegmentedControl } from "../../components/SegmentedControl";
import { Switch } from "../../components/TextField";
import { useResource } from "../../lib/resource/react";
import { useTheme, type Density, type Theme } from "../../settings/ThemeContext";
import { inertReasonCopy } from "./library/sourcesDerived";
import { SettingRow } from "./settings/SettingRow";
import { useSettings } from "./settings/useSettings";

const MIN_INTERVAL_MINUTES = 15;
const MAX_INTERVAL_MINUTES = 10080;

const REGISTRATION_OPTIONS: { value: RegistrationMode; label: string }[] = [
  { value: "closed", label: "Closed" },
  { value: "invite_only", label: "Invite only" },
  { value: "open", label: "Open" },
];

const IMAGE_POLICY_OPTIONS: { value: ImageUpdatePolicy; label: string }[] = [
  { value: "manual", label: "Manual" },
  { value: "notify", label: "Notify" },
  { value: "auto", label: "Auto" },
];

/** "Enabled"/"Disabled" everywhere — the caption reports state, not the
 *  action a click would take (that's what the row label + hint already say). */
function switchCaption(checked: boolean): string {
  return checked ? "Enabled" : "Disabled";
}

function SettingsSection({
  title,
  hint,
  children,
}: {
  title: string;
  hint: string;
  children: React.ReactNode;
}) {
  return (
    <Card>
      <div className="panel-head">
        <div>
          <span className="panel-title">{title}</span>
          <div className="hint" style={{ marginTop: 3 }}>
            {hint}
          </div>
        </div>
      </div>
      <div className="card-pad">{children}</div>
    </Card>
  );
}

export function Settings() {
  const { token } = useAuth();
  const { theme, density, setTheme, setDensity } = useTheme();
  const settings = useSettings();

  const libraryStatus = useResource<LibraryStatus | null>(
    {
      label: "library status",
      initialData: null,
      fetch: (ctx) => adminApi.getLibraryStatus(ctx.token),
    },
    [],
  );

  const secrets = useResource<SecretsResponse | null>(
    {
      label: "secrets",
      initialData: null,
      fetch: (ctx) => adminApi.listSecrets(ctx.token, ctx.signal),
    },
    [],
  );

  // Allowed origins and the scan interval are explicit-Save fields: local text
  // until saved, so a resource refresh never clobbers what the admin is
  // mid-typing. `null` means "not yet touched" — the textarea/input falls
  // back to the server value.
  const [originsText, setOriginsText] = useState<string | null>(null);
  const [originsError, setOriginsError] = useState<string | null>(null);
  const [originsSaving, setOriginsSaving] = useState(false);

  const [intervalText, setIntervalText] = useState<string | null>(null);
  const [intervalError, setIntervalError] = useState<string | null>(null);

  const s = settings.settings;
  const ls = libraryStatus.data;

  const originsValue = originsText ?? (s?.allowed_origins ?? []).join("\n");
  const intervalValue = intervalText ?? String(s?.library_discovery_interval_minutes ?? "");

  const librariesLoading = settings.loading || libraryStatus.loading;
  const librariesError = settings.error ?? libraryStatus.errorMessage;

  async function handleSaveOrigins() {
    const lines = originsValue
      .split("\n")
      .map((line) => line.trim())
      .filter(Boolean);
    setOriginsSaving(true);
    try {
      await settings.patchOrThrow({ allowed_origins: lines });
      setOriginsError(null);
    } catch (e: unknown) {
      setOriginsError(e instanceof ApiError ? e.message : "Could not update allowed origins.");
    } finally {
      setOriginsSaving(false);
    }
  }

  async function handleSaveInterval() {
    const n = Number(intervalValue);
    if (!Number.isInteger(n) || n < MIN_INTERVAL_MINUTES || n > MAX_INTERVAL_MINUTES) {
      setIntervalError(
        `Enter a whole number of minutes between ${MIN_INTERVAL_MINUTES} and ${MAX_INTERVAL_MINUTES}.`,
      );
      return;
    }
    setIntervalError(null);
    await settings.patch("library_discovery_interval_minutes", n);
  }

  return (
    <div className="page" style={{ maxWidth: 880 }}>
      <PageHeader title="Settings" sub="Instance-wide configuration" />

      <SettingsSection title="Instance" hint="Identity and access for this Quasar deployment">
        <ResourceStates loading={settings.loading} error={settings.error} />
        {!settings.loading && !settings.error && s && (
          <>
            <SettingRow label="Registration" hint="Who can create an account">
              <SegmentedControl<RegistrationMode>
                aria-label="Registration"
                value={s.registration_mode}
                onChange={(v) => void settings.patch("registration_mode", v)}
                disabled={settings.pending === "registration_mode"}
                activation="manual"
                options={REGISTRATION_OPTIONS}
              />
            </SettingRow>
            <SettingRow
              label="Allowed origins"
              hint="Browser origins allowed to call this control plane, one per line; leave empty to allow only this host"
            >
              <div className="col gap2" style={{ minWidth: 280, alignItems: "flex-end" }}>
                <textarea
                  className="input"
                  aria-label="Allowed origins"
                  rows={3}
                  value={originsValue}
                  onChange={(e) => setOriginsText(e.target.value)}
                  disabled={originsSaving}
                  style={{ width: "100%" }}
                />
                {originsError && (
                  <p className="form-error" role="alert">
                    {originsError}
                  </p>
                )}
                <Button
                  size="sm"
                  aria-label="Save allowed origins"
                  onClick={() => void handleSaveOrigins()}
                  disabled={originsSaving}
                >
                  {originsSaving ? "Saving…" : "Save"}
                </Button>
              </div>
            </SettingRow>
          </>
        )}
      </SettingsSection>

      <SettingsSection title="Libraries" hint="Discovery of installed titles on your hosts">
        <ResourceStates loading={librariesLoading} error={librariesError} />
        {!librariesLoading && !librariesError && s && ls && (
          <>
            {ls.inert_reason && (
              <div className="note warn mb4" role="status">
                <div>{inertReasonCopy(ls.inert_reason)}</div>
              </div>
            )}
            <SettingRow
              label="Library discovery"
              hint="Enabling discovery installs the Steam image on your hosts."
            >
              <Switch
                id="library-discovery-enabled"
                checked={s.library_discovery_enabled}
                onChange={(v) => void settings.patch("library_discovery_enabled", v)}
                label={switchCaption(s.library_discovery_enabled)}
                disabled={settings.pending === "library_discovery_enabled"}
              />
            </SettingRow>
            <SettingRow label="Scan interval" hint="Minutes between scans, 15 to 10080">
              <div className="col gap2" style={{ alignItems: "flex-end" }}>
                <input
                  className="input"
                  type="number"
                  aria-label="Scan interval"
                  min={MIN_INTERVAL_MINUTES}
                  max={MAX_INTERVAL_MINUTES}
                  value={intervalValue}
                  onChange={(e) => {
                    setIntervalText(e.target.value);
                    setIntervalError(null);
                  }}
                  disabled={ls.interval_overridden_by_env || settings.pending === "library_discovery_interval_minutes"}
                  style={{ width: 110 }}
                />
                {intervalError && (
                  <p className="form-error" role="alert">
                    {intervalError}
                  </p>
                )}
                {ls.interval_overridden_by_env ? (
                  <span className="hint">Set by QUASAR_LIBRARY_SCAN_INTERVAL on the server</span>
                ) : (
                  <Button
                    size="sm"
                    aria-label="Save scan interval"
                    onClick={() => void handleSaveInterval()}
                    disabled={settings.pending === "library_discovery_interval_minutes"}
                  >
                    Save
                  </Button>
                )}
              </div>
            </SettingRow>
            <SettingRow
              label="App details lookup"
              hint={
                ls.appdetails_overridden_by_env
                  ? "Set by QUASAR_STEAM_APPDETAILS_LOOKUP on the server"
                  : "Fetch titles and artwork ids from Steam for discovered games"
              }
            >
              <Switch
                id="appdetails-lookup-enabled"
                checked={s.library_discovery_appdetails_enabled}
                onChange={(v) => void settings.patch("library_discovery_appdetails_enabled", v)}
                label={switchCaption(s.library_discovery_appdetails_enabled)}
                disabled={
                  ls.appdetails_overridden_by_env ||
                  settings.pending === "library_discovery_appdetails_enabled"
                }
              />
            </SettingRow>
          </>
        )}
      </SettingsSection>

      <SettingsSection title="Voice" hint="Microphone capture for sessions">
        <ResourceStates loading={settings.loading} error={settings.error} />
        {!settings.loading && !settings.error && s && (
          <SettingRow
            label="Microphone capture"
            hint="Lets users talk into a game through the browser microphone. Needs a secure context (HTTPS, or localhost); otherwise the browser exposes no microphone at all, regardless of this setting."
          >
            <Switch
              id="mic-capture-enabled"
              checked={s.mic_capture_enabled}
              onChange={(v) => void settings.patch("mic_capture_enabled", v)}
              label={switchCaption(s.mic_capture_enabled)}
              disabled={settings.pending === "mic_capture_enabled"}
            />
          </SettingRow>
        )}
      </SettingsSection>

      <SettingsSection title="Images" hint="How installed app images follow the catalog">
        <ResourceStates loading={settings.loading} error={settings.error} />
        {!settings.loading && !settings.error && s && (
          <SettingRow
            label="Update policy"
            hint="After a sync, an installed image can be left alone (manual), flagged when a newer catalog version lands (notify), or updated to it automatically (auto)."
          >
            <SegmentedControl<ImageUpdatePolicy>
              aria-label="Update policy"
              value={s.image_update_policy ?? null}
              onChange={(v) => void settings.patch("image_update_policy", v)}
              disabled={settings.pending === "image_update_policy"}
              activation="manual"
              options={IMAGE_POLICY_OPTIONS}
            />
          </SettingRow>
        )}
      </SettingsSection>

      <SettingsSection
        title="Secrets"
        hint="Credentials stored encrypted; the server can use them but never shows them"
      >
        <ResourceStates loading={secrets.loading} error={secrets.errorMessage} />
        {!secrets.loading && !secrets.errorMessage && secrets.data && (() => {
          // The artwork key moved to Library > Sources — every other
          // descriptor stays here. When it was the only one declared, the card
          // keeps the master-key note and skips the "no credentials" copy,
          // which would otherwise wrongly imply nothing is configured at all.
          const otherSecrets = secrets.data.secrets.filter((s) => s.name !== ARTWORK_API_KEY_SECRET);
          return (
            <>
              {secrets.data.master_key_configured && secrets.data.key_versions.length > 0 && (
                <p className="hint mono mb3">
                  Master key configured. Decryptable key version
                  {secrets.data.key_versions.length > 1 ? "s" : ""} {secrets.data.key_versions.join(", ")}.
                </p>
              )}
              {!secrets.data.master_key_configured && (
                <div className="note warn mb4" role="status">
                  <div>
                    No master key is configured on this control plane. Credentials cannot be
                    stored here until one is set. A secret shown below as "Not configured" may
                    simply be waiting on this, not actually unset by choice. Set{" "}
                    <code>QUASAR_SECRET_KEY</code> to a base64 32-byte value (
                    <code>openssl rand -base64 32</code>) and restart;{" "}
                    <code>deploy/redeploy.sh</code> generates one automatically on deploy. Losing
                    this key makes anything already stored unrecoverable, so back it up with your
                    deployment secrets.
                  </div>
                </div>
              )}
              {otherSecrets.length === 0 ? (
                secrets.data.secrets.length === 0 && (
                  <p className="muted">This deployment does not declare any credentials yet.</p>
                )
              ) : (
                <div className="col gap5">
                  {otherSecrets.map((secret, i) => (
                    <div
                      key={secret.name}
                      style={i > 0 ? { borderTop: "1px solid var(--line)", paddingTop: "var(--s5)" } : undefined}
                    >
                      {token && (
                        <SecretField
                          secret={secret}
                          masterKeyConfigured={secrets.data!.master_key_configured}
                          token={token}
                          onChange={() => secrets.refresh()}
                        />
                      )}
                    </div>
                  ))}
                </div>
              )}
            </>
          );
        })()}
      </SettingsSection>

      <SettingsSection title="Appearance" hint="Applies to your account only">
        <SettingRow label="Theme" hint="Dark is the default">
          <SegmentedControl<Theme>
            aria-label="Theme"
            value={theme}
            onChange={setTheme}
            options={[
              { value: "dark", label: "Dark" },
              { value: "light", label: "Light" },
            ]}
          />
        </SettingRow>
        <SettingRow label="Density" hint="Row heights and paddings across the console">
          <SegmentedControl<Density>
            aria-label="Density"
            value={density}
            onChange={setDensity}
            options={[
              { value: "comfortable", label: "Comfortable" },
              { value: "dense", label: "Dense" },
            ]}
          />
        </SettingRow>
      </SettingsSection>
    </div>
  );
}
