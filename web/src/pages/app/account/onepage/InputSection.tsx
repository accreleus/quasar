// The account page's Input section. It owns the one read of `/v1/me/region-input` and the one
// save path; a field is a function of the loaded settings and a `save` that sends its patch.
// The keyboard layout and variant come first, then mouse speed; raw input, flip scroll and the
// gamepad layout are switches on the same `save`.

import { useEffect, useRef, useState } from "react";
import { getRegionInput, patchRegionInput, type RegionInputWire } from "../../../../api/me";
import { ApiError } from "../../../../api/client";
import { ResourceStates } from "../../../../components/ResourceStates";
import { SegmentedControl } from "../../../../components/SegmentedControl";
import { SliderField } from "../../../../components/SliderField";
import { SelectField, Switch } from "../../../../components/TextField";
import { rawMouseSupported } from "../../../../input/capture";
import { KEYBOARDS } from "../../../../lib/region/options";
import { useResource } from "../../../../lib/resource/react";
import { MOUSE_SENSITIVITY_MAX, MOUSE_SENSITIVITY_MIN } from "../../../../input/mouseScale";
import "../../../../styles/account-input.css";

type Input = NonNullable<RegionInputWire["input"]>;
type MatchCursor = NonNullable<Input["match_cursor"]>;

const MATCH_OPTIONS: { value: MatchCursor; label: string }[] = [
  { value: "auto", label: "Auto" },
  { value: "on", label: "Always on" },
  { value: "off", label: "Off" },
];

const MATCH_HINT: Record<MatchCursor, string> = {
  auto: "Auto turns this on for desktops and off for games. On, the cursor stays under your pointer when the stream is bigger or smaller than your window.",
  on: "On for every launch, games included. Some games that lock the cursor feel slower with it on.",
  off: "Off for every launch. The stream gets your mouse movement as it is, so the cursor can run ahead of your pointer when the stream is bigger than your window.",
};

const SAVED = "Saved. Applies from your next launch.";
// A keyboard change closes an idle Steam (control-api.md §Saving ends an idle Steam).
const IDLE_STEAM_CLOSED = " Your idle Steam was closed so it picks this up.";
const BUILT_IN_KEYBOARD = { layout: "us", variant: "" };
const SENS_STEP = 0.125;
const SAVE_DELAY_MS = 400;

// The slider moves in powers of two, so 1× sits in the middle of 0.25× to 4×.
const toPosition = (sensitivity: number) => Math.log2(sensitivity);
const fromPosition = (position: number) => Math.round(2 ** position * 1000) / 1000;
const showSensitivity = (v: number) => `${Number(v.toFixed(2))}×`;

export function InputSection() {
  const res = useResource<RegionInputWire>({
    label: "your input settings",
    fetch: ({ token }) => getRegionInput(token),
  });
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);

  // One PATCH at a time: each response carries the whole resource, so a late one from an earlier save would revert a later field.
  const queue = useRef<Promise<void>>(Promise.resolve());
  const save = (patch: Partial<Input>, saved: string = SAVED): Promise<void> => {
    const run = queue.current.then(async () => {
      setStatus(null);
      try {
        const r = await res.mutate(
          ({ token }) => patchRegionInput(token, { input: patch }),
          (_, r) => r,
        );
        setStatus({ ok: true, text: r.parked_session_stopped ? saved + IDLE_STEAM_CLOSED : saved });
      } catch (e) {
        setStatus({ ok: false, text: e instanceof ApiError ? e.message : "Could not save your input settings." });
      }
    });
    queue.current = run;
    return run;
  };

  const data = res.data;
  const mine = data?.input;
  const defaults = data?.defaults?.input;

  return (
    <div className="card sec-card">
      <ResourceStates loading={res.loading} error={res.errorMessage} />
      <div className="eyebrow ac-group">Keyboard</div>
      <KeyboardFields
        mine={mine?.keyboard ?? null}
        install={defaults?.keyboard ?? BUILT_IN_KEYBOARD}
        disabled={!data}
        onSave={save}
      />
      <div className="eyebrow ac-group os-sub-h">Mouse</div>
      <MatchCursorField
        value={mine?.match_cursor ?? defaults?.match_cursor ?? "auto"}
        disabled={!data}
        onChange={(match_cursor) => void save({ match_cursor })}
      />
      <SensitivityField
        value={mine?.mouse_sensitivity ?? defaults?.mouse_sensitivity ?? 1}
        disabled={!data}
        onSave={(mouse_sensitivity) =>
          save({ mouse_sensitivity }, `Saved. Your next launch uses ${showSensitivity(mouse_sensitivity)} sensitivity.`)
        }
      />
      <RawMouseField
        checked={mine?.raw_mouse ?? defaults?.raw_mouse ?? false}
        disabled={!data}
        onChange={(raw_mouse) => void save({ raw_mouse })}
      />
      <div className="ac-sw mt3">
        <Switch
          id="flip-scroll"
          label="Flip scroll direction"
          checked={mine?.flip_scroll ?? defaults?.flip_scroll ?? false}
          onChange={(flip_scroll) => void save({ flip_scroll })}
          disabled={!data}
        />
      </div>
      <div className="eyebrow ac-group os-sub-h">Gamepad</div>
      <div className="ac-sw">
        <Switch
          id="nintendo-layout"
          label="Nintendo layout (swap A/B and X/Y)"
          checked={mine?.nintendo_layout ?? defaults?.nintendo_layout ?? false}
          onChange={(nintendo_layout) => void save({ nintendo_layout })}
          disabled={!data}
          aria-describedby="nintendo-layout-hint"
        />
      </div>
      <p className="hint mt3" id="nintendo-layout-hint">
        For Switch-style controllers: the button printed A acts as A in games.
      </p>
      <p className={status && !status.ok ? "form-error mt5" : "muted mt5"} role="status">
        {status?.text ?? "Saved as you pick."}
      </p>
    </div>
  );
}

