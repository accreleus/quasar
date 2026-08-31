// /app/account/streaming — the default launch profile (handoff §A.22).
//
// Behind a Save button, unlike the overlay page: this one changes what a launch
// asks the scheduler for, so a mis-click should not silently re-tier the next
// session.

import { useState } from "react";
import { Link } from "react-router-dom";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import { Button } from "../../../components/Button";
import { ResourceStates } from "../../../components/ResourceStates";
import { SelectField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { useResource } from "../../../lib/resource/react";
import { useSectionHead } from "../../../components/shell/sectionHead";

interface StreamPrefs {
  profiles: { id: string; name: string }[];
  /** "" = no user default, i.e. "Use recommendation". */
  defaultProfileId: string;
  globalDefaultId: string | null;
  overridesAllowed: boolean;
}

export function AccountStreaming() {
  const { addToast } = useToast();
  useSectionHead({ sub: "Used when launching from the library." });

  const res = useResource<StreamPrefs>({
    label: "your stream preferences",
    fetch: async ({ token }) => {
      const [profileRes, prefRes] = await Promise.all([
        libraryApi.getProfiles(token),
        libraryApi.getProfilePreferences(token),
      ]);
      return {
        profiles: profileRes.profiles.map((p) => ({ id: p.id, name: p.display_name })),
        defaultProfileId: prefRes.default_profile_id ?? "",
        globalDefaultId: prefRes.global_default_profile_id,
        overridesAllowed: prefRes.user_overrides_allowed,
      };
    },
  });

  // Null until the user picks, so the select tracks the stored value on its own
  // and a reload never has to be reconciled against a half-made edit.
  const [picked, setPicked] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const prefs = res.data;
  const selected = picked ?? prefs?.defaultProfileId ?? "";
  const overridesAllowed = prefs?.overridesAllowed ?? true;
  const defaultName =
    prefs?.profiles.find((p) => p.id === prefs.globalDefaultId)?.name ??
    prefs?.globalDefaultId ??
    null;

  const save = async () => {
    setSaving(true);
    setSaveError(null);
    try {
      await res.mutate(
        ({ token }) => libraryApi.updateProfilePreferences(token, selected || null),
        (data, result) => ({ ...data, defaultProfileId: result.default_profile_id ?? "" }),
      );
      setPicked(null);
      addToast({ variant: "success", title: "Stream default saved" });
    } catch (e: unknown) {
      setSaveError(e instanceof ApiError ? e.message : "Could not save stream preference.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="card sec-card">
      <ResourceStates loading={res.loading} error={res.errorMessage} />

      <div className="grid g2 ac-narrow">
        <SelectField
          label="Default profile"
          value={selected}
          onChange={(e) => setPicked(e.target.value)}
          disabled={!prefs || !overridesAllowed}
          hint={
            defaultName
              ? `Admin default: ${defaultName}`
              : "Recommendation is used when no default is set."
          }
        >
          <option value="">Use recommendation</option>
          {(prefs?.profiles ?? []).map((p) => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </SelectField>
      </div>

      {prefs && !overridesAllowed && (
        <p className="muted mt3">An admin has disabled user profile overrides.</p>
      )}

      <p className="note mt5">
        Your device is measured at sign-in, and a launch never exceeds what it can decode.
        A profile you pick here is the ceiling, not a guarantee. See{" "}
        <Link to="/app/account/devices">Devices</Link>.
      </p>

      {saveError && <p className="form-error mt4" role="alert">{saveError}</p>}

      <div className="row gap3 mt5">
        <Button
          variant="primary"
          disabled={saving || !prefs || !overridesAllowed}
          onClick={() => void save()}
        >
          {saving ? "Saving…" : "Save stream default"}
        </Button>
      </div>
    </div>
  );
}
