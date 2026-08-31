// The ordered rung list, shared between the Launch tab's inline card editor
// and the "New launch profile" drawer (v3 handoff §A.16: `.rung` rows with
// rank/name/bitrate/controls, the trailing H.264 floor rung `.locked`).

import { useId, useState } from "react";
import type { CatalogCodec, StreamProfile } from "../../../api/types";
import { IconCross, IconPlus } from "../../../components/icons";
import { formatMbps, isFloorRung } from "./launchProfileHelpers";

const SHORT_CODEC_LABEL: Record<CatalogCodec, string> = {
  av1: "AV1",
  hevc: "HEVC",
  h264: "H.264",
};

function IconArrowUp() {
  return (
    <svg viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.7" aria-hidden="true">
      <path d="M2.5 7.5L6 4l3.5 3.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function IconArrowDown() {
  return (
    <svg viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.7" aria-hidden="true">
      <path d="M2.5 4.5L6 8l3.5-3.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

interface RungEditorProps {
  rungs: StreamProfile[];
  /** Stream profiles not already in `rungs` — the "Add a stream profile" candidates. */
  availableToAdd: StreamProfile[];
  onMove: (from: number, to: number) => void;
  onRemove: (index: number) => void;
  onAdd: (profileId: string) => void;
  disabled?: boolean;
}

export function RungEditor({ rungs, availableToAdd, onMove, onRemove, onAdd, disabled }: RungEditorProps) {
  const addId = useId();
  const [pendingAdd, setPendingAdd] = useState("");

  return (
    <div>
      {rungs.length === 0 ? (
        <p className="hint">
          No rungs yet. Add at least one H.264 rung. Every launch profile needs one to be startable.
        </p>
      ) : (
        rungs.map((rung, i) => {
          const locked = isFloorRung(rungs, i);
          const last = i === rungs.length - 1;
          return (
            <div className={locked ? "rung locked" : "rung"} key={rung.id}>
              <span className="rank" aria-hidden="true">{i + 1}</span>
              <span className="nm">
                {SHORT_CODEC_LABEL[rung.codec]} {rung.height}p{rung.fps}
              </span>
              <span className="br">{formatMbps(rung.nominal_bitrate_kbps)} Mb/s</span>
              <span className="ctl">
                <button
                  type="button"
                  title="Move up"
                  aria-label={`Move ${rung.display_name} up`}
                  disabled={disabled || i === 0}
                  onClick={() => onMove(i, i - 1)}
                >
                  <IconArrowUp />
                </button>
                <button
                  type="button"
                  title="Move down"
                  aria-label={`Move ${rung.display_name} down`}
                  disabled={disabled || locked || last}
                  onClick={() => onMove(i, i + 1)}
                >
                  <IconArrowDown />
                </button>
                <button
                  type="button"
                  className="rm"
                  title={locked ? "The last rung must be H.264" : "Remove rung"}
                  aria-label={`Remove ${rung.display_name}`}
                  aria-hidden={locked ? true : undefined}
                  tabIndex={locked ? -1 : undefined}
                  disabled={disabled}
                  onClick={() => onRemove(i)}
                >
                  <IconCross />
                </button>
              </span>
            </div>
          );
        })
      )}

      <div style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 12 }}>
        <select
          id={addId}
          className="select"
          style={{ flex: 1 }}
          aria-label="Add a stream profile"
          disabled={disabled || availableToAdd.length === 0}
          value={pendingAdd}
          onChange={(e) => setPendingAdd(e.target.value)}
        >
          <option value="">
            {availableToAdd.length === 0 ? "Every stream profile is already in this chain" : "Add a stream profile…"}
          </option>
          {availableToAdd.map((p) => (
            <option key={p.id} value={p.id}>
              {p.display_name} · {p.width}×{p.height} · {formatMbps(p.nominal_bitrate_kbps)} Mb/s
            </option>
          ))}
        </select>
        <button
          type="button"
          className="btn btn-sm"
          disabled={disabled || !pendingAdd}
          onClick={() => {
            if (!pendingAdd) return;
            onAdd(pendingAdd);
            setPendingAdd("");
          }}
        >
          <IconPlus />
          Add
        </button>
      </div>

      <p className="hint" style={{ marginTop: 10 }}>
        Falls through in order. The last rung must be H.264 because every browser can decode it.
      </p>
    </div>
  );
}

// Pure array-move helper so callers (the Launch tab's live-editing card and
// the New drawer's client-side draft) share one implementation of "reorder".
export function moveRung<T>(list: T[], from: number, to: number): T[] {
  if (to < 0 || to >= list.length) return list;
  const next = list.slice();
  const [entry] = next.splice(from, 1);
  next.splice(to, 0, entry);
  return next;
}