type Keyboard = NonNullable<Input["keyboard"]>;

const layoutName = (layout: string) => KEYBOARDS.find((k) => k.layout === layout)?.label ?? layout;
// The layout's variants, plus the stored one as itself when the list does not carry it.
const variantsOf = (layout: string, variant: string): readonly (readonly [string, string])[] => {
  const listed = KEYBOARDS.find((k) => k.layout === layout)?.variants ?? [];
  return listed.some(([v]) => v === variant) ? listed : [...listed, [variant, variant || "Default"]];
};
const variantName = ({ layout, variant }: Keyboard) => variantsOf(layout, variant).find(([v]) => v === variant)?.[1] ?? variant;

// Unset shows the install's keyboard (Variant locked to it) until a layout is picked; the first
// option puts it back with `keyboard: null`. A layout change resets the variant, as the admin card does.
function KeyboardFields({ mine, install, disabled, onSave }: {
  mine: Keyboard | null;
  install: Keyboard;
  disabled: boolean;
  onSave: (patch: Partial<Input>, saved: string) => Promise<void>;
}) {
  const shown = mine ?? install;
  const layouts = !mine || KEYBOARDS.some((k) => k.layout === mine.layout) ? KEYBOARDS : [...KEYBOARDS, { layout: mine.layout, label: mine.layout }];
  const pick = (keyboard: Keyboard) =>
    void onSave({ keyboard }, `Saved. Your next launch uses ${layoutName(keyboard.layout)}, ${variantName(keyboard)}.`);
  return (
    <>
      <div className="grid g2 os-field-wide">
        <SelectField
          label="Layout"
          id="keyboard-layout"
          value={mine?.layout ?? ""}
          disabled={disabled}
          onChange={(e) =>
            e.target.value
              ? pick({ layout: e.target.value, variant: "" })
              : void onSave({ keyboard: null }, `Saved. Your next launch uses the install’s keyboard, ${layoutName(install.layout)}, ${variantName(install)}.`)
          }
        >
          <option value="">Same as the install ({layoutName(install.layout)})</option>
          {layouts.map((k) => (
            <option key={k.layout} value={k.layout}>{k.label}</option>
          ))}
        </SelectField>
        <SelectField
          label="Variant"
          id="keyboard-variant"
          value={shown.variant}
          disabled={disabled || !mine}
          onChange={(e) => pick({ layout: shown.layout, variant: e.target.value })}
        >
          {variantsOf(shown.layout, shown.variant).map(([v, name]) => (
            <option key={v} value={v}>{name}</option>
          ))}
        </SelectField>
      </div>
      <p className="hint mt3">Desktops are set to this layout at every launch, over their own keyboard setting.</p>
    </>
  );
}

function MatchCursorField({ value, disabled, onChange }: { value: MatchCursor; disabled: boolean; onChange: (v: MatchCursor) => void }) {
  return (
    <div className="field">
      <span className="label" id="match-cursor-l">Match cursor to my window</span>
      <SegmentedControl
        options={MATCH_OPTIONS}
        value={value}
        onChange={onChange}
        aria-labelledby="match-cursor-l"
        activation="manual"
        disabled={disabled}
      />
      <p className="hint">{MATCH_HINT[value]}</p>
    </div>
  );
}

// Chrome and Edge only: elsewhere the switch stays, disabled, with the reason under it (DESIGN.md "Unavailable toggle").
function RawMouseField({ checked, disabled, onChange }: { checked: boolean; disabled: boolean; onChange: (v: boolean) => void }) {
  const supported = rawMouseSupported();
  return (
    <div className="mt4">
      <div className="ac-sw">
        <Switch
          id="raw-mouse"
          label="Raw mouse input (no acceleration)"
          checked={supported && checked}
          onChange={onChange}
          disabled={disabled || !supported}
          aria-describedby="raw-mouse-hint"
        />
      </div>
      <p className="hint mt3" id="raw-mouse-hint">
        {supported
          ? "Moves the cursor exactly as your mouse does, with your system’s acceleration off. Works in Chrome and Edge."
          : "Not available in this browser. Use Chrome or Edge for raw input."}
      </p>
    </div>
  );
}

// Shows the slider's position at once, saves once it has rested, and keeps showing it until the save settles.
function SensitivityField({ value, disabled, onSave }: { value: number; disabled: boolean; onSave: (v: number) => Promise<void> }) {
  const [draft, setDraft] = useState<number | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const shown = draft ?? value;
  const move = (position: number) => {
    const next = fromPosition(position);
    setDraft(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      void onSave(next).finally(() => setDraft((d) => (d === next ? null : d)));
    }, SAVE_DELAY_MS);
  };

  return (
    <div className="os-field mt5">
      <SliderField
        label="Sensitivity"
        id="mouse-sensitivity"
        min={toPosition(MOUSE_SENSITIVITY_MIN)}
        max={toPosition(MOUSE_SENSITIVITY_MAX)}
        step={SENS_STEP}
        value={toPosition(shown)}
        onChange={move}
        valueText={showSensitivity(shown)}
        ticks={["0.25×", "1×", "4×"]}
        disabled={disabled}
      />
    </div>
  );
}
