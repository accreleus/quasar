// Quality tab (handoff §A.10): which launch profile this app resolves through,
// and which profiles a user may pick from the menu beside Play.
//
// The allow-list is a menu filter, never the enforcement: POST /v1/sessions
// rejects a disallowed profile_id server-side (CLAUDE.md invariant #6).

import type { LaunchProfile, ProfilePolicyMode } from "../../../../api/types";
import { SegmentedControl } from "../../../../components/SegmentedControl";
import { SelectField } from "../../../../components/TextField";
import { formatMbps } from "../../streaming/launchProfileHelpers";
import { allowListApplies, pinnedLaunchProfileId } from "../launchableHelpers";
import { Section } from "./primitives";
import type { AppDraft, DraftErrors } from "./appDraft";
import { withLaunchProfile, withProfilePolicy } from "./appDraft";

const CODEC_LABEL: Record<string, string> = { h264: "H.264", hevc: "HEVC", av1: "AV1" };

interface QualityTabProps {
  draft: AppDraft;
  onChange: (draft: AppDraft) => void;
  errors: DraftErrors;
  profiles: LaunchProfile[];
}

export function QualityTab({ draft, onChange, errors, profiles }: QualityTabProps) {
  const selected = profiles.find((p) => p.id === draft.defaultProfileId) ?? null;
  const rungs = selected?.rungs.slice().sort((a, b) => a.position - b.position) ?? [];
  const top = rungs[0]?.stream_profile ?? null;
  const listApplies = allowListApplies(draft.profilePolicy);
  const pinnedId = pinnedLaunchProfileId(draft.profilePolicy, draft.defaultProfileId);

  const toggle = (id: string) =>
    onChange({
      ...draft,
      launchableIds: draft.launchableIds.includes(id)
        ? draft.launchableIds.filter((p) => p !== id)
        : [...draft.launchableIds, id],
    });

  return (
    <>
      <Section title="Quality profile" desc="How this app chooses stream quality.">
        <SelectField
          label="Source"
          value={draft.profilePolicy}
          onChange={(e) =>
            onChange(withProfilePolicy(draft, e.target.value as ProfilePolicyMode, profiles))
          }
          hint="Launch profiles are defined once under Streaming and reused globally or per app."
        >
          <option value="inherit">Use global or account default</option>
          <option value="prefer">Use an app default launch profile</option>
          <option value="force">Force an app launch profile</option>
        </SelectField>

        {draft.profilePolicy === "inherit" ? (
          <div className="note">
            <div>
              This app follows the account default, then the global default. Nothing is pinned here.
            </div>
          </div>
        ) : (
          <>
            <div>
              <SelectField
                label="App launch profile"
                value={draft.defaultProfileId}
                onChange={(e) =>
                  onChange(withLaunchProfile(draft, profiles.find((p) => p.id === e.target.value)))
                }
                hint={
                  draft.profilePolicy === "force"
                    ? "Users cannot choose a different launch profile for this app."
                    : "Used before account and global defaults."
                }
                aria-invalid={!!errors.defaultProfileId}
              >
                <option value="">Choose a launch profile</option>
                {profiles.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.display_name}
                  </option>
                ))}
              </SelectField>
              {errors.defaultProfileId && <p className="form-error">{errors.defaultProfileId}</p>}
            </div>
            {top && (
              <span className="spec-pill">
                <span className="sp">{CODEC_LABEL[top.codec] ?? top.codec}</span>
                <span className="sp">
                  {top.height}p{top.fps}
                </span>
                <span className="sp">{formatMbps(top.nominal_bitrate_kbps)} Mb/s</span>
              </span>
            )}
            {rungs.length > 0 && (
              <div className="field">
                <span className="label">Rungs, in order</span>
                <div>
                  {rungs.map((r, i) => (
                    <div className="ae-rung" key={r.stream_profile.id}>
                      <span className="num">{i + 1}</span>
                      <span>
                        {CODEC_LABEL[r.stream_profile.codec] ?? r.stream_profile.codec}{" "}
                        {r.stream_profile.height}p{r.stream_profile.fps} ·{" "}
                        {formatMbps(r.stream_profile.nominal_bitrate_kbps)} Mb/s
                      </span>
                    </div>
                  ))}
                </div>
                <span className="hint">
                  Advertised, not resolved. A launch may fall through to a lower rung.
                </span>
              </div>
            )}
          </>
        )}
      </Section>

      {/* Hidden allow-list for `force`, mirroring the server's refusal to store
          one under a pinned profile. A mirror, not the enforcement. */}
      {!listApplies ? (
        <Section
          title="Launch options"
          desc="Which launch profiles users can choose from the menu beside Play."
        >
          <div className="note">
            <div>
              This app forces <strong>{selected?.display_name ?? "one launch profile"}</strong>, so
              there is nothing for a user to choose and no allow-list can apply.
            </div>
          </div>
        </Section>
      ) : (
        <Section
          title="Launch options"
          desc="Which launch profiles users can choose from the menu beside Play. Always intersected with what their device can actually handle."
        >
          <SegmentedControl
            aria-label="Which launch profiles users can launch"
            value={draft.restrictLaunchable ? "only" : "any"}
            onChange={(v) => onChange({ ...draft, restrictLaunchable: v === "only" })}
            options={[
              { value: "any", label: "Any eligible profile" },
              { value: "only", label: "Only these" },
            ]}
          />
          {draft.restrictLaunchable && (
            <div className="field">
              <div className="ae-checks">
                {profiles.map((p) => {
                  const pinned = p.id === pinnedId;
                  return (
                    <label className="check" key={p.id}>
                      <input
                        type="checkbox"
                        checked={pinned || draft.launchableIds.includes(p.id)}
                        disabled={pinned}
                        onChange={() => toggle(p.id)}
                      />
                      {p.display_name}
                      {pinned && <span className="chip chip-accent">default</span>}
                    </label>
                  );
                })}
              </div>
              {pinnedId && (
                <span className="hint">
                  The app default is always launchable and cannot be unticked.
                </span>
              )}
              {errors.launchable && <p className="form-error">{errors.launchable}</p>}
            </div>
          )}
          <span className="hint">
            A menu filter, never the enforcement. The server rejects a disallowed profile on every
            launch regardless of what a client sends.
          </span>
        </Section>
      )}
    </>
  );
}
