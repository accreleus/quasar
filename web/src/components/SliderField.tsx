import type { CSSProperties } from "react";
import "../styles/slider.css";

interface SliderFieldProps {
  label: string;
  id: string;
  min: number;
  max: number;
  step: number;
  value: number;
  onChange: (value: number) => void;
  /** The value as the user reads it ("1.5×"); also the screen-reader value. */
  valueText: string;
  /** Labels under the track, spread end to end (`.rg-ticks`). */
  ticks?: string[];
  disabled?: boolean;
}

/** A labelled range input in the house style. `value` is the slider's own position; the caller maps it. */
export function SliderField({ label, id, min, max, step, value, onChange, valueText, ticks, disabled }: SliderFieldProps) {
  const fill = ((value - min) / (max - min)) * 100;
  return (
    <div className="field">
      <label className="label" htmlFor={id}>{label}</label>
      <div className="rg-range">
        <div className="rg-track">
          <input
            id={id}
            type="range"
            min={min}
            max={max}
            step={step}
            value={value}
            disabled={disabled}
            style={{ "--fill": `${fill}%` } as CSSProperties}
            aria-valuetext={valueText}
            onChange={(e) => onChange(Number(e.target.value))}
          />
          {ticks && (
            <div className="rg-ticks" aria-hidden="true">
              {ticks.map((t) => <span key={t}>{t}</span>)}
            </div>
          )}
        </div>
        <span className="rg-val num" aria-hidden="true">{valueText}</span>
      </div>
    </div>
  );
}
