// The rows of Admin ▸ Settings ▸ Input defaults (design_handoff_v3/screens/region-input-v3.html,
// frame 2). Words and the sensitivity scale match the account page's Input section.

import { useEffect, useRef, useState, type CSSProperties } from "react";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Switch } from "../../../components/TextField";
import { KEYBOARDS } from "../../../lib/region/options";
import "../../../styles/slider.css";
import { SettingRow } from "./SettingRow";
import { useInlineSave } from "./useInlineSave";
import type { UseSettingsResult } from "./useSettings";

type MatchCursor = "auto" | "on" | "off";

const MATCH_OPTIONS: { value: MatchCursor; label: string }[] = [
  { value: "auto", label: "Auto" },
  { value: "on", label: "Always on" },
  { value: "off", label: "Off" },
];

const SENS_MIN = 0.25;
const SENS_MAX = 4;
const SENS_STEP = 0.125;
const SAVE_DELAY_MS = 400;

// The slider moves in powers of two, so 1× sits in the middle of 0.25× to 4×.
const toPosition = (sensitivity: number) => Math.log2(sensitivity);
const fromPosition = (position: number) => Math.round(2 ** position * 1000) / 1000;
const showSensitivity = (v: number) => `${Number(v.toFixed(2))}×`;

const caption = (on: boolean) => (on ? "Enabled" : "Disabled");

export function InputDefaultRows({ settings }: { settings: UseSettingsResult }) {
  const { save, busy, errorFor } = useInlineSave(settings);
  const s = settings.settings;
  const layout = s?.default_keyboard?.layout ?? "us";
  const variant = s?.default_keyboard?.variant ?? "";
  const known = KEYBOARDS.find((k) => k.layout === layout);
  const layouts = known ? KEYBOARDS : [...KEYBOARDS, { layout, label: layout, variants: [] }];
  const variants = known?.variants.some(([v]) => v === variant) ? known.variants : [...(known?.variants ?? []), [variant, variant || "Default"] as const];
  const keyboardBusy = busy("default_keyboard");

  const toggle = (key: "default_raw_mouse" | "default_flip_scroll" | "default_nintendo_layout", id: string, label: string, on: boolean) => (
    <Switch
      id={id}
      aria-label={label}
      checked={on}
      onChange={(v) => void save(key, { [key]: v })}
      label={caption(on)}
      disabled={busy(key)}
    />
  );

  return (
    <>
      <SettingRow label="Keyboard layout" hint="Layout and variant">
        <div className="col gap2 admin-settings-end">
          <div className="rowflex gap2">
            <select
              className="select admin-settings-pair"
              aria-label="Default keyboard layout"
              value={layout}
              disabled={keyboardBusy}
              onChange={(e) => void save("default_keyboard", { default_keyboard: { layout: e.target.value, variant: "" } })}
            >
              {layouts.map((k) => (
                <option key={k.layout} value={k.layout}>{k.label}</option>
              ))}
            </select>
            <select
              className="select admin-settings-pair"
              aria-label="Default keyboard variant"
              value={variant}
              disabled={keyboardBusy}
              onChange={(e) => void save("default_keyboard", { default_keyboard: { layout, variant: e.target.value } })}
            >
              {variants.map(([v, name]) => (
                <option key={v} value={v}>{name}</option>
              ))}
            </select>
          </div>
          {errorFor("default_keyboard")}
        </div>
      </SettingRow>
      <SettingRow label="Match cursor to my window" hint="Auto is on for desktops, off for games">
        <div className="col gap2 admin-settings-end">
          <SegmentedControl<MatchCursor>
            aria-label="Match cursor to my window"
            value={s?.default_match_cursor ?? "auto"}
            onChange={(v) => void save("default_match_cursor", { default_match_cursor: v })}
            disabled={busy("default_match_cursor")}
            activation="manual"
            options={MATCH_OPTIONS}
          />
          {errorFor("default_match_cursor")}
        </div>
      </SettingRow>
      <SettingRow label="Mouse sensitivity" hint="0.25× to 4×">
        <div className="col gap2 admin-settings-end">
          <SensitivitySlider
            value={s?.default_mouse_sensitivity ?? 1}
            onSave={(v) => save("default_mouse_sensitivity", { default_mouse_sensitivity: v })}
          />
          {errorFor("default_mouse_sensitivity")}
        </div>
      </SettingRow>
      <SettingRow label="Raw mouse input" hint="No acceleration. Chrome and Edge only; other browsers ignore it">
        <div className="col gap2 admin-settings-end">
          {toggle("default_raw_mouse", "default-raw-mouse", "Raw mouse input by default", s?.default_raw_mouse ?? false)}
          {errorFor("default_raw_mouse")}
        </div>
      </SettingRow>
      <SettingRow label="Flip scroll direction">
        <div className="col gap2 admin-settings-end">
          {toggle("default_flip_scroll", "default-flip-scroll", "Flip scroll direction by default", s?.default_flip_scroll ?? false)}
          {errorFor("default_flip_scroll")}
        </div>
      </SettingRow>
      <SettingRow label="Gamepad Nintendo layout" hint="Swap A/B and X/Y">
        <div className="col gap2 admin-settings-end">
          {toggle("default_nintendo_layout", "default-nintendo-layout", "Gamepad Nintendo layout by default", s?.default_nintendo_layout ?? false)}
          {errorFor("default_nintendo_layout")}
        </div>
      </SettingRow>
    </>
  );
}

// Shows the slider's position at once, saves once it has rested, and keeps showing it until the save settles.
function SensitivitySlider({ value, onSave }: { value: number; onSave: (v: number) => Promise<void> }) {
  const [draft, setDraft] = useState<number | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const shown = draft ?? value;
  const min = toPosition(SENS_MIN);
  const max = toPosition(SENS_MAX);
  const move = (position: number) => {
    const next = fromPosition(position);
    setDraft(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      void onSave(next).finally(() => setDraft((d) => (d === next ? null : d)));
    }, SAVE_DELAY_MS);
  };

  return (
    <div className="rg-range admin-settings-range">
      <div className="rg-track">
        <input
          type="range"
          min={min}
          max={max}
          step={SENS_STEP}
          value={toPosition(shown)}
          style={{ "--fill": `${((toPosition(shown) - min) / (max - min)) * 100}%` } as CSSProperties}
          aria-label="Default mouse sensitivity"
          aria-valuetext={showSensitivity(shown)}
          onChange={(e) => move(Number(e.target.value))}
        />
        <div className="rg-ticks" aria-hidden="true">
          <span>0.25×</span>
          <span>1×</span>
          <span>4×</span>
        </div>
      </div>
      <span className="rg-val num" aria-hidden="true">{showSensitivity(shown)}</span>
    </div>
  );
}
