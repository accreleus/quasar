// One device on /app/account/devices (handoff §A.22).
//
// Capability chips are derived from what the device measured at sign-in, not
// from anything it claims, which is why an unmeasured device shows none and
// says so rather than showing an optimistic default.

import { useState } from "react";
import type { Device } from "../../../api/auth";
import { Button } from "../../../components/Button";
import { Switch } from "../../../components/TextField";
import type { DeviceCapabilities } from "../../../webrtc/capability";
import { fmtDate } from "../../../lib/formatLegacy";
import { relativeTime } from "../../../lib/format/relativeTime";

/** Chips for a stored capability record; the first is highlighted. */
export function buildCapChips(
  caps: Partial<DeviceCapabilities> & { measured_at?: string },
): { label: string; highlight: boolean }[] {
  const result: { label: string; highlight: boolean }[] = [];
  if (caps.max_decode_height) {
    const height = caps.max_decode_height;
    const label = height >= 2160 ? "4K H.264" : height >= 1080 ? "1080p H.264" : `${height}p H.264`;
    result.push({ label, highlight: true });
  }
  const codecs = caps.codecs;
  if (codecs) {
    // Multi-codec spec §6.2: HEVC has been probed since P4-08 and must show.
    if (codecs.hevc) result.push({ label: "HEVC decode", highlight: false });
    if (codecs.av1) result.push({ label: "AV1 decode", highlight: false });
    if (codecs.vp9) result.push({ label: "VP9", highlight: false });
  }
  if (caps.features?.gamepad) result.push({ label: "Gamepad API", highlight: false });
  return result;
}

/** A readable label for a device the user has never named. */
export function deviceLabel(d: Device): string {
  return d.name ? d.name : `Device ${d.device_key.slice(0, 8)}`;
}

function DeviceIcon() {
  return (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <rect x="2.5" y="3.5" width="15" height="10" rx="1.5" />
      <path d="M7 17h6M10 13.5V17" strokeLinecap="round" />
    </svg>
  );
}

export interface DeviceCardProps {
  device: Device;
  /** True while this card's own request is in flight. */
  busy: boolean;
  onRename: (id: string, name: string) => Promise<void>;
  onSetTrusted: (id: string, trusted: boolean) => Promise<void>;
  onRevoke: (device: Device) => void;
}

export function DeviceCard({ device, busy, onRename, onSetTrusted, onRevoke }: DeviceCardProps) {
  const [editing, setEditing] = useState(false);
  const [nameDraft, setNameDraft] = useState(deviceLabel(device));

  const caps = device.capabilities;
  const capChips = caps ? buildCapChips(caps) : [];

  async function commitRename() {
    setEditing(false);
    const trimmed = nameDraft.trim();
    if (!trimmed || trimmed === deviceLabel(device)) return;
    await onRename(device.id, trimmed);
  }

  return (
    <div className="dev">
      <div className="row gap3 dev-head">
        <span className="dev-ico">
          <DeviceIcon />
        </span>
        <div className="grow">
          {editing ? (
            <input
              className="input"
              autoFocus
              value={nameDraft}
              onChange={(e) => setNameDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void commitRename();
                if (e.key === "Escape") {
                  setNameDraft(deviceLabel(device));
                  setEditing(false);
                }
              }}
              onBlur={() => void commitRename()}
              aria-label="Device name"
            />
          ) : (
            <div className="row gap2 center">
              <button
                type="button"
                className="dev-name"
                onClick={() => {
                  setNameDraft(deviceLabel(device));
                  setEditing(true);
                }}
                title="Rename this device"
              >
                {deviceLabel(device)}
              </button>
              {device.current && <span className="hint">this device</span>}
            </div>
          )}
          <div className="sub mono dev-key">{device.device_key}</div>
        </div>
        {device.active_session_id && (
          <span className="row gap2 dev-live">
            <span className="sdot ok" aria-hidden="true" />
            <span className="hint">streaming now</span>
          </span>
        )}
      </div>

      {capChips.length > 0 && (
        <div className="caps">
          {capChips.map((c) => (
            <span key={c.label} className={`cap${c.highlight ? " hl" : ""}`}>
              {c.label}
            </span>
          ))}
        </div>
      )}

      {/* The row reads label-first, control-right (account.css reverses the
          Switch's own order) so one labelled control carries both. */}
      <div className="dev-trust">
        <Switch
          checked={device.trusted}
          disabled={busy}
          onChange={(checked) => void onSetTrusted(device.id, checked)}
          id={`trust-${device.id}`}
          label="Trusted device"
        />
      </div>

      <div className="row dev-foot">
        <span className="hint">
          {caps?.measured_at ? `Measured ${fmtDate(caps.measured_at)}` : "Not yet measured"}
        </span>
        <span className="hint ml-auto">Last seen {relativeTime(device.last_seen_at)}</span>
        <Button variant="ghost" size="sm" disabled={busy} onClick={() => onRevoke(device)}>
          Revoke
        </Button>
      </div>
    </div>
  );
}
