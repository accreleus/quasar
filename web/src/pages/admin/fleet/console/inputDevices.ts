// Pure helpers shared by InputDevicesRow (the editor) and CapabilitiesRail
// (the "N reported · M passed through" summary) — kept in one place so the
// two can never disagree about which devices are currently passed through.
//
// The wire model is coarser than the mock's per-class picture: `console-
// config.md`'s `input_devices` is `"auto" | string[]` of `/dev/input/event*`
// paths, and `ConsoleCapabilities.input_devices` reports only `{path, label}`
// — no class, no hot-plug/pinned distinction. `classifyDevice` infers a class
// from the label for display only; nothing server-side keys off it.

export type DeviceClass = "Keyboard" | "Mouse" | "Controller" | "Touch" | "Tablet" | "Audio jack" | "Other";

const CLASS_KEYWORDS: [DeviceClass, RegExp][] = [
  ["Keyboard", /keyboard/i],
  ["Mouse", /mouse/i],
  ["Controller", /controller|gamepad|joystick|xbox|playstation|dualshock|dualsense/i],
  ["Tablet", /tablet|wacom|intuos/i],
  ["Touch", /touch/i],
  ["Audio jack", /audio|mic|jack|sound/i],
];

export function classifyDevice(label: string): DeviceClass {
  for (const [cls, re] of CLASS_KEYWORDS) {
    if (re.test(label)) return cls;
  }
  return "Other";
}

export type InputDevicesValue = "auto" | string[] | undefined;

/** "auto" (or unset, the API default) passes every reported device; an
 *  explicit array is exactly the paths it names. */
export function passedThroughPaths(
  value: InputDevicesValue,
  devices: { path: string; label: string }[],
): Set<string> {
  if (Array.isArray(value)) return new Set(value);
  return new Set(devices.map((d) => d.path));
}
