// The launch-options overlay (v3 handoff §B "Launch options"): three radio
// columns over the drafted selection, a verdict line, Cancel and Play now.
//
// Presentational; the draft lives in useLaunchDraft, which never commits, so
// closing the panel discards it. A row that cannot be picked is still rendered,
// disabled, carrying the reason in its sub-line and its title.

import { useCallback, useRef } from "react";
import type { RefObject } from "react";
import { Button } from "../../../components/Button";
import { IconClose, IconPlayGlyph } from "../../../components/icons";
import type { DraftCodec } from "../launchOptions";
import type { LaunchSpec, OptionColumns, OptionRow, Verdict } from "./launchOptionRules";

export interface LaunchOptionsProps {
  id: string;
  open: boolean;
  appName: string;
  columns: OptionColumns;
  /** The draft's spec, live in the head as the columns are worked. */
  spec: LaunchSpec;
  verdict: Verdict;
  launching: boolean;
  waitingForSlot: boolean;
  onSelectCodec: (codec: DraftCodec) => void;
  onSelectFps: (fps: number) => void;
  onSelectHeight: (height: number) => void;
  onCancel: () => void;
  onPlay: () => void;
  /** Focused when the panel opens, so keyboard focus enters the overlay rather
   *  than staying on the covered band. */
  closeRef?: RefObject<HTMLButtonElement>;
}

export function LaunchOptions({
  id,
  open,
  appName,
  columns,
  spec,
  verdict,
  launching,
  waitingForSlot,
  onSelectCodec,
  onSelectFps,
  onSelectHeight,
  onCancel,
  onPlay,
  closeRef,
}: LaunchOptionsProps) {
  const playLabel = waitingForSlot ? "Waiting for a slot…" : launching ? "Launching…" : "Play now";
  return (
    <div className={`lo${open ? " show" : ""}`} id={id} aria-label="Launch options">
      <div className="qp-head">
        <div>
          <div className="qp-eyebrow">Launch options</div>
          <div className="qp-game">{appName}</div>
        </div>
        <div className="qp-spec">
          <Spec label="Resolution" value={spec.resolution} />
          <Spec label="Frame rate" value={spec.fps} />
          <Spec label="Bitrate" value={spec.bitrate} />
          <Spec label="Codec" value={spec.codec} />
        </div>
        <button
          type="button"
          ref={closeRef}
          className="icon-btn d-close"
          aria-label="Close options"
          onClick={onCancel}
        >
          <IconClose />
        </button>
      </div>

      <div className="qp-cols">
        <RadioColumn
          label="Codec"
          rows={columns.codec}
          onSelect={onSelectCodec}
          sectionTitle="Only codecs this device can decode are listed."
          hints={[columns.codecHint]}
        />
        <RadioColumn
          label="Frame rate"
          rows={columns.fps}
          onSelect={onSelectFps}
          hints={columns.fpsHint ? [columns.fpsHint] : []}
        />
        <RadioColumn
          label="Resolution"
          rows={columns.resolution}
          onSelect={onSelectHeight}
          hints={columns.resolutionHint ? [columns.resolutionHint] : []}
        />
      </div>

      <div className="qp-foot">
        <div className={`qp-verdict${verdict.tone === "ok" ? "" : ` ${verdict.tone}`}`} role="status">
          <span className="dot" aria-hidden />
          <span>{verdict.text}</span>
        </div>
        <div className="qp-acts">
          <Button variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={verdict.tone === "off" || launching}
            onClick={onPlay}
          >
            <IconPlayGlyph className="ic" />
            {playLabel}
          </Button>
        </div>
      </div>
    </div>
  );
}

function Spec({ label, value }: { label: string; value: string }) {
  return (
    <span className="sp">
      <span className="l">{label}</span>
      <span className="v">{value}</span>
    </span>
  );
}

interface RadioColumnProps<V> {
  label: string;
  rows: OptionRow<V>[];
  onSelect: (value: V) => void;
  sectionTitle?: string;
  hints: string[];
}

/**
 * One `.qp-col` radiogroup. Arrows both move and select within the column,
 * skipping disabled rows (the ARIA radio-group pattern); the group is one tab
 * stop, so Tab walks the three columns rather than every row in them.
 */
function RadioColumn<V extends string | number>({
  label,
  rows,
  onSelect,
  sectionTitle,
  hints,
}: RadioColumnProps<V>) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const selectedIndex = Math.max(
    0,
    rows.findIndex((r) => r.selected),
  );

  const move = useCallback(
    (from: number, step: 1 | -1) => {
      const n = rows.length;
      for (let i = 1; i <= n; i++) {
        const at = (((from + step * i) % n) + n) % n;
        if (!rows[at].enabled) continue;
        refs.current[at]?.focus();
        onSelect(rows[at].value);
        return;
      }
    },
    [rows, onSelect],
  );

  return (
    <div className="qp-col" role="radiogroup" aria-label={label}>
      <div className="qp-section" title={sectionTitle}>
        {label}
      </div>
      {rows.map((row, i) => (
        <button
          key={String(row.value)}
          type="button"
          ref={(el) => {
            refs.current[i] = el;
          }}
          className="qp-row"
          role="radio"
          aria-checked={row.selected}
          disabled={!row.enabled}
          title={row.title}
          tabIndex={i === selectedIndex ? 0 : -1}
          onClick={() => onSelect(row.value)}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown" || e.key === "ArrowRight") {
              e.preventDefault();
              move(i, 1);
            } else if (e.key === "ArrowUp" || e.key === "ArrowLeft") {
              e.preventDefault();
              move(i, -1);
            }
          }}
        >
          <span>
            <span className="qr-label">{row.label}</span>
            {row.sub && <span className="qr-sub">{row.sub}</span>}
            {row.why && <span className="qr-why">{row.why}</span>}
          </span>
          <span className="qr-side">
            {(row.tags ?? []).map((tag) => (
              <span
                key={tag}
                className={`qp-tag${tag === "risky" ? " risky" : ""}`}
                title={tag === "risky" ? row.why : undefined}
              >
                {tag === "risky" ? "Risky" : "Recommended"}
              </span>
            ))}
            <span className="qp-check" aria-hidden>
              ✓
            </span>
          </span>
        </button>
      ))}
      {hints.map((hint) => (
        <p className="seg-hint" key={hint}>
          {hint}
        </p>
      ))}
    </div>
  );
}
